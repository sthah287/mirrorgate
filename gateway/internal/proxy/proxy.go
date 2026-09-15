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
	"time"
	"unicode/utf8"

	"mirrorgate/gateway/internal/compare"
	"mirrorgate/gateway/internal/storage"
)

const maxRequestBody = 1 << 20

// Headers that only apply to a single connection and shouldn't be forwarded.
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

type Store interface {
	SaveComparison(ctx context.Context, c storage.Comparison) error
}

type Config struct {
	StableURL     string
	CandidateURL  string
	ShadowTimeout time.Duration
	MirrorMethods []string
	MaxInFlight   int
}

type Proxy struct {
	stableURL     *url.URL
	candidateURL  *url.URL
	client        *http.Client
	store         Store
	shadowTimeout time.Duration
	mirrorMethods map[string]bool

	slots chan struct{}
	wg    sync.WaitGroup
}

func New(cfg Config, store Store) (*Proxy, error) {
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
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 50,
				IdleConnTimeout:     90 * time.Second,
			},
			// The gateway should pass redirects back to the client, not follow them.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		store:         store,
		shadowTimeout: cfg.ShadowTimeout,
		mirrorMethods: methods,
		slots:         make(chan struct{}, cfg.MaxInFlight),
	}, nil
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

	var stableDone chan compare.Response
	if p.mirrorMethods[r.Method] {
		stableDone = p.startShadow(r, body, requestID)
	}

	stable, header := p.send(stableReq)
	if stableDone != nil {
		stableDone <- stable
	}

	if stable.Err != nil {
		slog.Error("stable request failed", "request_id", requestID, "path", r.URL.Path, "err", stable.Err)
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
	w.WriteHeader(stable.Status)
	if _, err := w.Write(stable.Body); err != nil {
		slog.Warn("write response to client", "request_id", requestID, "err", err)
	}
}

// startShadow sends a copy of the request to the candidate in a separate
// goroutine. It returns a channel the caller uses to hand over the stable
// response once it has it, or nil if the request isn't being mirrored.
func (p *Proxy) startShadow(r *http.Request, body []byte, requestID string) chan compare.Response {
	select {
	case p.slots <- struct{}{}:
	default:
		// If the candidate is slow and traffic is high, shadow goroutines
		// would pile up. Skipping the mirror is better than hurting the gateway.
		slog.Warn("too many shadow requests in flight, skipping", "path", r.URL.Path)
		return nil
	}

	// This can't use r.Context(). That context is canceled as soon as the
	// handler returns, which would kill the candidate request mid-flight.
	ctx, cancel := context.WithTimeout(context.Background(), p.shadowTimeout)
	candidateReq, err := newUpstreamRequest(ctx, r, p.candidateURL, body, requestID)
	if err != nil {
		cancel()
		<-p.slots
		slog.Error("build candidate request", "err", err)
		return nil
	}
	candidateReq.Header.Set("X-MirrorGate-Shadow", "true")

	comparison := storage.Comparison{
		RequestID:  requestID,
		Method:     r.Method,
		Path:       r.URL.Path,
		Query:      r.URL.RawQuery,
		ReceivedAt: time.Now(),
	}

	// Buffered so the user's request never waits on this goroutine.
	stableDone := make(chan compare.Response, 1)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.slots }()
		defer cancel()

		candidate, _ := p.send(candidateReq)
		stable := <-stableDone
		if stable.Err != nil {
			return
		}
		p.record(comparison, stable, candidate)
	}()

	return stableDone
}

func (p *Proxy) record(c storage.Comparison, stable, candidate compare.Response) {
	result := compare.Compare(stable, candidate)

	c.StableStatus = stable.Status
	c.StableLatencyMs = milliseconds(stable.Latency)
	c.CandidateLatencyMs = milliseconds(candidate.Latency)
	c.StableBody = bodyText(stable.Body)
	c.CandidateBody = bodyText(candidate.Body)
	c.StatusMatch = result.StatusMatch
	c.BodyMatch = result.BodyMatch
	c.CandidateSlow = result.CandidateSlow
	c.Outcome = result.Outcome
	c.Differences = result.Differences

	if candidate.Err != nil {
		c.CandidateError = candidate.Err.Error()
		if errors.Is(candidate.Err, context.DeadlineExceeded) {
			c.CandidateError = fmt.Sprintf("timed out after %s", p.shadowTimeout)
		}
	} else {
		status := candidate.Status
		c.CandidateStatus = &status
	}

	if c.Outcome != compare.Match {
		slog.Warn("candidate mismatch", "request_id", c.RequestID, "path", c.Path,
			"outcome", c.Outcome, "differences", c.Differences, "candidate_error", c.CandidateError)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.store.SaveComparison(ctx, c); err != nil {
		slog.Error("save comparison", "request_id", c.RequestID, "err", err)
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

func (p *Proxy) send(req *http.Request) (compare.Response, http.Header) {
	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		return compare.Response{Latency: time.Since(start), Err: err}, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	latency := time.Since(start)
	if err != nil {
		return compare.Response{Latency: latency, Err: fmt.Errorf("read body: %w", err)}, nil
	}
	return compare.Response{Status: resp.StatusCode, Body: body, Latency: latency}, resp.Header
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

// bodyText converts a response body for storage. Postgres TEXT can't hold
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
