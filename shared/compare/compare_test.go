package compare

import (
	"errors"
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
