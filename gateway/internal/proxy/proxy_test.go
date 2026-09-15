package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mirrorgate/gateway/internal/compare"
	"mirrorgate/gateway/internal/storage"
)

type fakeStore struct {
	saved chan storage.Comparison
}

func newFakeStore() *fakeStore {
	return &fakeStore{saved: make(chan storage.Comparison, 10)}
}

func (f *fakeStore) SaveComparison(ctx context.Context, c storage.Comparison) error {
	f.saved <- c
	return nil
}

func (f *fakeStore) next(t *testing.T) storage.Comparison {
	t.Helper()
	select {
	case c := <-f.saved:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("no comparison was saved")
		return storage.Comparison{}
	}
}

func jsonServer(status int, body string, delay time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
}

func newTestProxy(t *testing.T, stableURL, candidateURL string, store Store, cfg Config) *Proxy {
	t.Helper()
	cfg.StableURL = stableURL
	cfg.CandidateURL = candidateURL
	if cfg.MirrorMethods == nil {
		cfg.MirrorMethods = []string{"GET"}
	}
	p, err := New(cfg, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestStableResponseIsReturned(t *testing.T) {
	stable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/products/1" || r.URL.RawQuery != "currency=usd" {
			t.Errorf("stable got %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("X-Served-By", "stable")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":1,"name":"USB-C Charger"}`)
	}))
	defer stable.Close()

	var shadowHeader atomic.Value
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shadowHeader.Store(r.Header.Get("X-MirrorGate-Shadow"))
		io.WriteString(w, `{"name":"USB-C Charger","id":1}`)
	}))
	defer candidate.Close()

	store := newFakeStore()
	p := newTestProxy(t, stable.URL, candidate.URL, store, Config{})

	req := httptest.NewRequest(http.MethodGet, "/api/products/1?currency=usd", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"id":1,"name":"USB-C Charger"}` {
		t.Errorf("body = %s", got)
	}
	if rec.Header().Get("X-Served-By") != "stable" {
		t.Error("stable response header was not copied")
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("missing X-Request-ID")
	}

	c := store.next(t)
	if c.Outcome != compare.Match {
		t.Errorf("outcome = %s, want match (differences: %v)", c.Outcome, c.Differences)
	}
	if c.RequestID != rec.Header().Get("X-Request-ID") {
		t.Error("saved request id does not match the one returned to the client")
	}
	if shadowHeader.Load() != "true" {
		t.Error("candidate request was not marked as shadow traffic")
	}
}

func TestCandidateDifferenceDoesNotReachClient(t *testing.T) {
	stable := jsonServer(http.StatusOK, `{"id":12,"name":"Wireless Keyboard","price":49.99}`, 0)
	defer stable.Close()
	candidate := jsonServer(http.StatusOK, `{"id":12,"name":"Wireless Keyboard","price":59.99}`, 0)
	defer candidate.Close()

	store := newFakeStore()
	p := newTestProxy(t, stable.URL, candidate.URL, store, Config{})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/products/12", nil))

	if strings.Contains(rec.Body.String(), "59.99") {
		t.Fatalf("client received candidate body: %s", rec.Body.String())
	}

	c := store.next(t)
	if c.Outcome != compare.Different || c.BodyMatch {
		t.Errorf("outcome = %s body_match = %v, want different/false", c.Outcome, c.BodyMatch)
	}
	if len(c.Differences) != 1 || c.Differences[0] != "price: 49.99 != 59.99" {
		t.Errorf("differences = %v", c.Differences)
	}
}

func TestSlowCandidateDoesNotDelayClient(t *testing.T) {
	stable := jsonServer(http.StatusOK, `{"ok":true}`, 0)
	defer stable.Close()
	candidate := jsonServer(http.StatusOK, `{"ok":true}`, 5*time.Second)
	defer candidate.Close()

	store := newFakeStore()
	p := newTestProxy(t, stable.URL, candidate.URL, store, Config{ShadowTimeout: 300 * time.Millisecond})

	start := time.Now()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/users/7", nil))
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The client should get its answer well before the shadow timeout fires.
	if elapsed > 150*time.Millisecond {
		t.Errorf("client waited %s, candidate is slowing down the real request", elapsed)
	}

	c := store.next(t)
	if c.Outcome != compare.Error {
		t.Errorf("outcome = %s, want error", c.Outcome)
	}
	if c.CandidateStatus != nil {
		t.Errorf("candidate status = %d, want nil after timeout", *c.CandidateStatus)
	}
	if !strings.Contains(c.CandidateError, "timed out") {
		t.Errorf("candidate error = %q", c.CandidateError)
	}
}

func TestCandidateDownDoesNotBreakClient(t *testing.T) {
	stable := jsonServer(http.StatusOK, `{"ok":true}`, 0)
	defer stable.Close()
	candidate := jsonServer(http.StatusOK, `{"ok":true}`, 0)
	candidate.Close()

	store := newFakeStore()
	p := newTestProxy(t, stable.URL, candidate.URL, store, Config{})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/products", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	c := store.next(t)
	if c.Outcome != compare.Error || c.CandidateError == "" {
		t.Errorf("outcome = %s error = %q, want an error to be recorded", c.Outcome, c.CandidateError)
	}
}

func TestRequestBodyIsSentToBothServices(t *testing.T) {
	var stableBody, candidateBody atomic.Value
	stable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		stableBody.Store(string(b))
		io.WriteString(w, `{"total":3}`)
	}))
	defer stable.Close()
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		candidateBody.Store(string(b))
		io.WriteString(w, `{"total":3}`)
	}))
	defer candidate.Close()

	store := newFakeStore()
	p := newTestProxy(t, stable.URL, candidate.URL, store, Config{MirrorMethods: []string{"GET", "POST"}})

	payload := `{"items":[1,2,3]}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cart/quote", strings.NewReader(payload)))
	store.next(t)

	if stableBody.Load() != payload {
		t.Errorf("stable body = %v", stableBody.Load())
	}
	if candidateBody.Load() != payload {
		t.Errorf("candidate body = %v", candidateBody.Load())
	}
}

func TestMethodsNotInMirrorListAreNotMirrored(t *testing.T) {
	stable := jsonServer(http.StatusCreated, `{"id":99}`, 0)
	defer stable.Close()

	var candidateHits atomic.Int32
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		candidateHits.Add(1)
	}))
	defer candidate.Close()

	p := newTestProxy(t, stable.URL, candidate.URL, newFakeStore(), Config{MirrorMethods: []string{"GET"}})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/orders", strings.NewReader(`{}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if n := candidateHits.Load(); n != 0 {
		t.Errorf("candidate received %d requests, want 0", n)
	}
}
