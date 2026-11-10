package sampling

import (
	"fmt"
	"testing"
)

func templatedSampler(on bool) *Sampler {
	return NewWithRand(Config{Mode: Risk, RouteTemplates: on}, func() float64 { return 0 })
}

func TestTemplateCollapsesIdentifierSegments(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/api/products/1", "/api/products/{id}"},
		{"/api/products/12", "/api/products/{id}"},
		{"/api/users/7", "/api/users/{id}"},
		{"/api/orders/2024/15", "/api/orders/{id}/{id}"},
		{"/api/orders/3f8b7c21-5d9e-4a1b-8c0d-2e6f7a9b1c33", "/api/orders/{id}"},
		{"/api/sessions/01JBQ3K9ZQX7M4N2P8R6T5V0WY", "/api/sessions/{id}"},
		// Left alone: a collection, a version, a word, and an already
		// templated path.
		{"/api/products", "/api/products"},
		{"/api/v2/products", "/api/v2/products"},
		{"/health", "/health"},
		{"/api/products/{id}", "/api/products/{id}"},
		{"/", "/"},
		{"", ""},
	}
	for _, c := range cases {
		if got := Template(c.path); got != c.want {
			t.Errorf("Template(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// The point of the change: a mismatch found on one product id raises the rate
// for the next product id, because they are the same handler.
func TestTemplatedEndpointsShareRiskHistory(t *testing.T) {
	s := templatedSampler(true)
	s.UpdateSignals(map[string]Signal{"GET /api/products/{id}": {Mismatches: 1}})

	if got := rateFor(s, "GET", "/api/products/7"); got != 0.25 {
		t.Errorf("rate = %.2f, want 0.25: the mismatch belongs to the route", got)
	}
	if got := rateFor(s, "GET", "/api/users/3"); got != 0.1 {
		t.Errorf("rate = %.2f, a different route should be untouched", got)
	}
}

// A collection and one of its items are different handlers and keep separate
// scores, which is what keeps the R3 slow-query signal on /api/products from
// leaking into every product lookup.
func TestTemplatesKeepTheCollectionApartFromAnItem(t *testing.T) {
	s := templatedSampler(true)
	s.UpdateSignals(map[string]Signal{"GET /api/products": {Mismatches: 1, Slow: 4}})

	if got := rateFor(s, "GET", "/api/products"); got != 0.5 {
		t.Errorf("rate = %.2f, want 0.50 for the collection (mismatch + slow)", got)
	}
	if got := rateFor(s, "GET", "/api/products/9"); got != 0.1 {
		t.Errorf("rate = %.2f, want the item route left at the floor", got)
	}
}

func TestTemplatesKeepMethodsApart(t *testing.T) {
	s := templatedSampler(true)
	s.UpdateSignals(map[string]Signal{"DELETE /api/orders/{id}": {Errors: 2}})

	if got := rateFor(s, "DELETE", "/api/orders/4"); got != 0.5 {
		t.Errorf("rate = %.2f, want 0.50: a write with an error is score 5", got)
	}
	if got := rateFor(s, "GET", "/api/orders/4"); got != 0.1 {
		t.Errorf("rate = %.2f, the read should not inherit the write's history", got)
	}
}

// Turning templates off restores the per-URL scoring the earlier reports
// measured, which is what the week 11 comparison needs.
func TestTemplatesOffScoreEachUrlOnItsOwn(t *testing.T) {
	s := templatedSampler(false)
	s.UpdateSignals(map[string]Signal{"GET /api/products/12": {Mismatches: 1}})

	if got := rateFor(s, "GET", "/api/products/12"); got != 0.25 {
		t.Errorf("rate = %.2f, want 0.25 for the url that had the mismatch", got)
	}
	if got := rateFor(s, "GET", "/api/products/7"); got != 0.1 {
		t.Errorf("rate = %.2f, want the neighbouring url left alone", got)
	}
}

// The other half of the trade-off. Each product id on its own is a small
// share of traffic, so per-URL scoring calls it rare and lifts its rate.
// Templated, the ids add up to a busy route and the rare bonus goes away.
func TestTemplatingChangesWhichEndpointsCountAsRare(t *testing.T) {
	for _, templates := range []bool{false, true} {
		s := templatedSampler(templates)
		for i := 0; i < 300; i++ {
			s.Mirror("GET", fmt.Sprintf("/api/products/%d", i%10))
		}
		s.Mirror("GET", "/api/products/12")

		got := rateFor(s, "GET", "/api/products/12")
		want := 0.25
		if templates {
			want = 0.1
		}
		if got != want {
			t.Errorf("templates=%v: rate = %.2f, want %.2f", templates, got, want)
		}
	}
}

// Explain drives the dashboard, so it has to report the same grouping the
// decision used rather than the raw urls.
func TestExplainReportsTemplatedEndpoints(t *testing.T) {
	s := templatedSampler(true)
	for i := 1; i <= 5; i++ {
		s.Mirror("GET", fmt.Sprintf("/api/products/%d", i))
	}

	rows := s.Explain()
	if len(rows) != 1 {
		t.Fatalf("Explain returned %d rows, want the five urls folded into one", len(rows))
	}
	if rows[0].Endpoint != "GET /api/products/{id}" {
		t.Errorf("endpoint = %q, want GET /api/products/{id}", rows[0].Endpoint)
	}
	if rows[0].Requests != 5 {
		t.Errorf("requests = %d, want all 5 counted against the route", rows[0].Requests)
	}
}
