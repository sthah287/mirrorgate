package sampling

import "testing"

// riskSampler returns a sampler in risk mode whose generator always draws 0,
// so Mirror is true whenever the rate is above zero. These tests are about
// which rate an endpoint gets, not about the draw.
func riskSampler() *Sampler {
	return NewWithRand(Config{Mode: Risk}, func() float64 { return 0 })
}

// rateFor runs the scoring the same way Mirror does, without the draw.
func rateFor(s *Sampler, method, path string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	rate, _ := s.riskRate(method, path)
	return rate
}

// Everything starts at the floor. An endpoint nobody has complained about
// gets the base rate, which is what keeps risk mode cheap.
func TestNormalEndpointGetsTheBaseRate(t *testing.T) {
	s := riskSampler()
	if got := rateFor(s, "GET", "/api/products/1"); got != 0.1 {
		t.Errorf("rate = %.2f, want 0.10 for an endpoint with no signals", got)
	}
}

func TestRecentMismatchRaisesTheRate(t *testing.T) {
	s := riskSampler()
	s.UpdateSignals(map[string]Signal{"GET /api/products/12": {Mismatches: 4}})

	if got := rateFor(s, "GET", "/api/products/12"); got != 0.25 {
		t.Errorf("rate = %.2f, want 0.25 after a mismatch (score 3)", got)
	}
	// The endpoint next door is unaffected.
	if got := rateFor(s, "GET", "/api/products/1"); got != 0.1 {
		t.Errorf("rate = %.2f, want the quiet endpoint left at 0.10", got)
	}
}

func TestRecentCandidateErrorRaisesTheRate(t *testing.T) {
	s := riskSampler()
	s.UpdateSignals(map[string]Signal{"GET /api/products/8": {Errors: 2}})

	if got := rateFor(s, "GET", "/api/products/8"); got != 0.25 {
		t.Errorf("rate = %.2f, want 0.25 after a candidate error (score 3)", got)
	}
}

func TestLatencyRegressionRaisesTheRate(t *testing.T) {
	s := riskSampler()
	// A slow endpoint on its own is only +1, which is still the base rate.
	s.UpdateSignals(map[string]Signal{"GET /api/products": {Slow: 9}})
	if got := rateFor(s, "GET", "/api/products"); got != 0.1 {
		t.Errorf("rate = %.2f, a slow signal alone is score 1 and stays at 0.10", got)
	}

	// Combined with a mismatch it is score 4, which is the 50% band.
	s.UpdateSignals(map[string]Signal{"GET /api/products": {Slow: 9, Mismatches: 1}})
	if got := rateFor(s, "GET", "/api/products"); got != 0.5 {
		t.Errorf("rate = %.2f, want 0.50 for mismatch plus slow (score 4)", got)
	}
}

// Writes are riskier than reads, so they start above the floor even with a
// clean history.
func TestWriteRequestsGetElevatedPriority(t *testing.T) {
	s := riskSampler()
	if got := rateFor(s, "POST", "/api/orders"); got != 0.25 {
		t.Errorf("POST rate = %.2f, want 0.25 (score 2) with no other signals", got)
	}
	if got := rateFor(s, "GET", "/api/orders"); got != 0.1 {
		t.Errorf("GET rate = %.2f, want the same path as a read to stay at 0.10", got)
	}
	for _, method := range []string{"PUT", "PATCH", "DELETE"} {
		if got := rateFor(s, method, "/api/orders"); got != 0.25 {
			t.Errorf("%s rate = %.2f, want 0.25", method, got)
		}
	}
}

// An endpoint that sees very little traffic is prioritised, because random
// sampling would rarely land on it.
func TestRareEndpointGetsElevatedPriority(t *testing.T) {
	s := riskSampler()

	// 300 requests: 297 to a busy path, 3 to a quiet one (1%).
	for i := 0; i < 297; i++ {
		s.Mirror("GET", "/api/products/1")
	}
	for i := 0; i < 3; i++ {
		s.Mirror("GET", "/api/reports/monthly")
	}

	if got := rateFor(s, "GET", "/api/reports/monthly"); got != 0.25 {
		t.Errorf("rare endpoint rate = %.2f, want 0.25 (score 2)", got)
	}
	if got := rateFor(s, "GET", "/api/products/1"); got != 0.1 {
		t.Errorf("busy endpoint rate = %.2f, want 0.10", got)
	}
}

// Rarity needs enough traffic to mean anything, or every endpoint looks rare
// in the first few seconds after a restart.
func TestRarityNeedsEnoughTraffic(t *testing.T) {
	s := riskSampler()
	for i := 0; i < 50; i++ {
		s.Mirror("GET", "/api/products/1")
	}
	s.Mirror("GET", "/api/reports/monthly")

	if got := rateFor(s, "GET", "/api/reports/monthly"); got != 0.1 {
		t.Errorf("rate = %.2f, want 0.10 before %d requests have been seen", got, rareMinRequests)
	}
}

// The worst case: a rare write endpoint that has already produced both a
// mismatch and an error. That is score 10 and should be mirrored every time.
func TestHighestRiskEndpointReaches100Percent(t *testing.T) {
	s := riskSampler()
	for i := 0; i < 297; i++ {
		s.Mirror("GET", "/api/products/1")
	}
	for i := 0; i < 3; i++ {
		s.Mirror("POST", "/api/orders")
	}
	s.UpdateSignals(map[string]Signal{"POST /api/orders": {Mismatches: 2, Errors: 1, Slow: 1}})

	if got := rateFor(s, "POST", "/api/orders"); got != 1 {
		t.Errorf("rate = %.2f, want 1.00 for the highest risk endpoint", got)
	}
	// And it really is mirrored every time, not just very often.
	for i := 0; i < 20; i++ {
		if !s.Mirror("POST", "/api/orders") {
			t.Fatal("a 100% endpoint was skipped")
		}
	}
}

func TestScoreToRateMapping(t *testing.T) {
	cases := []struct {
		score int
		want  float64
	}{{0, 0.1}, {1, 0.1}, {2, 0.25}, {3, 0.25}, {4, 0.5}, {5, 0.5}, {6, 1}, {10, 1}}
	for _, c := range cases {
		if got := rateForScore(c.score); got != c.want {
			t.Errorf("score %d -> %.2f, want %.2f", c.score, got, c.want)
		}
	}
}

// The dashboard shows why an endpoint is at the rate it is, so the reasons
// have to come back with the score.
func TestExplainListsTheReasons(t *testing.T) {
	s := riskSampler()
	s.UpdateSignals(map[string]Signal{"POST /api/orders": {Mismatches: 1}})
	s.Mirror("POST", "/api/orders")
	s.Mirror("GET", "/api/products/1")

	rows := s.Explain()
	if len(rows) != 2 {
		t.Fatalf("got %d endpoints, want 2", len(rows))
	}
	// Highest rate first.
	if rows[0].Endpoint != "POST /api/orders" {
		t.Errorf("first row = %s, want the riskiest endpoint", rows[0].Endpoint)
	}
	if rows[0].Score != scoreMismatch+scoreWrite || rows[0].Rate != 0.5 {
		t.Errorf("score = %d rate = %.2f, want %d and 0.50", rows[0].Score, rows[0].Rate, scoreMismatch+scoreWrite)
	}
	if len(rows[0].Reasons) != 2 {
		t.Errorf("reasons = %v, want one for the mismatch and one for the write", rows[0].Reasons)
	}
	if rows[1].Reasons[0] != "no risk signals" {
		t.Errorf("quiet endpoint reasons = %v", rows[1].Reasons)
	}
}

// Risk mode is the only one that explains itself; the others have a fixed
// rate that is already in the stats.
func TestExplainIsEmptyForOtherModes(t *testing.T) {
	s := NewWithRand(Config{Mode: Random, Rate: 0.5}, func() float64 { return 0 })
	s.Mirror("GET", "/api/products/1")
	if rows := s.Explain(); rows != nil {
		t.Errorf("Explain() = %v, want nothing outside risk mode", rows)
	}
}

// Traffic counts have to age out, or an endpoint that was busy once keeps
// looking busy and never qualifies as rare again.
func TestTrafficCountsAgeOut(t *testing.T) {
	s := riskSampler()

	// A burst of traffic to one endpoint, which is then never called again.
	for i := 0; i < 400; i++ {
		s.Mirror("GET", "/api/products/1")
	}
	for i := 0; i < 3; i++ {
		s.Mirror("GET", "/api/reports/monthly")
	}
	if got := rateFor(s, "GET", "/api/products/1"); got != 0.1 {
		t.Fatalf("busy endpoint rate = %.2f, want 0.10", got)
	}

	// Two windows later the burst is gone, and the quiet endpoint is now the
	// one carrying the traffic.
	s.RotateTraffic()
	for i := 0; i < 400; i++ {
		s.Mirror("GET", "/api/reports/monthly")
	}
	s.RotateTraffic()
	for i := 0; i < 400; i++ {
		s.Mirror("GET", "/api/reports/monthly")
	}

	if got := rateFor(s, "GET", "/api/products/1"); got != 0.25 {
		t.Errorf("rate = %.2f, want 0.25: the old burst should have aged out and left it rare", got)
	}
}
