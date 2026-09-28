package sampling

import (
	"math/rand/v2"
	"testing"
)

// seeded gives every test the same sequence of draws, so a rate of 0.5 is
// checked against a known outcome instead of a probability.
func seeded() func() float64 {
	r := rand.New(rand.NewPCG(1, 2))
	return r.Float64
}

func TestFullModeMirrorsEverything(t *testing.T) {
	s := NewWithRand(Config{Mode: Full, Rate: 1}, seeded())
	for i := 0; i < 100; i++ {
		if !s.Mirror("/api/products/1") {
			t.Fatalf("request %d was skipped in full mode", i)
		}
	}
	if st := s.Stats(); st.Mirrored != 100 || st.Skipped != 0 {
		t.Errorf("stats = %+v, want 100 mirrored", st)
	}
}

func TestZeroRateMirrorsNothing(t *testing.T) {
	s := NewWithRand(Config{Mode: Random, Rate: 0}, seeded())
	for i := 0; i < 100; i++ {
		if s.Mirror("/api/products/1") {
			t.Fatalf("request %d was mirrored at rate 0", i)
		}
	}
	if st := s.Stats(); st.Skipped != 100 || st.Mirrored != 0 {
		t.Errorf("stats = %+v, want 100 skipped", st)
	}
}

// A seeded generator makes the same decisions every run, so this asserts an
// exact count rather than a range.
func TestSeededRandomIsRepeatable(t *testing.T) {
	counts := make([]int64, 2)
	for run := range counts {
		s := NewWithRand(Config{Mode: Random, Rate: 0.25}, seeded())
		for i := 0; i < 1000; i++ {
			s.Mirror("/api/products/1")
		}
		counts[run] = s.Stats().Mirrored
	}
	if counts[0] != counts[1] {
		t.Fatalf("two seeded runs mirrored %d and %d requests", counts[0], counts[1])
	}
	// 1000 draws at 0.25 should land near 250. A wide band still catches a
	// rate that is being ignored or applied twice.
	if counts[0] < 200 || counts[0] > 300 {
		t.Errorf("mirrored %d of 1000 at rate 0.25", counts[0])
	}
}

func TestEndpointRulesOverrideTheDefaultRate(t *testing.T) {
	s := NewWithRand(Config{
		Mode: Endpoint,
		Rate: 1,
		Rules: []Rule{
			{Pattern: "/health", Rate: 0},
			{Pattern: "/api/users/*", Rate: 0},
			{Pattern: "/api/products/*", Rate: 1},
		},
	}, seeded())

	cases := []struct {
		path string
		want bool
	}{
		{"/health", false},
		{"/api/users/7", false},
		{"/api/products/12", true},
		{"/api/products", true},
		// Nothing matches, so the fallback rate applies.
		{"/api/orders", true},
	}
	for _, c := range cases {
		if got := s.Mirror(c.path); got != c.want {
			t.Errorf("Mirror(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestFirstMatchingRuleWins(t *testing.T) {
	s := NewWithRand(Config{
		Mode:  Endpoint,
		Rate:  1,
		Rules: []Rule{{Pattern: "/api/products/8", Rate: 0}, {Pattern: "/api/products/*", Rate: 1}},
	}, seeded())

	if s.Mirror("/api/products/8") {
		t.Error("the more specific rule should have skipped /api/products/8")
	}
	if !s.Mirror("/api/products/12") {
		t.Error("/api/products/12 should fall through to the prefix rule")
	}
}

func TestParse(t *testing.T) {
	t.Run("random rate", func(t *testing.T) {
		s, err := Parse("random", "0.25", "")
		if err != nil {
			t.Fatal(err)
		}
		if st := s.Stats(); st.Mode != Random || st.Rate != 0.25 {
			t.Errorf("stats = %+v", st)
		}
	})

	t.Run("full mode ignores the rate", func(t *testing.T) {
		s, err := Parse("full", "0.25", "")
		if err != nil {
			t.Fatal(err)
		}
		if st := s.Stats(); st.Rate != 1 {
			t.Errorf("rate = %v, want 1 in full mode", st.Rate)
		}
	})

	t.Run("endpoint rules", func(t *testing.T) {
		s, err := Parse("endpoint", "0.1", "/api/products/*=0.5, /api/users/*=0.25,/health=0")
		if err != nil {
			t.Fatal(err)
		}
		st := s.Stats()
		if len(st.Rules) != 3 || st.Rules[1].Pattern != "/api/users/*" || st.Rules[1].Rate != 0.25 {
			t.Fatalf("rules = %+v", st.Rules)
		}
		if st.Rate != 0.1 {
			t.Errorf("fallback rate = %v, want 0.1", st.Rate)
		}
	})

	t.Run("bad input", func(t *testing.T) {
		for _, c := range []struct{ mode, rate, rules string }{
			{"sometimes", "", ""},
			{"random", "1.5", ""},
			{"random", "half", ""},
			{"endpoint", "", ""},
			{"endpoint", "", "/api/products/*"},
			{"endpoint", "", "/api/products/*=nope"},
		} {
			if _, err := Parse(c.mode, c.rate, c.rules); err == nil {
				t.Errorf("Parse(%q, %q, %q) should have failed", c.mode, c.rate, c.rules)
			}
		}
	})
}
