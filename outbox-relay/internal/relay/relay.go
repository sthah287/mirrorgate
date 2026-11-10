// Package relay moves comparison events from the Postgres outbox to the
// topic. It is the only part of MirrorGate that talks to the broker on the
// write side, which is what lets a broker outage turn into a delay instead of
// a loss.
package relay

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"mirrorgate/shared/broker"
	"mirrorgate/shared/outbox"
)

// Publisher is the piece of the broker the relay needs. Keeping it an
// interface means a test can make publishing fail on demand.
type Publisher interface {
	// PublishStoredBatch sends a batch and returns the request ids the
	// broker acknowledged. An error with a shorter acknowledged list means a
	// partial failure; the rest stay unpublished and are retried.
	PublishStoredBatch(ctx context.Context, msgs []broker.StoredMessage) ([]string, error)
}

var tracer = otel.Tracer("mirrorgate/outbox-relay")

type Config struct {
	BatchSize    int
	PollInterval time.Duration
	MaxBackoff   time.Duration
	Retention    time.Duration
	// PublishTimeout bounds one batch. The claiming transaction is open while
	// the batch publishes, so this is also how long rows can stay locked.
	PublishTimeout time.Duration
	// SinglePublish restores the original behaviour of one publish call per
	// row. It is slow on purpose and exists so the before and after
	// measurement can be reproduced without checking out an old commit.
	SinglePublish bool
}

type Relay struct {
	store     *outbox.Store
	publisher Publisher
	cfg       Config
}

func New(store *outbox.Store, publisher Publisher, cfg Config) *Relay {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 200 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 10 * time.Second
	}
	if cfg.Retention <= 0 {
		cfg.Retention = time.Hour
	}
	if cfg.PublishTimeout <= 0 {
		cfg.PublishTimeout = 10 * time.Second
	}
	return &Relay{store: store, publisher: publisher, cfg: cfg}
}

// Run polls until the context is cancelled. Backoff grows while the broker is
// unreachable and resets the moment a batch gets through.
func (r *Relay) Run(ctx context.Context) error {
	backoff := r.cfg.PollInterval
	cleanup := time.NewTicker(time.Minute)
	defer cleanup.Stop()

	for {
		sent, err := r.drainOnce(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			backoff = min(backoff*2, r.cfg.MaxBackoff)
			slog.Warn("outbox batch failed, will retry", "err", err, "retry_in", backoff)
		case sent > 0:
			// More may be waiting, so come straight back for the next batch.
			backoff = r.cfg.PollInterval
			continue
		default:
			backoff = r.cfg.PollInterval
		}

		select {
		case <-ctx.Done():
			return nil
		case <-cleanup.C:
			r.cleanup(ctx)
		case <-time.After(backoff):
		}
	}
}

// drainOnce publishes one batch. Rows stay locked from the moment they are
// claimed until they are marked published and the transaction commits, so two
// relays never publish the same row at the same time.
func (r *Relay) drainOnce(ctx context.Context) (int, error) {
	batchCtx, cancel := context.WithTimeout(ctx, r.cfg.PublishTimeout)
	defer cancel()

	tx, pending, err := r.store.Claim(batchCtx, r.cfg.BatchSize)
	if err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		tx.Rollback(batchCtx)
		return 0, nil
	}

	// One span per row, so each event stays in the trace of the request that
	// produced it, but a single publish call for the whole batch.
	msgs, spans := r.prepare(batchCtx, pending)
	acked, publishErr := r.publish(batchCtx, msgs)
	for _, span := range spans {
		if publishErr != nil {
			span.RecordError(publishErr)
			span.SetStatus(codes.Error, "publish failed")
		}
		span.End()
	}

	if publishErr != nil {
		// Only the acknowledged ones are marked. Anything the broker did not
		// confirm stays pending and goes out again next pass.
		if markErr := r.finish(batchCtx, tx, acked); markErr != nil {
			slog.Error("could not mark the acknowledged part of a failed batch", "err", markErr)
		}
		r.recordFailure(ctx, remaining(pending, acked), publishErr)
		return len(acked), publishErr
	}

	if err := r.finish(batchCtx, tx, acked); err != nil {
		// The events reached the broker but the rows are still unpublished,
		// so they will be sent again. The worker drops duplicates by
		// request id, which is why that matters.
		r.recordFailure(ctx, pending, err)
		return 0, err
	}
	return len(acked), nil
}

// publish sends the batch, or one message at a time when the slow path is
// turned on for comparison.
func (r *Relay) publish(ctx context.Context, msgs []broker.StoredMessage) ([]string, error) {
	if !r.cfg.SinglePublish {
		return r.publisher.PublishStoredBatch(ctx, msgs)
	}
	var acked []string
	for _, m := range msgs {
		one, err := r.publisher.PublishStoredBatch(ctx, []broker.StoredMessage{m})
		acked = append(acked, one...)
		if err != nil {
			return acked, err
		}
	}
	return acked, nil
}

// prepare turns claimed rows into messages, restoring each row's trace so the
// relay hop lands inside the original request's trace rather than starting a
// new one. The returned spans are still open and the caller ends them.
func (r *Relay) prepare(ctx context.Context, pending []outbox.Pending) ([]broker.StoredMessage, []trace.Span) {
	msgs := make([]broker.StoredMessage, 0, len(pending))
	spans := make([]trace.Span, 0, len(pending))

	for _, p := range pending {
		rowCtx := ctx
		if p.Traceparent != "" {
			rowCtx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": p.Traceparent})
		}
		rowCtx, span := tracer.Start(rowCtx, "relay publish")
		span.SetAttributes(attribute.String("mirrorgate.request_id", p.RequestID))

		// The message carries this span's context, not the caller's, so the
		// worker links to the relay hop for this specific request.
		carrier := propagation.MapCarrier{}
		otel.GetTextMapPropagator().Inject(rowCtx, carrier)

		msgs = append(msgs, broker.StoredMessage{
			RequestID:   p.RequestID,
			Payload:     p.Payload,
			Traceparent: carrier.Get("traceparent"),
		})
		spans = append(spans, span)
	}
	return msgs, spans
}

// remaining returns the rows that were not acknowledged.
func remaining(pending []outbox.Pending, acked []string) []outbox.Pending {
	if len(acked) == 0 {
		return pending
	}
	done := make(map[string]bool, len(acked))
	for _, id := range acked {
		done[id] = true
	}
	out := make([]outbox.Pending, 0, len(pending)-len(acked))
	for _, p := range pending {
		if !done[p.RequestID] {
			out = append(out, p)
		}
	}
	return out
}

func (r *Relay) finish(ctx context.Context, tx pgx.Tx, published []string) error {
	if len(published) == 0 {
		return tx.Rollback(ctx)
	}
	if err := outbox.MarkPublished(ctx, tx, published); err != nil {
		tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func (r *Relay) recordFailure(ctx context.Context, pending []outbox.Pending, cause error) {
	if len(pending) == 0 || errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	ids := make([]string, 0, len(pending))
	for _, p := range pending {
		ids = append(ids, p.RequestID)
	}
	// A fresh context: the batch one may already be done for.
	failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.store.RecordFailure(failCtx, ids, cause); err != nil {
		slog.Warn("could not record outbox failure", "err", err)
	}
}

func (r *Relay) cleanup(ctx context.Context) {
	cleanCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	deleted, err := r.store.DeletePublished(cleanCtx, r.cfg.Retention)
	if err != nil {
		slog.Warn("outbox cleanup", "err", err)
		return
	}
	if deleted > 0 {
		slog.Info("outbox cleanup", "deleted", deleted)
	}
}
