// Package consume turns a comparison event into a stored result. This is
// the work the gateway used to do inline after every shadow request.
package consume

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"mirrorgate/shared/compare"
	"mirrorgate/shared/event"
	"mirrorgate/shared/storage"
)

var tracer = otel.Tracer("mirrorgate/comparison-worker")

type Store interface {
	SaveComparison(ctx context.Context, c storage.Comparison) error
}

type Handler struct {
	store Store
}

func New(store Store) *Handler {
	return &Handler{store: store}
}

// Handle compares one event and saves the result. Returning an error leaves
// the Kafka offset uncommitted so the event is retried.
func (h *Handler) Handle(ctx context.Context, e event.Comparison) error {
	// ctx already carries the trace context read off the Kafka headers, so
	// this span lands in the same trace as the request that produced it.
	ctx, span := tracer.Start(ctx, "process comparison")
	defer span.End()

	c := Result(e)
	span.SetAttributes(
		attribute.String("mirrorgate.request_id", c.RequestID),
		attribute.String("http.route", c.Path),
		attribute.String("mirrorgate.outcome", c.Outcome),
	)

	if c.Outcome != compare.Match {
		slog.Warn("candidate mismatch", "request_id", c.RequestID, "path", c.Path,
			"outcome", c.Outcome, "differences", c.Differences, "candidate_error", c.CandidateError)
	} else {
		slog.Info("comparison processed", "request_id", c.RequestID, "path", c.Path, "outcome", c.Outcome)
	}

	saveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	saveCtx, saveSpan := tracer.Start(saveCtx, "save comparison")
	defer saveSpan.End()
	if err := h.store.SaveComparison(saveCtx, c); err != nil {
		saveSpan.RecordError(err)
		saveSpan.SetStatus(codes.Error, "save failed")
		return err
	}
	return nil
}

// Result runs the comparison for one event. It is separate from Handle so
// tests can check the classification without a database.
func Result(e event.Comparison) storage.Comparison {
	stable := compare.Response{
		Status:  e.Stable.Status,
		Body:    []byte(e.Stable.Body),
		Latency: duration(e.Stable.LatencyMs),
	}
	candidate := compare.Response{
		Status:  e.Candidate.Status,
		Body:    []byte(e.Candidate.Body),
		Latency: duration(e.Candidate.LatencyMs),
	}
	// The gateway already phrased the failure ("timed out after 2s"), so the
	// worker only needs to know that there was one.
	if e.CandidateError != "" {
		candidate.Err = errors.New(e.CandidateError)
	}

	result := compare.Compare(stable, candidate)
	c := storage.Comparison{
		RequestID:          e.RequestID,
		TraceID:            e.TraceID,
		Method:             e.Method,
		Path:               e.Path,
		Query:              e.Query,
		ReceivedAt:         e.ReceivedAt,
		StableStatus:       e.Stable.Status,
		StableLatencyMs:    e.Stable.LatencyMs,
		CandidateLatencyMs: e.Candidate.LatencyMs,
		StableBody:         e.Stable.Body,
		CandidateBody:      e.Candidate.Body,
		StatusMatch:        result.StatusMatch,
		BodyMatch:          result.BodyMatch,
		CandidateSlow:      result.CandidateSlow,
		Outcome:            result.Outcome,
		Differences:        result.Differences,
		CandidateError:     e.CandidateError,
	}
	if e.CandidateError == "" {
		status := e.Candidate.Status
		c.CandidateStatus = &status
	}
	return c
}

func duration(ms float64) time.Duration {
	return time.Duration(ms * float64(time.Millisecond))
}
