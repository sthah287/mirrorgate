// Package sampling decides which eligible requests actually get mirrored.
// Mirroring every request doubles the load on the backend, so the point of
// the project is to find out how little traffic is enough to still catch
// regressions.
package sampling

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
)

const (
	Full     = "full"     // mirror every eligible request
	Random   = "random"   // mirror a fixed percentage
	Endpoint = "endpoint" // per-path rates, with Rate as the fallback
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

type Sampler struct {
	cfg Config

	// One mutex covers the draw and the counters. A gateway handling a few
	// thousand local requests a second doesn't notice it, and it keeps the
	// seeded generator used in tests safe to share.
	mu        sync.Mutex
	randFloat func() float64
	mirrored  int64
	skipped   int64
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
	default:
		return nil, fmt.Errorf("SAMPLING_MODE must be full, random, or endpoint, got %q", cfg.Mode)
	}
	return New(cfg), nil
}

func New(cfg Config) *Sampler {
	return &Sampler{cfg: cfg, randFloat: rand.Float64}
}

// NewWithRand is used by tests so a run is repeatable.
func NewWithRand(cfg Config, randFloat func() float64) *Sampler {
	return &Sampler{cfg: cfg, randFloat: randFloat}
}

// Mirror reports whether this request should be shadowed. It is called
// before any candidate work starts, so a skipped request costs nothing
// beyond the rate lookup.
func (s *Sampler) Mirror(path string) bool {
	rate := s.rateFor(path)

	s.mu.Lock()
	defer s.mu.Unlock()
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

func (s *Sampler) rateFor(path string) float64 {
	if s.cfg.Mode != Endpoint {
		return s.cfg.Rate
	}
	// First matching rule wins, so more specific patterns go first in config.
	for _, r := range s.cfg.Rules {
		if matches(r.Pattern, path) {
			return r.Rate
		}
	}
	return s.cfg.Rate
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
