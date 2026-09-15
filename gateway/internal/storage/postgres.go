package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("comparison not found")

type Comparison struct {
	ID                 int64     `json:"id"`
	RequestID          string    `json:"request_id"`
	Method             string    `json:"method"`
	Path               string    `json:"path"`
	Query              string    `json:"query"`
	StableStatus       int       `json:"stable_status"`
	CandidateStatus    *int      `json:"candidate_status"`
	StableLatencyMs    float64   `json:"stable_latency_ms"`
	CandidateLatencyMs float64   `json:"candidate_latency_ms"`
	StableBody         string    `json:"stable_body,omitempty"`
	CandidateBody      string    `json:"candidate_body,omitempty"`
	StatusMatch        bool      `json:"status_match"`
	BodyMatch          bool      `json:"body_match"`
	CandidateSlow      bool      `json:"candidate_slow"`
	Outcome            string    `json:"outcome"`
	Differences        []string  `json:"differences"`
	CandidateError     string    `json:"candidate_error,omitempty"`
	ReceivedAt         time.Time `json:"received_at"`
}

type Stats struct {
	Total                 int64   `json:"total"`
	Matched               int64   `json:"matched"`
	Different             int64   `json:"different"`
	Errors                int64   `json:"errors"`
	Slow                  int64   `json:"slow"`
	AvgStableLatencyMs    float64 `json:"avg_stable_latency_ms"`
	AvgCandidateLatencyMs float64 `json:"avg_candidate_latency_ms"`
}

type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres and retries for a few seconds, since the
// database container is often still starting when the gateway comes up.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	for attempt := 1; ; attempt++ {
		err = pool.Ping(ctx)
		if err == nil {
			return &Store{pool: pool}, nil
		}
		if attempt == 10 {
			pool.Close()
			return nil, fmt.Errorf("ping database: %w", err)
		}
		time.Sleep(time.Second)
	}
}

func (s *Store) Close() {
	s.pool.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) SaveComparison(ctx context.Context, c Comparison) error {
	if c.Differences == nil {
		c.Differences = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO request_comparisons (
			request_id, method, path, query,
			stable_status, candidate_status, stable_latency_ms, candidate_latency_ms,
			stable_body, candidate_body,
			status_match, body_match, candidate_slow, outcome, differences, candidate_error,
			received_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, NULLIF($16, ''), $17)`,
		c.RequestID, c.Method, c.Path, c.Query,
		c.StableStatus, c.CandidateStatus, c.StableLatencyMs, c.CandidateLatencyMs,
		c.StableBody, c.CandidateBody,
		c.StatusMatch, c.BodyMatch, c.CandidateSlow, c.Outcome, c.Differences, c.CandidateError,
		c.ReceivedAt,
	)
	if err != nil {
		return fmt.Errorf("insert comparison %s: %w", c.RequestID, err)
	}
	return nil
}

// ListComparisons returns the most recent comparisons without the response
// bodies. An empty outcome means no filter.
func (s *Store) ListComparisons(ctx context.Context, limit int, outcome string) ([]Comparison, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, request_id, method, path, query,
		       stable_status, candidate_status, stable_latency_ms, candidate_latency_ms,
		       status_match, body_match, candidate_slow, outcome, differences,
		       COALESCE(candidate_error, ''), received_at
		FROM request_comparisons
		WHERE ($1 = '' OR outcome = $1)
		ORDER BY received_at DESC, id DESC
		LIMIT $2`, outcome, limit)
	if err != nil {
		return nil, fmt.Errorf("list comparisons: %w", err)
	}
	defer rows.Close()

	comparisons := []Comparison{}
	for rows.Next() {
		var c Comparison
		err := rows.Scan(&c.ID, &c.RequestID, &c.Method, &c.Path, &c.Query,
			&c.StableStatus, &c.CandidateStatus, &c.StableLatencyMs, &c.CandidateLatencyMs,
			&c.StatusMatch, &c.BodyMatch, &c.CandidateSlow, &c.Outcome, &c.Differences,
			&c.CandidateError, &c.ReceivedAt)
		if err != nil {
			return nil, fmt.Errorf("scan comparison: %w", err)
		}
		comparisons = append(comparisons, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list comparisons: %w", err)
	}
	return comparisons, nil
}

func (s *Store) GetComparison(ctx context.Context, id int64) (Comparison, error) {
	var c Comparison
	err := s.pool.QueryRow(ctx, `
		SELECT id, request_id, method, path, query,
		       stable_status, candidate_status, stable_latency_ms, candidate_latency_ms,
		       stable_body, candidate_body,
		       status_match, body_match, candidate_slow, outcome, differences,
		       COALESCE(candidate_error, ''), received_at
		FROM request_comparisons
		WHERE id = $1`, id).Scan(
		&c.ID, &c.RequestID, &c.Method, &c.Path, &c.Query,
		&c.StableStatus, &c.CandidateStatus, &c.StableLatencyMs, &c.CandidateLatencyMs,
		&c.StableBody, &c.CandidateBody,
		&c.StatusMatch, &c.BodyMatch, &c.CandidateSlow, &c.Outcome, &c.Differences,
		&c.CandidateError, &c.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Comparison{}, ErrNotFound
	}
	if err != nil {
		return Comparison{}, fmt.Errorf("get comparison %d: %w", id, err)
	}
	return c, nil
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	// Candidate latency only counts requests where the candidate actually
	// answered. Timeouts would otherwise just show up as the timeout value.
	err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE outcome = 'match'),
		       count(*) FILTER (WHERE outcome = 'different'),
		       count(*) FILTER (WHERE outcome = 'error'),
		       count(*) FILTER (WHERE candidate_slow),
		       COALESCE(avg(stable_latency_ms), 0),
		       COALESCE(avg(candidate_latency_ms) FILTER (WHERE candidate_error IS NULL), 0)
		FROM request_comparisons`).Scan(
		&st.Total, &st.Matched, &st.Different, &st.Errors, &st.Slow,
		&st.AvgStableLatencyMs, &st.AvgCandidateLatencyMs)
	if err != nil {
		return Stats{}, fmt.Errorf("load stats: %w", err)
	}
	return st, nil
}
