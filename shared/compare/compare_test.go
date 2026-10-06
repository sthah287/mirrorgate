package compare

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestCompare(t *testing.T) {
	ok := func(body string) Response {
		return Response{Status: 200, Body: []byte(body), Latency: 5 * time.Millisecond}
	}

	tests := []struct {
		name        string
		stable      Response
		candidate   Response
		outcome     string
		statusMatch bool
		bodyMatch   bool
		differences []string
	}{
		{
			name:        "identical responses",
			stable:      ok(`{"id":1,"price":19.99}`),
			candidate:   ok(`{"id":1,"price":19.99}`),
			outcome:     Match,
			statusMatch: true,
			bodyMatch:   true,
			differences: []string{},
		},
		{
			name:        "key order and whitespace are ignored",
			stable:      ok(`{"id":3,"name":"Sam","plan":"pro"}`),
			candidate:   ok("{\n    \"plan\": \"pro\",\n    \"name\": \"Sam\",\n    \"id\": 3\n}"),
			outcome:     Match,
			statusMatch: true,
			bodyMatch:   true,
			differences: []string{},
		},
		{
			name:        "different body is detected",
			stable:      ok(`{"id":12,"price":49.99}`),
			candidate:   ok(`{"id":12,"price":59.99}`),
			outcome:     Different,
			statusMatch: true,
			bodyMatch:   false,
			differences: []string{"price: 49.99 != 59.99"},
		},
		{
			name:        "different status is detected",
			stable:      Response{Status: 200, Body: []byte(`{}`)},
			candidate:   Response{Status: 404, Body: []byte(`{}`)},
			outcome:     Different,
			statusMatch: false,
			bodyMatch:   true,
			differences: []string{"status: 200 != 404"},
		},
		{
			name:        "candidate 500 is an error",
			stable:      ok(`{"id":8}`),
			candidate:   Response{Status: 500, Body: []byte(`{"error":"inventory lookup failed"}`)},
			outcome:     Error,
			statusMatch: false,
			bodyMatch:   false,
			differences: []string{"status: 200 != 500", "body: differs"},
		},
		{
			name:        "nested array difference",
			stable:      ok(`{"items":[{"id":1},{"id":2}]}`),
			candidate:   ok(`{"items":[{"id":1},{"id":3},{"id":4}]}`),
			outcome:     Different,
			statusMatch: true,
			bodyMatch:   false,
			differences: []string{"items: length 2 != 3", "items[1].id: 2 != 3"},
		},
		{
			name:        "plain text bodies",
			stable:      ok("pong\n"),
			candidate:   ok("pong"),
			outcome:     Match,
			statusMatch: true,
			bodyMatch:   true,
			differences: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compare(tt.stable, tt.candidate)
			if got.Outcome != tt.outcome {
				t.Errorf("outcome = %s, want %s", got.Outcome, tt.outcome)
			}
			if got.StatusMatch != tt.statusMatch {
				t.Errorf("status match = %v, want %v", got.StatusMatch, tt.statusMatch)
			}
			if got.BodyMatch != tt.bodyMatch {
				t.Errorf("body match = %v, want %v", got.BodyMatch, tt.bodyMatch)
			}
			if !reflect.DeepEqual(got.Differences, tt.differences) {
				t.Errorf("differences = %q, want %q", got.Differences, tt.differences)
			}
		})
	}
}

func TestCompareCandidateFailure(t *testing.T) {
	got := Compare(
		Response{Status: 200, Body: []byte(`{}`)},
		Response{Err: errors.New("connection refused")},
	)
	if got.Outcome != Error {
		t.Errorf("outcome = %s, want error", got.Outcome)
	}
}

func TestCompareSlowCandidate(t *testing.T) {
	stable := Response{Status: 200, Body: []byte(`[]`), Latency: 3 * time.Millisecond}
	candidate := Response{Status: 200, Body: []byte(`[]`), Latency: 410 * time.Millisecond}

	got := Compare(stable, candidate)
	if got.Outcome != Match {
		t.Errorf("outcome = %s, a slow but correct response should still match", got.Outcome)
	}
	if !got.CandidateSlow {
		t.Error("expected candidate to be flagged as slow")
	}
}

// The order endpoint returns a generated id, a timestamp, and a payment
// reference that are different on every request. Without ignore rules every
// single order would be reported as a difference.
func TestIgnoredFields(t *testing.T) {
	rules := Rules{IgnoreFields: []string{"order_id", "created_at", "payment.reference"}}

	stable := Response{Status: 200, Body: []byte(`{"order_id":"A1","created_at":"2026-10-05T10:00:00Z","payment":{"reference":"pay_1","amount":25},"total":25}`)}
	candidate := Response{Status: 200, Body: []byte(`{"order_id":"B2","created_at":"2026-10-05T10:00:03Z","payment":{"reference":"pay_2","amount":25},"total":25}`)}

	got := rules.Compare(stable, candidate)
	if got.Outcome != Match {
		t.Errorf("outcome = %s, want match (differences: %v)", got.Outcome, got.Differences)
	}

	// The same bodies with a real change in them still has to come out as a
	// difference, otherwise the ignore list would be hiding regressions.
	candidate.Body = []byte(`{"order_id":"B2","created_at":"2026-10-05T10:00:03Z","payment":{"reference":"pay_2","amount":25},"total":20}`)
	got = rules.Compare(stable, candidate)
	if got.Outcome != Different {
		t.Fatalf("outcome = %s, want different", got.Outcome)
	}
	if len(got.Differences) != 1 || got.Differences[0] != "total: 25 != 20" {
		t.Errorf("differences = %v, want just the total", got.Differences)
	}
}

// A bare name ignores the field anywhere, a dotted path only in one place.
func TestIgnoreByNameVersusPath(t *testing.T) {
	stable := Response{Status: 200, Body: []byte(`{"id":1,"item":{"id":5}}`)}
	candidate := Response{Status: 200, Body: []byte(`{"id":2,"item":{"id":6}}`)}

	byName := Rules{IgnoreFields: []string{"id"}}.Compare(stable, candidate)
	if byName.Outcome != Match {
		t.Errorf("ignoring \"id\" by name left %v", byName.Differences)
	}

	byPath := Rules{IgnoreFields: []string{"item.id"}}.Compare(stable, candidate)
	if len(byPath.Differences) != 1 || byPath.Differences[0] != "id: 1 != 2" {
		t.Errorf("differences = %v, want only the top level id", byPath.Differences)
	}
}

func TestSlowThresholdIsConfigurable(t *testing.T) {
	stable := Response{Status: 200, Body: []byte(`{}`), Latency: 2 * time.Millisecond}
	candidate := Response{Status: 200, Body: []byte(`{}`), Latency: 60 * time.Millisecond}

	if DefaultRules().Compare(stable, candidate).CandidateSlow {
		t.Error("58ms slower should not trip the default 200ms threshold")
	}
	strict := Rules{SlowThreshold: 50 * time.Millisecond}
	if !strict.Compare(stable, candidate).CandidateSlow {
		t.Error("58ms slower should trip a 50ms threshold")
	}
}

func TestParseRules(t *testing.T) {
	rules, err := ParseRules(" created_at , order_id ", "350")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules.IgnoreFields) != 2 || rules.IgnoreFields[0] != "created_at" || rules.IgnoreFields[1] != "order_id" {
		t.Errorf("ignore fields = %v", rules.IgnoreFields)
	}
	if rules.SlowThreshold != 350*time.Millisecond {
		t.Errorf("slow threshold = %s, want 350ms", rules.SlowThreshold)
	}

	empty, err := ParseRules("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.IgnoreFields) != 0 || empty.SlowThreshold != SlowThreshold {
		t.Errorf("empty config should give the defaults, got %+v", empty)
	}

	if _, err := ParseRules("", "soon"); err == nil {
		t.Error("a non-numeric threshold should be rejected")
	}
}

// One badly broken response shouldn't be able to write an unbounded list of
// differences into the database.
func TestDifferencesAreCapped(t *testing.T) {
	stable, candidate := map[string]int{}, map[string]int{}
	for i := 0; i < 200; i++ {
		stable[fmt.Sprintf("field_%03d", i)] = i
		candidate[fmt.Sprintf("field_%03d", i)] = i + 1
	}
	stableBody, err := json.Marshal(stable)
	if err != nil {
		t.Fatal(err)
	}
	candidateBody, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}

	diffs := DiffBodies(stableBody, candidateBody)
	if len(diffs) != maxDifferences {
		t.Errorf("got %d differences, want them capped at %d", len(diffs), maxDifferences)
	}
}
