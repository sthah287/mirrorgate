package replay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Each test gets its own Postgres schema, the same way the storage tests do,
// so a run never touches the live recording tables.
//
//	TEST_DATABASE_URL=postgres://... go test ./internal/replay/
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; start the stack with docker compose up -d")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := fmt.Sprintf("replay_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	return pool
}

func recorderFor(t *testing.T, pool *pgxpool.Pool, cfg Config) *Recorder {
	t.Helper()
	if cfg.MaxURLBytes == 0 {
		cfg.MaxURLBytes = 2048
	}
	if cfg.Retention == 0 {
		cfg.Retention = time.Hour
	}
	if cfg.MaxRecords == 0 {
		cfg.MaxRecords = 100
	}
	cfg.Enabled = true
	cfg.Rate = 1
	return NewRecorder(pool, cfg, func() float64 { return 0 })
}

func get(t *testing.T, path string, headers map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func countRecorded(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM recorded_requests").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Credentials are the thing that makes keeping request copies risky, so the
// allow list is the first thing to check.
func TestSafeHeadersKeepsOnlyTheAllowList(t *testing.T) {
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("User-Agent", "k6/0.55")
	h.Set("Authorization", "Bearer secret-token")
	h.Set("Cookie", "session=abc123")
	h.Set("X-Api-Key", "key-123")
	h.Set("Proxy-Authorization", "Basic abc")
	h.Set("X-Internal-Trace", "whatever")

	got := SafeHeaders(h)
	if got["Accept"] != "application/json" || got["User-Agent"] != "k6/0.55" {
		t.Errorf("headers = %v, want Accept and User-Agent kept", got)
	}
	if len(got) != 2 {
		t.Errorf("headers = %v, want nothing outside the allow list", got)
	}
	for _, name := range []string{"Authorization", "Cookie", "X-Api-Key",
		"Proxy-Authorization", "X-Internal-Trace"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s was kept", name)
		}
	}
}

func TestRecorderOnlyKeepsReads(t *testing.T) {
	pool := testPool(t)
	rec := recorderFor(t, pool, Config{})

	rec.Observe(get(t, "/api/products/3", nil), "REQREAD")
	post := httptest.NewRequest(http.MethodPost, "/api/orders", strings.NewReader("{}"))
	rec.Observe(post, "REQWRITE")
	rec.drain()

	if n := countRecorded(t, pool); n != 1 {
		t.Fatalf("stored %d requests, want only the GET", n)
	}
	var method, path string
	if err := pool.QueryRow(context.Background(),
		"SELECT method, path FROM recorded_requests").Scan(&method, &path); err != nil {
		t.Fatal(err)
	}
	if method != "GET" || path != "/api/products/3" {
		t.Errorf("stored %s %s, want GET /api/products/3", method, path)
	}
}

// The method is constrained in the database as well, so a later change that
// started recording writes would fail loudly instead of quietly making
// replayable writes.
func TestTheTableRefusesAWrite(t *testing.T) {
	pool := testPool(t)
	_, err := pool.Exec(context.Background(),
		"INSERT INTO recorded_requests (request_id, method, path) VALUES ($1, $2, $3)",
		"REQ1", "POST", "/api/orders")
	if err == nil {
		t.Fatal("the table accepted a POST row")
	}
	if !strings.Contains(err.Error(), "constraint") {
		t.Errorf("rejected for the wrong reason: %v", err)
	}
}

func TestRecorderStopsAtTheCap(t *testing.T) {
	pool := testPool(t)
	rec := recorderFor(t, pool, Config{MaxRecords: 3})

	for i := 0; i < 7; i++ {
		rec.Observe(get(t, fmt.Sprintf("/api/products/%d", i), nil), fmt.Sprintf("REQ%d", i))
	}
	rec.drain()

	if n := countRecorded(t, pool); n != 3 {
		t.Errorf("stored %d requests, want the cap of 3", n)
	}
	if dropped := rec.Stats(context.Background()).DroppedAtCap; dropped != 4 {
		t.Errorf("counted %d dropped at the cap, want 4", dropped)
	}
}

func TestRecorderDropsRequestsPastRetention(t *testing.T) {
	pool := testPool(t)
	rec := recorderFor(t, pool, Config{Retention: time.Hour})
	ctx := context.Background()

	rec.Observe(get(t, "/api/products/1", nil), "REQFRESH")
	rec.drain()
	if _, err := pool.Exec(ctx, `
		INSERT INTO recorded_requests (request_id, path, recorded_at)
		VALUES ($1, $2, now() - interval '2 hours')`, "REQOLD", "/api/products/2"); err != nil {
		t.Fatal(err)
	}

	rec.Cleanup(ctx)

	var left string
	if err := pool.QueryRow(ctx, "SELECT request_id FROM recorded_requests").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != "REQFRESH" {
		t.Errorf("kept %s, want only the recent request", left)
	}
}

func TestRecorderSamples(t *testing.T) {
	pool := testPool(t)
	draws := []float64{0.01, 0.9, 0.02, 0.8}
	i := 0
	rec := NewRecorder(pool, Config{Enabled: true, Rate: 0.5, MaxRecords: 100,
		Retention: time.Hour, MaxURLBytes: 2048}, func() float64 {
		v := draws[i%len(draws)]
		i++
		return v
	})

	for n := 0; n < 4; n++ {
		rec.Observe(get(t, fmt.Sprintf("/api/products/%d", n), nil), fmt.Sprintf("REQ%d", n))
	}
	rec.drain()

	if n := countRecorded(t, pool); n != 2 {
		t.Errorf("stored %d of 4 requests, want the 2 that drew under the rate", n)
	}
}

// What a replay must never do: reach the stable service.
func TestReplayOnlyEverCallsTheCandidate(t *testing.T) {
	pool := testPool(t)
	var mu sync.Mutex
	var candidateHits []string
	stableHits := 0

	stable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stableHits++
		mu.Unlock()
	}))
	defer stable.Close()
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		candidateHits = append(candidateHits, r.URL.RequestURI())
		mu.Unlock()
		if r.Header.Get("X-MirrorGate-Shadow") != "true" {
			t.Errorf("replay did not mark itself as shadow traffic")
		}
		if r.Header.Get("X-MirrorGate-Replay") != "true" {
			t.Errorf("replay did not mark itself as a replay")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":3,"secret_body":"never stored"}`))
	}))
	defer candidate.Close()

	rec := recorderFor(t, pool, Config{})
	rec.Observe(get(t, "/api/products/3?include=stock", nil), "REQA")
	rec.Observe(get(t, "/api/users/1", nil), "REQB")
	rec.drain()

	runner, err := NewRunner(pool, candidate.URL, RunnerConfig{Token: "t0ken", MaxBatch: 10})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := runner.Run(context.Background(), 10)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if stableHits != 0 {
		t.Errorf("the stable service was called %d times by a replay", stableHits)
	}
	if len(candidateHits) != 2 {
		t.Fatalf("candidate saw %v, want both recorded requests", candidateHits)
	}
	if summary.Replayed != 2 || summary.Failed != 0 {
		t.Errorf("summary = %+v, want 2 replayed and none failed", summary)
	}
	if summary.StatusCounts["200"] != 2 {
		t.Errorf("status counts = %v, want two 200s", summary.StatusCounts)
	}

	// The query string has to survive, or a replay is not the same request.
	found := false
	for _, hit := range candidateHits {
		if hit == "/api/products/3?include=stock" {
			found = true
		}
	}
	if !found {
		t.Errorf("candidate saw %v, want the query string preserved", candidateHits)
	}
}

// The response of a replay is read and thrown away. Nothing that comes back
// is stored, so a replay cannot become a second copy of the data.
func TestReplayStoresOutcomesWithoutBodies(t *testing.T) {
	pool := testPool(t)
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"sensitive-body-content"}`))
	}))
	defer candidate.Close()

	rec := recorderFor(t, pool, Config{})
	rec.Observe(get(t, "/api/products/99", nil), "REQC")
	rec.drain()

	runner, err := NewRunner(pool, candidate.URL, RunnerConfig{Token: "t0ken"})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := runner.Run(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	results, err := runner.Results(context.Background(), summary.BatchID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("stored %d results, want 1", len(results))
	}
	if results[0].Status == nil || *results[0].Status != http.StatusNotFound {
		t.Errorf("status = %v, want 404 recorded as it was", results[0].Status)
	}
	if results[0].Path != "/api/products/99" {
		t.Errorf("path = %q", results[0].Path)
	}

	var columns int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'replay_results' AND column_name LIKE '%body%'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Errorf("replay_results has %d body columns, want none", columns)
	}
}

func TestReplayNeedsTheToken(t *testing.T) {
	pool := testPool(t)
	runner, err := NewRunner(pool, "http://candidate.invalid", RunnerConfig{Token: "right-token"})
	if err != nil {
		t.Fatal(err)
	}
	if runner.Authorized("") || runner.Authorized("wrong-token") {
		t.Error("the wrong token was accepted")
	}
	if !runner.Authorized("right-token") {
		t.Error("the configured token was rejected")
	}

	off, err := NewRunner(pool, "http://candidate.invalid", RunnerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if off.Enabled() || off.Authorized("") {
		t.Error("replay with no token should be off, not open")
	}
	if _, err := off.Run(context.Background(), 1); err != ErrDisabled {
		t.Errorf("run without a token returned %v, want ErrDisabled", err)
	}
}

// Two batches at once would double the load the candidate sees from a
// feature whose point is a controlled amount of extra traffic.
func TestOnlyOneReplayRunsAtATime(t *testing.T) {
	pool := testPool(t)
	release := make(chan struct{})
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer candidate.Close()

	rec := recorderFor(t, pool, Config{})
	rec.Observe(get(t, "/api/products/1", nil), "REQD")
	rec.drain()

	runner, err := NewRunner(pool, candidate.URL, RunnerConfig{Token: "t0ken", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, runErr := runner.Run(context.Background(), 1)
		done <- runErr
	}()

	// Wait for the first batch to be in the candidate request before asking
	// for a second one.
	deadline := time.Now().Add(2 * time.Second)
	for !runner.running.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := runner.Run(context.Background(), 1); err != ErrBusy {
		t.Errorf("second replay returned %v, want ErrBusy", err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first replay: %v", err)
	}
}

func TestReplayBatchIsCapped(t *testing.T) {
	pool := testPool(t)
	hits := 0
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer candidate.Close()

	rec := recorderFor(t, pool, Config{})
	for i := 0; i < 10; i++ {
		rec.Observe(get(t, fmt.Sprintf("/api/products/%d", i), nil), fmt.Sprintf("REQ%d", i))
	}
	rec.drain()

	runner, err := NewRunner(pool, candidate.URL, RunnerConfig{Token: "t0ken", MaxBatch: 4})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := runner.Run(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requested != 4 || hits != 4 {
		t.Errorf("replayed %d requests (%d hits), want the cap of 4", summary.Requested, hits)
	}
}

// A candidate that is down is a failed replay, not a lost one: the outcome
// is still recorded.
func TestFailedReplayIsRecorded(t *testing.T) {
	pool := testPool(t)
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	candidate.Close()

	rec := recorderFor(t, pool, Config{})
	rec.Observe(get(t, "/api/products/1", nil), "REQE")
	rec.drain()

	runner, err := NewRunner(pool, candidate.URL, RunnerConfig{Token: "t0ken", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := runner.Run(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 1 || summary.Replayed != 0 {
		t.Errorf("summary = %+v, want the failure counted", summary)
	}

	results, err := runner.Results(context.Background(), summary.BatchID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Error == nil {
		t.Fatalf("results = %+v, want the error stored", results)
	}
	if results[0].Status != nil {
		t.Errorf("status = %v, want none for a request that never answered", *results[0].Status)
	}
}
