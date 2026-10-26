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

	"mirrorgate/shared/outbox"
)

// Publisher is the piece of the broker the relay needs. Keeping it an
// interface means a test can make publishing fail on demand.
type Publisher interface {
	PublishStored(ctx context.Context, requestID string, payload []byte) error
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

	published := make([]string, 0, len(pending))
	for _, p := range pending {
		if err := r.publishOne(batchCtx, p); err != nil {
			// Stop at the first failure. Everything already published in this
			// batch still gets marked, so a partial outage doesn't replay the
			// whole batch.
			if markErr := r.finish(batchCtx, tx, published); markErr != nil {
				slog.Error("could not mark the published part of a failed batch", "err", markErr)
			}
			r.recordFailure(ctx, pending[len(published):], err)
			return len(published), err
		}
		published = append(published, p.RequestID)
	}

	if err := r.finish(batchCtx, tx, published); err != nil {
		// The events reached the broker but the rows are still unpublished,
		// so they will be sent again. The worker drops duplicates by
		// request id, which is why that matters.
		r.recordFailure(ctx, pending, err)
		return 0, err
	}
	return len(published), nil
}

// publishOne rebuilds the request's trace from the traceparent stored with
// the row, so the relay hop shows up inside the original trace instead of
// starting a new one.
func (r *Relay) publishOne(ctx context.Context, p outbox.Pending) error {
	if p.Traceparent != "" {
		ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": p.Traceparent})
	}
	ctx, span := tracer.Start(ctx, "relay publish")
	defer span.End()
	span.SetAttributes(attribute.String("mirrorgate.request_id", p.RequestID))

	if err := r.publisher.PublishStored(ctx, p.RequestID, p.Payload); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish failed")
		return err
	}
	return nil
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
