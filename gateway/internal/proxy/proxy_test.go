package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mirrorgate/shared/event"
)

// The gateway no longer decides whether responses match, so these tests
// check what it publishes. Classification is tested in the worker.
type fakePublisher struct {
	events chan event.Comparison
	fail   error
}

func newFakePublisher() *fakePublisher {
	return &fakePublisher{events: make(chan event.Comparison, 10)}
}

func (f *fakePublisher) Publish(ctx context.Context, e event.Comparison) error {
	if f.fail != nil {
		return f.fail
	}
	f.events <- e
	return nil
}

func (f *fakePublisher) next(t *testing.T) event.Comparison {
	t.Helper()
	select {
	case e := <-f.events:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("no comparison event was published")
		return event.Comparison{}
	}
}

// mirrorAll and mirrorNone stand in for the real sampler, which has its own
// tests in the sampling package.
type fixedSampler bool

func (f fixedSampler) Mirror(string) bool { return bool(f) }

const (
	mirrorAll  = fixedSampler(true)
	mirrorNone = fixedSampler(false)
)

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

func newTestProxy(t *testing.T, stableURL, candidateURL string, publisher Publisher, cfg Config) *Proxy {
	t.Helper()
	return newTestProxyWithSampler(t, stableURL, candidateURL, publisher, mirrorAll, cfg)
}

func newTestProxyWithSampler(t *testing.T, stableURL, candidateURL string, publisher Publisher, sampler Sampler, cfg Config) *Proxy {
	t.Helper()
	cfg.StableURL = stableURL
	cfg.CandidateURL = candidateURL
	if cfg.MirrorMethods == nil {
		cfg.MirrorMethods = []string{"GET"}
	}
	p, err := New(cfg, publisher, sampler)
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

	publisher := newFakePublisher()
	p := newTestProxy(t, stable.URL, candidate.URL, publisher, Config{})

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

	e := publisher.next(t)
	if e.RequestID != rec.Header().Get("X-Request-ID") {
		t.Error("published request id does not match the one returned to the client")
	}
	if e.Method != http.MethodGet || e.Path != "/api/products/1" || e.Query != "currency=usd" {
		t.Errorf("event request = %s %s?%s", e.Method, e.Path, e.Query)
	}
	if e.Stable.Status != 200 || e.Candidate.Status != 200 {
		t.Errorf("statuses = %d / %d", e.Stable.Status, e.Candidate.Status)
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

	publisher := newFakePublisher()
	p := newTestProxy(t, stable.URL, candidate.URL, publisher, Config{})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/products/12", nil))

	if strings.Contains(rec.Body.String(), "59.99") {
		t.Fatalf("client received candidate body: %s", rec.Body.String())
	}

	e := publisher.next(t)
	if !strings.Contains(e.Stable.Body, "49.99") || !strings.Contains(e.Candidate.Body, "59.99") {
		t.Errorf("event bodies = %q / %q", e.Stable.Body, e.Candidate.Body)
	}
}

func TestSlowCandidateDoesNotDelayClient(t *testing.T) {
	stable := jsonServer(http.StatusOK, `{"ok":true}`, 0)
	defer stable.Close()
	candidate := jsonServer(http.StatusOK, `{"ok":true}`, 5*time.Second)
	defer candidate.Close()

	publisher := newFakePublisher()
	p := newTestProxy(t, stable.URL, candidate.URL, publisher, Config{ShadowTimeout: 300 * time.Millisecond})

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

	e := publisher.next(t)
	if !strings.Contains(e.CandidateError, "timed out") {
		t.Errorf("candidate error = %q", e.CandidateError)
	}
	if e.Candidate.Status != 0 {
		t.Errorf("candidate status = %d, want 0 after a timeout", e.Candidate.Status)
	}
}

func TestCandidateDownDoesNotBreakClient(t *testing.T) {
	stable := jsonServer(http.StatusOK, `{"ok":true}`, 0)
	defer stable.Close()
	candidate := jsonServer(http.StatusOK, `{"ok":true}`, 0)
	candidate.Close()

	publisher := newFakePublisher()
	p := newTestProxy(t, stable.URL, candidate.URL, publisher, Config{})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/products", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	e := publisher.next(t)
	if e.CandidateError == "" {
		t.Error("expected the connection failure to be recorded on the event")
	}
}

// A broker outage has to look like a lost comparison, not a failed request.
func TestBrokerFailureDoesNotAffectClient(t *testing.T) {
	stable := jsonServer(http.StatusOK, `{"id":1}`, 0)
	defer stable.Close()
	candidate := jsonServer(http.StatusOK, `{"id":1}`, 0)
	defer candidate.Close()

	publisher := newFakePublisher()
	publisher.fail = errors.New("kafka: no brokers available")
	p := newTestProxy(t, stable.URL, candidate.URL, publisher, Config{})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/products/1", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"id":1}` {
		t.Fatalf("client got %d %q, want the stable response", rec.Code, rec.Body.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	st := p.Stats()
	if st.PublishFailed != 1 || st.Published != 0 {
		t.Errorf("stats = %+v, want the failure counted and nothing published", st)
	}
	if st.Mirrored != 1 {
		t.Errorf("mirrored = %d, want 1", st.Mirrored)
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

	publisher := newFakePublisher()
	p := newTestProxy(t, stable.URL, candidate.URL, publisher, Config{MirrorMethods: []string{"GET", "POST"}})

	payload := `{"items":[1,2,3]}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cart/quote", strings.NewReader(payload)))
	publisher.next(t)

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

	p := newTestProxy(t, stable.URL, candidate.URL, newFakePublisher(), Config{MirrorMethods: []string{"GET"}})

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
	if candidateHits.Load() != 0 {
		t.Errorf("candidate received %d requests, want 0", candidateHits.Load())
	}
	if st := p.Stats(); st.Eligible != 0 || st.Mirrored != 0 {
		t.Errorf("stats = %+v, want nothing counted as eligible", st)
	}
}

// A skipped request should not reach the candidate at all, which is the
// whole point of sampling: less load, not just fewer stored rows.
func TestSkippedRequestNeverReachesCandidate(t *testing.T) {
	stable := jsonServer(http.StatusOK, `{"id":1}`, 0)
	defer stable.Close()

	var candidateHits atomic.Int32
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		candidateHits.Add(1)
	}))
	defer candidate.Close()

	publisher := newFakePublisher()
	p := newTestProxyWithSampler(t, stable.URL, candidate.URL, publisher, mirrorNone, Config{})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/products/1", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"id":1}` {
		t.Fatalf("client got %d %q, want the stable response", rec.Code, rec.Body.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if candidateHits.Load() != 0 {
		t.Errorf("candidate received %d requests, want 0", candidateHits.Load())
	}
	st := p.Stats()
	if st.Eligible != 1 || st.Mirrored != 0 || st.Published != 0 {
		t.Errorf("stats = %+v, want eligible=1 with nothing mirrored", st)
	}
}
