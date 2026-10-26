// Package outbox stores comparison events in Postgres so a broker outage
// delays them instead of losing them. The gateway writes rows, the relay
// publishes them and marks them done.
//
// The guarantee is deliberately narrow: once a row is committed, the event
// will eventually reach the topic. It says nothing about the stable service's
// own database write, which happens in a different service entirely, and it
// does not survive a gateway crash between the shadow request finishing and
// the row committing. See docs/outbox.md.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"mirrorgate/shared/event"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pending is one row waiting to be published.
type Pending struct {
	RequestID   string
	Payload     []byte
	Traceparent string
	Attempts    int
}

// Publish writes the event to the outbox. It satisfies the same interface the
// gateway used for publishing straight to Kafka, so switching between the two
// is a one line change in main.
//
// The insert is a no-op if the request id is already there, which makes a
// retry of the same shadow request harmless.
func (s *Store) Publish(ctx context.Context, e event.Comparison) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode event %s: %w", e.RequestID, err)
	}

	// Captured here rather than in the relay: this is the only moment the
	// request's trace context still exists.
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	_, err = s.pool.Exec(ctx, `
		INSERT INTO comparison_outbox (request_id, payload, traceparent)
		VALUES ($1, $2, NULLIF($3, ''))
		ON CONFLICT (request_id) DO NOTHING`,
		e.RequestID, payload, carrier.Get("traceparent"))
	if err != nil {
		return fmt.Errorf("write outbox row %s: %w", e.RequestID, err)
	}
	return nil
}

// Claim locks up to limit unpublished rows and hands them back with the
// transaction still open. SKIP LOCKED means a second relay picks up different
// rows instead of blocking, so running two is safe even though one is enough
// here. The caller must Commit or Rollback.
func (s *Store) Claim(ctx context.Context, limit int) (pgx.Tx, []Pending, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin outbox batch: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT request_id, payload, COALESCE(traceparent, ''), attempts
		FROM comparison_outbox
		WHERE published_at IS NULL
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		tx.Rollback(ctx)
		return nil, nil, fmt.Errorf("claim outbox rows: %w", err)
	}

	var pending []Pending
	for rows.Next() {
		var p Pending
		if err := rows.Scan(&p.RequestID, &p.Payload, &p.Traceparent, &p.Attempts); err != nil {
			rows.Close()
			tx.Rollback(ctx)
			return nil, nil, fmt.Errorf("scan outbox row: %w", err)
		}
		pending = append(pending, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		tx.Rollback(ctx)
		return nil, nil, fmt.Errorf("claim outbox rows: %w", err)
	}
	return tx, pending, nil
}

// MarkPublished runs inside the claiming transaction, so the rows stay locked
// from the moment they are read until they are marked.
func MarkPublished(ctx context.Context, tx pgx.Tx, requestIDs []string) error {
	if len(requestIDs) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx,
		`UPDATE comparison_outbox SET published_at = now(), last_error = NULL
		 WHERE request_id = ANY($1)`, requestIDs)
	if err != nil {
		return fmt.Errorf("mark published: %w", err)
	}
	return nil
}

// RecordFailure runs after the batch transaction has been rolled back, so the
// attempt count survives the failure that caused the rollback.
func (s *Store) RecordFailure(ctx context.Context, requestIDs []string, cause error) error {
	if len(requestIDs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE comparison_outbox SET attempts = attempts + 1, last_error = $2
		 WHERE request_id = ANY($1)`, requestIDs, cause.Error())
	if err != nil {
		return fmt.Errorf("record outbox failure: %w", err)
	}
	return nil
}

// DeletePublished keeps the table from growing without bound. Published rows
// are only kept for a while so a run can still be inspected afterwards.
func (s *Store) DeletePublished(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM comparison_outbox
		 WHERE published_at IS NOT NULL AND published_at < now() - make_interval(secs => $1)`,
		olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("delete published outbox rows: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Stats is what the dashboard and the experiment harness read to tell whether
// the pipeline is keeping up.
type Stats struct {
	Pending          int64   `json:"pending"`
	Published        int64   `json:"published"`
	OldestPendingSec float64 `json:"oldest_pending_sec"`
	FailedAttempts   int64   `json:"failed_attempts"`
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE published_at IS NULL),
		       count(*) FILTER (WHERE published_at IS NOT NULL),
		       COALESCE(EXTRACT(epoch FROM now() - min(created_at) FILTER (WHERE published_at IS NULL)), 0),
		       COALESCE(sum(attempts), 0)
		FROM comparison_outbox`).Scan(
		&st.Pending, &st.Published, &st.OldestPendingSec, &st.FailedAttempts)
	if err != nil {
		return Stats{}, fmt.Errorf("outbox stats: %w", err)
	}
	return st, nil
}
