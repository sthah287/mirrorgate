package relay

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"mirrorgate/shared/broker"
	"mirrorgate/shared/event"
	"mirrorgate/shared/outbox"
)

// These tests use a real Postgres, because the whole point of the outbox is
// what the database does: row locking, the unpublished scan, and the attempt
// counter. Each run gets its own schema so it never touches live data.
//
//	TEST_DATABASE_URL=postgres://mirrorgate:mirrorgate@localhost:5433/mirrorgate?sslmode=disable go test ./...
func testStore(t *testing.T) (*outbox.Store, *pgxpool.Pool) {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; start the stack with docker compose up -d")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := fmt.Sprintf("relay_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close(context.Background())
	})

	// The real schema, so this test fails if the table definition drifts.
	schemaSQL, err := os.ReadFile("../../../db/init.sql")
	if err != nil {
		t.Fatalf("read init.sql: %v", err)
	}
	if _, err := admin.Exec(ctx, "SET search_path TO "+schema+"; "+string(schemaSQL)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return outbox.New(pool), pool
}

// The relay restores a stored traceparent through the global propagator,
// which the service sets up in tracing.Init. Tests never call that, so
// without this the extraction silently does nothing and every trace id comes
// back as zeroes.
func TestMain(m *testing.M) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	os.Exit(m.Run())
}

type fakePublisher struct {
	mu       sync.Mutex
	sent     []string
	traceIDs []string
	batches  []int
	fail     error
	// ackFirst makes the publisher accept this many messages and reject the
	// rest, which is how a partial broker failure looks.
	ackFirst int
}

func (f *fakePublisher) PublishStoredBatch(ctx context.Context, msgs []broker.StoredMessage) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, len(msgs))

	if f.ackFirst > 0 {
		var acked []string
		for i, m := range msgs {
			if i >= f.ackFirst {
				break
			}
			acked = append(acked, m.RequestID)
			f.sent = append(f.sent, m.RequestID)
			f.traceIDs = append(f.traceIDs, traceIDOf(m.Traceparent))
		}
		return acked, errors.New("broker rejected the rest of the batch")
	}
	if f.fail != nil {
		return nil, f.fail
	}

	acked := make([]string, 0, len(msgs))
	for _, m := range msgs {
		acked = append(acked, m.RequestID)
		f.sent = append(f.sent, m.RequestID)
		f.traceIDs = append(f.traceIDs, traceIDOf(m.Traceparent))
	}
	return acked, nil
}

// traceIDOf pulls the trace id out of a W3C traceparent header value.
func traceIDOf(traceparent string) string {
	parts := strings.Split(traceparent, "-")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

func (f *fakePublisher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func writeEvents(t *testing.T, store *outbox.Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		e := event.Comparison{RequestID: fmt.Sprintf("REQ%03d", i), Path: "/api/products/12"}
		if err := store.Publish(context.Background(), e); err != nil {
			t.Fatalf("write outbox row: %v", err)
		}
	}
}

func pendingCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM comparison_outbox WHERE published_at IS NULL").Scan(&n)
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}
	return n
}

func TestRelayPublishesAndMarksRows(t *testing.T) {
	store, pool := testStore(t)
	writeEvents(t, store, 3)

	publisher := &fakePublisher{}
	sent, err := New(store, publisher, Config{}).drainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sent != 3 || publisher.count() != 3 {
		t.Errorf("published %d, publisher saw %d, want 3 each", sent, publisher.count())
	}
	if left := pendingCount(t, pool); left != 0 {
		t.Errorf("%d rows still pending, want 0", left)
	}
}

// A broker failure must leave the rows alone, or the event is lost. This is
// the behaviour the whole feature exists for.
func TestRelayKeepsRowsWhenPublishingFails(t *testing.T) {
	store, pool := testStore(t)
	writeEvents(t, store, 3)

	publisher := &fakePublisher{fail: errors.New("broker is down")}
	relay := New(store, publisher, Config{})

	if _, err := relay.drainOnce(context.Background()); err == nil {
		t.Fatal("expected the drain to report the publish failure")
	}
	if left := pendingCount(t, pool); left != 3 {
		t.Errorf("%d rows pending, want all 3 kept for a retry", left)
	}

	var attempts int
	var lastError *string
	err := pool.QueryRow(context.Background(),
		"SELECT attempts, last_error FROM comparison_outbox WHERE request_id = 'REQ000'").Scan(&attempts, &lastError)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if lastError == nil || *lastError != "broker is down" {
		t.Errorf("last_error = %v, want the publish failure recorded", lastError)
	}

	// And once the broker comes back, the same rows go out.
	publisher.fail = nil
	sent, err := relay.drainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain after recovery: %v", err)
	}
	if sent != 3 {
		t.Errorf("published %d after recovery, want 3", sent)
	}
	if left := pendingCount(t, pool); left != 0 {
		t.Errorf("%d rows still pending after recovery, want 0", left)
	}
}

func TestRelayRespectsBatchSize(t *testing.T) {
	store, pool := testStore(t)
	writeEvents(t, store, 5)

	publisher := &fakePublisher{}
	sent, err := New(store, publisher, Config{BatchSize: 2}).drainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sent != 2 {
		t.Errorf("published %d in one batch, want 2", sent)
	}
	if left := pendingCount(t, pool); left != 3 {
		t.Errorf("%d rows pending, want 3", left)
	}
}

// The relay publishes long after the request is gone, so the only way the
// message can stay in the original trace is the traceparent stored with it.
func TestRelayRestoresTheStoredTrace(t *testing.T) {
	store, pool := testStore(t)
	writeEvents(t, store, 1)

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	_, err := pool.Exec(context.Background(),
		"UPDATE comparison_outbox SET traceparent = $1",
		"00-"+traceID+"-00f067aa0ba902b7-01")
	if err != nil {
		t.Fatal(err)
	}

	publisher := &fakePublisher{}
	if _, err := New(store, publisher, Config{}).drainOnce(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(publisher.traceIDs) != 1 || publisher.traceIDs[0] != traceID {
		t.Errorf("published with trace %v, want the stored %s", publisher.traceIDs, traceID)
	}
}

// Writing the same request id twice must not queue it twice.
func TestOutboxIgnoresDuplicateRequestIDs(t *testing.T) {
	store, pool := testStore(t)
	writeEvents(t, store, 1)
	writeEvents(t, store, 1)

	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM comparison_outbox").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d rows, want 1", n)
	}
}

// The whole point of the change: one publish call per batch, not one per row.
func TestRelayPublishesTheBatchInOneCall(t *testing.T) {
	store, _ := testStore(t)
	writeEvents(t, store, 25)

	publisher := &fakePublisher{}
	sent, err := New(store, publisher, Config{BatchSize: 100}).drainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sent != 25 {
		t.Errorf("published %d, want 25", sent)
	}
	if len(publisher.batches) != 1 || publisher.batches[0] != 25 {
		t.Errorf("publisher saw batches %v, want one call carrying 25", publisher.batches)
	}
}

// A broker that takes some of a batch and rejects the rest must leave exactly
// the rejected rows pending. Marking an unacknowledged event as published
// would lose it.
func TestPartialBatchFailureOnlyMarksAcknowledged(t *testing.T) {
	store, pool := testStore(t)
	writeEvents(t, store, 10)

	publisher := &fakePublisher{ackFirst: 4}
	sent, err := New(store, publisher, Config{BatchSize: 10}).drainOnce(context.Background())
	if err == nil {
		t.Fatal("expected the partial failure to be reported")
	}
	if sent != 4 {
		t.Errorf("reported %d published, want 4", sent)
	}

	var published, pending int
	row := pool.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE published_at IS NOT NULL),
		       count(*) FILTER (WHERE published_at IS NULL)
		FROM comparison_outbox`)
	if err := row.Scan(&published, &pending); err != nil {
		t.Fatal(err)
	}
	if published != 4 || pending != 6 {
		t.Errorf("published %d pending %d, want 4 and 6", published, pending)
	}

	// And the six that were refused go out on the next pass.
	publisher.ackFirst = 0
	sent, err = New(store, publisher, Config{BatchSize: 10}).drainOnce(context.Background())
	if err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if sent != 6 {
		t.Errorf("second drain published %d, want the 6 that were left", sent)
	}
	if left := pendingCount(t, pool); left != 0 {
		t.Errorf("%d rows still pending", left)
	}
}

// When the broker fails without saying which messages it took, nothing may be
// marked: the batch is retried and the worker drops the duplicates.
func TestUnknownBatchOutcomeMarksNothing(t *testing.T) {
	store, pool := testStore(t)
	writeEvents(t, store, 5)

	publisher := &fakePublisher{fail: errors.New("connection reset")}
	if _, err := New(store, publisher, Config{}).drainOnce(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if left := pendingCount(t, pool); left != 5 {
		t.Errorf("%d rows pending, want all 5 kept", left)
	}
}

// Each row in a batch keeps its own trace, rather than all of them inheriting
// whatever the relay happened to be doing.
func TestBatchKeepsPerRowTraces(t *testing.T) {
	store, pool := testStore(t)
	writeEvents(t, store, 2)

	first := "4bf92f3577b34da6a3ce929d0e0e4736"
	second := "0af7651916cd43dd8448eb211c80319c"
	for id, trace := range map[string]string{"REQ000": first, "REQ001": second} {
		if _, err := pool.Exec(context.Background(),
			"UPDATE comparison_outbox SET traceparent = $2 WHERE request_id = $1",
			id, "00-"+trace+"-00f067aa0ba902b7-01"); err != nil {
			t.Fatal(err)
		}
	}

	publisher := &fakePublisher{}
	if _, err := New(store, publisher, Config{}).drainOnce(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	got := map[string]bool{}
	for _, id := range publisher.traceIDs {
		got[id] = true
	}
	if !got[first] || !got[second] {
		t.Errorf("published traces %v, want both %s and %s", publisher.traceIDs, first, second)
	}
}
