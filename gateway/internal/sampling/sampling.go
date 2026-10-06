// Package sampling decides which eligible requests actually get mirrored.
// Mirroring every request doubles the load on the backend, so the point of
// the project is to find out how little traffic is enough to still catch
// regressions.
package sampling

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	Full     = "full"     // mirror every eligible request
	Random   = "random"   // mirror a fixed percentage
	Endpoint = "endpoint" // per-path rates, with Rate as the fallback
	Risk     = "risk"     // rate from a per-endpoint risk score
)

// Risk scoring. The weights are small whole numbers on purpose, so a rate can
// be explained in one sentence: this endpoint is at 50% because it had a
// mismatch recently and it is a write.
const (
	scoreMismatch = 3 // a different response in the window
	scoreError    = 3 // a candidate error or timeout in the window
	scoreRare     = 2 // little traffic, so random sampling rarely lands on it
	scoreWrite    = 2 // POST/PUT/PATCH/DELETE, where a bug is most expensive
	scoreSlow     = 1 // a latency regression in the window

	// An endpoint counts as rare below this share of eligible traffic, once
	// enough requests have been seen for a share to mean anything. Tuned so a
	// 1-in-100 endpoint is rare and a 1-in-20 endpoint is not.
	rareShare       = 0.02
	rareMinRequests = 200
)

type Config struct {
	Mode string
	Rate float64
	// Rules are path patterns with their own rate, used in endpoint mode.
	// A trailing * matches any suffix: "/api/products/*" = 0.5.
	Rules []Rule
}

type Rule struct {
	Pattern string  `json:"pattern"`
	Rate    float64 `json:"rate"`
}

// Signal is what recent comparison history says about one endpoint. The
// gateway refreshes these from Postgres; the sampler only reads them.
type Signal struct {
	Mismatches int `json:"mismatches"`
	Errors     int `json:"errors"`
	Slow       int `json:"slow"`
}

type Sampler struct {
	cfg Config

	// One mutex covers the draw, the counters, and the risk state. A gateway
	// handling a few thousand local requests a second doesn't notice it, and
	// it keeps the seeded generator used in tests safe to share.
	mu        sync.Mutex
	randFloat func() float64
	mirrored  int64
	skipped   int64

	// Risk mode only: recent history per endpoint, and how much traffic each
	// endpoint has seen, which is how rare is decided. The traffic counts are
	// kept in two buckets and rotated, so an endpoint that was busy an hour
	// ago doesn't keep looking busy forever.
	signals      map[string]Signal
	requests     map[string]int64
	prevRequests map[string]int64
	total        int64
	prevTotal    int64
}

// Parse builds a sampler from the environment values. Bad input is an error
// rather than a silent default, because a typo in the rate would quietly
// invalidate an experiment run.
func Parse(mode, rate, rules string) (*Sampler, error) {
	cfg := Config{Mode: strings.ToLower(strings.TrimSpace(mode)), Rate: 1}
	if cfg.Mode == "" {
		cfg.Mode = Full
	}
	if rate != "" {
		r, err := strconv.ParseFloat(rate, 64)
		if err != nil || r < 0 || r > 1 {
			return nil, fmt.Errorf("SAMPLING_RATE must be between 0 and 1, got %q", rate)
		}
		cfg.Rate = r
	}

	switch cfg.Mode {
	case Full:
		cfg.Rate = 1
	case Random:
	case Endpoint:
		parsed, err := parseRules(rules)
		if err != nil {
			return nil, err
		}
		cfg.Rules = parsed
	case Risk:
		// Risk mode works out its own rate per endpoint, so a configured
		// rate would just be misleading on the dashboard.
		cfg.Rate = 0
	default:
		return nil, fmt.Errorf("SAMPLING_MODE must be full, random, endpoint, or risk, got %q", cfg.Mode)
	}
	return New(cfg), nil
}

func New(cfg Config) *Sampler {
	return NewWithRand(cfg, rand.Float64)
}

// NewWithRand is used by tests so a run is repeatable.
func NewWithRand(cfg Config, randFloat func() float64) *Sampler {
	return &Sampler{
		cfg:          cfg,
		randFloat:    randFloat,
		signals:      map[string]Signal{},
		requests:     map[string]int64{},
		prevRequests: map[string]int64{},
	}
}

// Mirror reports whether this request should be shadowed. It is called
// before any candidate work starts, so a skipped request costs nothing
// beyond the rate lookup.
func (s *Sampler) Mirror(method, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	rate := s.cfg.Rate
	switch s.cfg.Mode {
	case Endpoint:
		rate = s.endpointRate(path)
	case Risk:
		// Counted before the decision, so an endpoint share reflects the
		// traffic that arrived rather than the traffic that got mirrored.
		s.total++
		s.requests[key(method, path)]++
		rate, _ = s.riskRate(method, path)
	}

	// Handle the two certain cases without touching the generator, so a 100%
	// or 0% run is exact rather than merely very likely.
	mirror := rate >= 1 || (rate > 0 && s.randFloat() < rate)
	if mirror {
		s.mirrored++
	} else {
		s.skipped++
	}
	return mirror
}

// endpointRate returns the configured rate for a path. The first matching
// rule wins, so more specific patterns go first in config.
func (s *Sampler) endpointRate(path string) float64 {
	for _, r := range s.cfg.Rules {
		if matches(r.Pattern, path) {
			return r.Rate
		}
	}
	return s.cfg.Rate
}

// riskRate maps an endpoint score to its sampling rate.
// Callers hold the mutex.
func (s *Sampler) riskRate(method, path string) (float64, []string) {
	score, reasons := s.score(method, path)
	return rateForScore(score), reasons
}

// score adds up the risk rules for one endpoint and returns the reasons with
// it. Everything goes through here, so the score shown on the dashboard and
// the rate used for the decision can't drift apart.
// Callers hold the mutex.
func (s *Sampler) score(method, path string) (int, []string) {
	k := key(method, path)
	signal := s.signals[k]

	score := 0
	var reasons []string
	if signal.Mismatches > 0 {
		score += scoreMismatch
		reasons = append(reasons, fmt.Sprintf("+%d recent mismatch", scoreMismatch))
	}
	if signal.Errors > 0 {
		score += scoreError
		reasons = append(reasons, fmt.Sprintf("+%d recent candidate error", scoreError))
	}
	if s.isRare(k) {
		score += scoreRare
		reasons = append(reasons, fmt.Sprintf("+%d rare endpoint", scoreRare))
	}
	if isWrite(method) {
		score += scoreWrite
		reasons = append(reasons, fmt.Sprintf("+%d write request", scoreWrite))
	}
	if signal.Slow > 0 {
		score += scoreSlow
		reasons = append(reasons, fmt.Sprintf("+%d recent latency regression", scoreSlow))
	}
	return score, reasons
}

// rateForScore is the published mapping from risk score to sampling rate.
func rateForScore(score int) float64 {
	switch {
	case score >= 6:
		return 1
	case score >= 4:
		return 0.5
	case score >= 2:
		return 0.25
	default:
		return 0.1
	}
}

// isRare reports whether an endpoint gets so little traffic that random
// sampling would rarely land on it. Callers hold the mutex.
func (s *Sampler) isRare(k string) bool {
	seen, total := s.trafficFor(k)
	if total < rareMinRequests {
		return false
	}
	return float64(seen)/float64(total) < rareShare
}

// trafficFor returns how many requests an endpoint has had in the current
// window and how many requests there were in total.
// Callers hold the mutex.
func (s *Sampler) trafficFor(k string) (int64, int64) {
	return s.requests[k] + s.prevRequests[k], s.total + s.prevTotal
}

// RotateTraffic ages out the traffic counts behind the rare-endpoint rule.
// The gateway calls it once per risk window, so rare means rare lately rather
// than rare since the process started.
func (s *Sampler) RotateTraffic() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prevRequests, s.prevTotal = s.requests, s.total
	s.requests, s.total = map[string]int64{}, 0
}

func isWrite(method string) bool {
	switch strings.ToUpper(method) {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	}
	return false
}

// key is what an endpoint is scored as. The query string is deliberately not
// part of it: /api/products?category=audio and /api/products are the same
// handler, and splitting them would give every distinct query its own score.
func key(method, path string) string {
	return strings.ToUpper(method) + " " + path
}

// UpdateSignals replaces the risk state with a fresh read of recent
// comparison history. The gateway calls this on a timer.
func (s *Sampler) UpdateSignals(signals map[string]Signal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signals = signals
}

// EndpointRate is one line of the explanation behind the current rates.
type EndpointRate struct {
	Endpoint string   `json:"endpoint"`
	Requests int64    `json:"requests"`
	Share    float64  `json:"share"`
	Score    int      `json:"score"`
	Rate     float64  `json:"rate"`
	Reasons  []string `json:"reasons"`
	Signal   Signal   `json:"signal"`
}

// Explain lists what risk mode is doing per endpoint, so the dashboard can
// show why a rate is what it is. Highest rate first.
func (s *Sampler) Explain() []EndpointRate {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.Mode != Risk {
		return nil
	}

	seen := make(map[string]bool, len(s.requests)+len(s.prevRequests))
	for k := range s.requests {
		seen[k] = true
	}
	for k := range s.prevRequests {
		seen[k] = true
	}

	out := make([]EndpointRate, 0, len(seen))
	for k := range seen {
		method, path, _ := strings.Cut(k, " ")
		score, reasons := s.score(method, path)
		count, total := s.trafficFor(k)
		share := 0.0
		if total > 0 {
			share = float64(count) / float64(total)
		}
		if reasons == nil {
			reasons = []string{"no risk signals"}
		}
		out = append(out, EndpointRate{
			Endpoint: k,
			Requests: count,
			Share:    share,
			Score:    score,
			Rate:     rateForScore(score),
			Reasons:  reasons,
			Signal:   s.signals[k],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rate != out[j].Rate {
			return out[i].Rate > out[j].Rate
		}
		return out[i].Requests > out[j].Requests
	})
	return out
}

func matches(pattern, path string) bool {
	if suffix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(path, suffix)
	}
	return pattern == path
}

type Stats struct {
	Mode     string  `json:"mode"`
	Rate     float64 `json:"rate"`
	Rules    []Rule  `json:"rules,omitempty"`
	Mirrored int64   `json:"mirrored"`
	Skipped  int64   `json:"skipped"`
}

func (s *Sampler) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Mode:     s.cfg.Mode,
		Rate:     s.cfg.Rate,
		Rules:    s.cfg.Rules,
		Mirrored: s.mirrored,
		Skipped:  s.skipped,
	}
}

// parseRules reads "/api/products/*=0.5,/api/users/*=0.25,/health=0".
func parseRules(rules string) ([]Rule, error) {
	var out []Rule
	for _, part := range strings.Split(rules, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		pattern, rawRate, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("sampling rule %q should look like /api/path/*=0.25", part)
		}
		rate, err := strconv.ParseFloat(strings.TrimSpace(rawRate), 64)
		if err != nil || rate < 0 || rate > 1 {
			return nil, fmt.Errorf("sampling rule %q has a rate outside 0..1", part)
		}
		out = append(out, Rule{Pattern: strings.TrimSpace(pattern), Rate: rate})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("SAMPLING_MODE=endpoint needs SAMPLING_RULES")
	}
	return out, nil
}
