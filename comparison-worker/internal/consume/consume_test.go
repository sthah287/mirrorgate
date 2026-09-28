package consume

import (
	"context"
	"errors"
	"testing"
	"time"

	"mirrorgate/shared/compare"
	"mirrorgate/shared/event"
	"mirrorgate/shared/storage"
)

type fakeStore struct {
	saved []storage.Comparison
	fail  error
}

func (f *fakeStore) SaveComparison(ctx context.Context, c storage.Comparison) error {
	if f.fail != nil {
		return f.fail
	}
	f.saved = append(f.saved, c)
	return nil
}

// These are the same scenarios the candidate service fakes on purpose, but
// checked at the event level, which is where the worker sees them.
func TestResultClassifiesEvents(t *testing.T) {
	cases := []struct {
		name      string
		e         event.Comparison
		outcome   string
		slow      bool
		diffCount int
	}{
		{
			name: "identical responses match",
			e: event.Comparison{
				Stable:    event.Response{Status: 200, Body: `{"id":1,"price":19.99}`, LatencyMs: 1},
				Candidate: event.Response{Status: 200, Body: `{"id":1,"price":19.99}`, LatencyMs: 1},
			},
			outcome: compare.Match,
		},
		{
			name: "reordered json still matches",
			e: event.Comparison{
				Stable:    event.Response{Status: 200, Body: `{"id":3,"name":"Sam Rivera"}`, LatencyMs: 1},
				Candidate: event.Response{Status: 200, Body: "{\n    \"name\": \"Sam Rivera\",\n    \"id\": 3\n}", LatencyMs: 1},
			},
			outcome: compare.Match,
		},
		{
			name: "price change is a difference",
			e: event.Comparison{
				Stable:    event.Response{Status: 200, Body: `{"id":12,"price":49.99}`, LatencyMs: 1},
				Candidate: event.Response{Status: 200, Body: `{"id":12,"price":59.99}`, LatencyMs: 1},
			},
			outcome:   compare.Different,
			diffCount: 1,
		},
		{
			name: "candidate 500 is an error",
			e: event.Comparison{
				Stable:    event.Response{Status: 200, Body: `{"id":8}`, LatencyMs: 1},
				Candidate: event.Response{Status: 500, Body: `{"error":"inventory lookup failed"}`, LatencyMs: 1},
			},
			outcome:   compare.Error,
			diffCount: 2,
		},
		{
			name: "timeout is an error with no candidate status",
			e: event.Comparison{
				Stable:         event.Response{Status: 200, Body: `{"id":7}`, LatencyMs: 1},
				Candidate:      event.Response{LatencyMs: 2000},
				CandidateError: "timed out after 2s",
			},
			outcome: compare.Error,
		},
		{
			name: "same body but much slower is a match with the slow flag",
			e: event.Comparison{
				Stable:    event.Response{Status: 200, Body: `[{"id":4}]`, LatencyMs: 1},
				Candidate: event.Response{Status: 200, Body: `[{"id":4}]`, LatencyMs: 401},
			},
			outcome: compare.Match,
			slow:    true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Result(c.e)
			if got.Outcome != c.outcome {
				t.Errorf("outcome = %s, want %s (differences: %v)", got.Outcome, c.outcome, got.Differences)
			}
			if got.CandidateSlow != c.slow {
				t.Errorf("candidate_slow = %v, want %v", got.CandidateSlow, c.slow)
			}
			if len(got.Differences) != c.diffCount {
				t.Errorf("differences = %v, want %d of them", got.Differences, c.diffCount)
			}
		})
	}
}

// A candidate that never answered has no status, and the dashboard shows a
// dash instead of a number for it.
func TestCandidateStatusIsNilAfterAnError(t *testing.T) {
	withError := Result(event.Comparison{
		Stable:         event.Response{Status: 200, LatencyMs: 1},
		Candidate:      event.Response{LatencyMs: 2000},
		CandidateError: "timed out after 2s",
	})
	if withError.CandidateStatus != nil {
		t.Errorf("candidate status = %d, want nil", *withError.CandidateStatus)
	}

	answered := Result(event.Comparison{
		Stable:    event.Response{Status: 200, Body: `{}`, LatencyMs: 1},
		Candidate: event.Response{Status: 404, Body: `{}`, LatencyMs: 1},
	})
	if answered.CandidateStatus == nil || *answered.CandidateStatus != 404 {
		t.Errorf("candidate status = %v, want 404", answered.CandidateStatus)
	}
}

func TestHandleSavesTheComparison(t *testing.T) {
	store := &fakeStore{}
	received := time.Date(2026, 9, 20, 14, 30, 0, 0, time.UTC)

	err := New(store).Handle(context.Background(), event.Comparison{
		RequestID:  "REQ123",
		TraceID:    "4bf92f3577b34da6a3ce929d0e0e4736",
		Method:     "GET",
		Path:       "/api/products/12",
		Query:      "currency=usd",
		ReceivedAt: received,
		Stable:     event.Response{Status: 200, Body: `{"price":49.99}`, LatencyMs: 1.5},
		Candidate:  event.Response{Status: 200, Body: `{"price":59.99}`, LatencyMs: 2.5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.saved) != 1 {
		t.Fatalf("saved %d comparisons, want 1", len(store.saved))
	}

	c := store.saved[0]
	if c.RequestID != "REQ123" || c.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("ids = %s / %s", c.RequestID, c.TraceID)
	}
	if c.Path != "/api/products/12" || c.Query != "currency=usd" || !c.ReceivedAt.Equal(received) {
		t.Errorf("request fields = %+v", c)
	}
	if c.Outcome != compare.Different || len(c.Differences) != 1 {
		t.Errorf("outcome = %s differences = %v", c.Outcome, c.Differences)
	}
	if c.StableLatencyMs != 1.5 || c.CandidateLatencyMs != 2.5 {
		t.Errorf("latencies = %v / %v", c.StableLatencyMs, c.CandidateLatencyMs)
	}
}

// A failed save has to surface so the Kafka offset stays uncommitted and the
// event gets retried instead of disappearing.
func TestHandleReturnsSaveErrors(t *testing.T) {
	store := &fakeStore{fail: errors.New("connection refused")}
	err := New(store).Handle(context.Background(), event.Comparison{
		Stable:    event.Response{Status: 200, Body: `{}`},
		Candidate: event.Response{Status: 200, Body: `{}`},
	})
	if err == nil {
		t.Fatal("expected the save error to be returned")
	}
}
