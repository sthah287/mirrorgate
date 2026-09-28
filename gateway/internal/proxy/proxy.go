package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"mirrorgate/shared/event"
)

var tracer = otel.Tracer("mirrorgate/gateway")

const maxRequestBody = 1 << 20

// Headers that only apply to a single connection and shouldn't be forwarded.
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Publisher is what the gateway needs from the broker. Keeping it an
// interface means a test can make publishing fail on demand.
type Publisher interface {
	Publish(ctx context.Context, e event.Comparison) error
}

// Sampler decides whether one request gets mirrored.
type Sampler interface {
	Mirror(path string) bool
}

type Config struct {
	StableURL     string
	CandidateURL  string
	ShadowTimeout time.Duration
	MirrorMethods []string
	MaxInFlight   int
}

// Stats are counted in the gateway because the database only ever hears
// about requests that were actually mirrored. Skipped traffic has to be
// counted here or the sampling experiment has no denominator.
type Stats struct {
	Eligible        int64 `json:"eligible"`
	Mirrored        int64 `json:"mirrored"`
	SkippedInFlight int64 `json:"skipped_in_flight"`
	Published       int64 `json:"published"`
	PublishFailed   int64 `json:"publish_failed"`
}

type Proxy struct {
	stableURL     *url.URL
	candidateURL  *url.URL
	client        *http.Client
	publisher     Publisher
	sampler       Sampler
	shadowTimeout time.Duration
	mirrorMethods map[string]bool

	slots chan struct{}
	wg    sync.WaitGroup

	eligible        atomic.Int64
	mirrored        atomic.Int64
	skippedInFlight atomic.Int64
	published       atomic.Int64
	publishFailed   atomic.Int64
}

func New(cfg Config, publisher Publisher, sampler Sampler) (*Proxy, error) {
	stableURL, err := url.Parse(cfg.StableURL)
	if err != nil {
		return nil, fmt.Errorf("parse stable url: %w", err)
	}
	candidateURL, err := url.Parse(cfg.CandidateURL)
	if err != nil {
		return nil, fmt.Errorf("parse candidate url: %w", err)
	}

	methods := make(map[string]bool)
	for _, m := range cfg.MirrorMethods {
		methods[strings.ToUpper(strings.TrimSpace(m))] = true
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 100
	}
	if cfg.ShadowTimeout <= 0 {
		cfg.ShadowTimeout = 2 * time.Second
	}

	return &Proxy{
		stableURL:    stableURL,
		candidateURL: candidateURL,
		client: &http.Client{
			Timeout: 30 * time.Second,
			// otelhttp adds a span per upstream call and puts the traceparent
			// header on the request, which is what links the gateway to the
			// stable and candidate services in one trace.
			Transport: otelhttp.NewTransport(
				&http.Transport{
					MaxIdleConnsPerHost: 50,
					IdleConnTimeout:     90 * time.Second,
				},
				otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
					return r.Method + " " + r.URL.Host
				}),
			),
			// The gateway should pass redirects back to the client, not follow them.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		publisher:     publisher,
		sampler:       sampler,
		shadowTimeout: cfg.ShadowTimeout,
		mirrorMethods: methods,
		slots:         make(chan struct{}, cfg.MaxInFlight),
	}, nil
}

// response is one upstream call as the gateway saw it. The gateway no longer
// decides whether two of these match; that happens in the worker.
type response struct {
	status  int
	body    []byte
	latency time.Duration
	err     error
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The body can only be read once, so it's buffered here and each upstream
	// request gets its own reader over the same bytes.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}

	requestID := rand.Text()
	stableReq, err := newUpstreamRequest(r.Context(), r, p.stableURL, body, requestID)
	if err != nil {
		slog.Error("build stable request", "err", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	var stableDone chan response
	// Sampling is checked before startShadow so a skipped request never pays
	// for a request copy, a goroutine, or a candidate call.
	if p.mirrorMethods[r.Method] {
		p.eligible.Add(1)
		if p.sampler.Mirror(r.URL.Path) {
			stableDone = p.startShadow(r, body, requestID)
		}
	}

	stable, header := p.send(stableReq)
	if stableDone != nil {
		stableDone <- stable
	}

	if stable.err != nil {
		slog.Error("stable request failed", "request_id", requestID, "path", r.URL.Path, "err", stable.err)
		http.Error(w, "stable service unavailable", http.StatusBadGateway)
		return
	}

	for k, values := range header {
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}
	for _, h := range hopHeaders {
		w.Header().Del(h)
	}
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(stable.status)
	if _, err := w.Write(stable.body); err != nil {
		slog.Warn("write response to client", "request_id", requestID, "err", err)
	}
}

// startShadow sends a copy of the request to the candidate in a separate
// goroutine. It returns a channel the caller uses to hand over the stable
// response once it has it, or nil if the request isn't being mirrored.
func (p *Proxy) startShadow(r *http.Request, body []byte, requestID string) chan response {
	select {
	case p.slots <- struct{}{}:
	default:
		// If the candidate is slow and traffic is high, shadow goroutines
		// would pile up. Skipping the mirror is better than hurting the gateway.
		p.skippedInFlight.Add(1)
		slog.Warn("too many shadow requests in flight, skipping", "path", r.URL.Path)
		return nil
	}

	// r.Context() is canceled as soon as the handler returns, which would kill
	// the candidate request mid-flight. WithoutCancel drops the cancellation
	// but keeps the values, so the shadow request stays in the same trace.
	// Copying just the SpanContext instead looks like it should work, but the
	// resulting non-recording span hands otelhttp a noop tracer provider and
	// the candidate span silently disappears.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), p.shadowTimeout)
	candidateReq, err := newUpstreamRequest(ctx, r, p.candidateURL, body, requestID)
	if err != nil {
		cancel()
		<-p.slots
		slog.Error("build candidate request", "err", err)
		return nil
	}
	candidateReq.Header.Set("X-MirrorGate-Shadow", "true")

	p.mirrored.Add(1)
	e := event.Comparison{
		RequestID:  requestID,
		Method:     r.Method,
		Path:       r.URL.Path,
		Query:      r.URL.RawQuery,
		ReceivedAt: time.Now(),
	}

	// Buffered so the user's request never waits on this goroutine.
	stableDone := make(chan response, 1)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.slots }()
		defer cancel()

		candidate, _ := p.send(candidateReq)
		stable := <-stableDone
		if stable.err != nil {
			return
		}
		p.publish(ctx, e, stable, candidate)
	}()

	return stableDone
}

// publish hands both responses to the broker. It runs in the shadow
// goroutine, after the client already has its answer, so a slow or broken
// broker shows up as a lost comparison and never as a failed request.
func (p *Proxy) publish(ctx context.Context, e event.Comparison, stable, candidate response) {
	// WithoutCancel keeps the trace context but drops the shadow deadline, so
	// a candidate that only just made the timeout still gets published.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	ctx, span := tracer.Start(ctx, "publish comparison event")
	defer span.End()
	e.TraceID = span.SpanContext().TraceID().String()
	span.SetAttributes(attribute.String("mirrorgate.request_id", e.RequestID), attribute.String("http.route", e.Path))

	e.Stable = event.Response{
		Status:    stable.status,
		LatencyMs: milliseconds(stable.latency),
		Body:      bodyText(stable.body),
	}
	if candidate.err != nil {
		// A candidate that never answered has no status or body worth sending.
		e.Candidate = event.Response{LatencyMs: milliseconds(candidate.latency)}
		e.CandidateError = candidate.err.Error()
		if errors.Is(candidate.err, context.DeadlineExceeded) {
			e.CandidateError = fmt.Sprintf("timed out after %s", p.shadowTimeout)
		}
	} else {
		e.Candidate = event.Response{
			Status:    candidate.status,
			LatencyMs: milliseconds(candidate.latency),
			Body:      bodyText(candidate.body),
		}
	}

	if err := p.publisher.Publish(ctx, e); err != nil {
		// At-most-once: there is no retry queue yet, so an event that can't be
		// published is counted and dropped. The client was already served.
		p.publishFailed.Add(1)
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish failed")
		slog.Error("publish comparison event", "request_id", e.RequestID, "err", err)
		return
	}
	p.published.Add(1)
}

func (p *Proxy) Stats() Stats {
	return Stats{
		Eligible:        p.eligible.Load(),
		Mirrored:        p.mirrored.Load(),
		SkippedInFlight: p.skippedInFlight.Load(),
		Published:       p.published.Load(),
		PublishFailed:   p.publishFailed.Load(),
	}
}

// Wait blocks until all in-flight shadow requests are done or ctx expires.
func (p *Proxy) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Proxy) send(req *http.Request) (response, http.Header) {
	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		return response{latency: time.Since(start), err: err}, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	latency := time.Since(start)
	if err != nil {
		return response{latency: latency, err: fmt.Errorf("read body: %w", err)}, nil
	}
	return response{status: resp.StatusCode, body: body, latency: latency}, resp.Header
}

func newUpstreamRequest(ctx context.Context, r *http.Request, target *url.URL, body []byte, requestID string) (*http.Request, error) {
	u := *target
	u.Path = strings.TrimRight(target.Path, "/") + r.URL.Path
	u.RawQuery = r.URL.RawQuery

	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header = r.Header.Clone()
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	req.Header.Set("X-Request-ID", requestID)
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
			ip = prior + ", " + ip
		}
		req.Header.Set("X-Forwarded-For", ip)
	}
	return req, nil
}

// bodyText converts a response body for the event. Postgres TEXT can't hold
// invalid UTF-8 or null bytes, so binary responses are replaced by a note.
func bodyText(body []byte) string {
	if !utf8.Valid(body) || bytes.IndexByte(body, 0) != -1 {
		return fmt.Sprintf("<%d bytes of binary data>", len(body))
	}
	return string(body)
}

func milliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
