package replay

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrBusy is returned when a replay is already running. Two batches at once
// would double the load the candidate sees from a feature whose whole point
// is to be a controlled amount of extra traffic.
var ErrBusy = errors.New("a replay is already running")

// ErrDisabled is returned when no token is configured. Replay fails closed:
// an operator who has not set a token has not turned replay on.
var ErrDisabled = errors.New("replay is not configured")

// How much of a replayed response is read before giving up on it. The body
// is never stored and never returned, but it has to be drained for the
// connection to be reused.
const maxReplayBody = 1 << 20

type RunnerConfig struct {
	// Token has to be presented to trigger a replay. Empty disables the
	// trigger completely.
	Token string
	// MaxBatch caps one replay, whatever the caller asks for.
	MaxBatch int
	// Timeout is per replayed request.
	Timeout time.Duration
}

// Runner sends recorded requests at the candidate.
//
// It holds the candidate's base url and nothing else, and it builds every
// request from a row it read out of recorded_requests. There is no code path
// that takes a target from the caller, so the trigger cannot be turned into a
// general request forwarder, and there is no way for it to reach the stable
// service.
type Runner struct {
	pool      *pgxpool.Pool
	candidate *url.URL
	client    *http.Client
	cfg       RunnerConfig
	running   atomic.Bool
}

func NewRunner(pool *pgxpool.Pool, candidateURL string, cfg RunnerConfig) (*Runner, error) {
	u, err := url.Parse(candidateURL)
	if err != nil {
		return nil, fmt.Errorf("candidate url: %w", err)
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 100
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &Runner{
		pool:      pool,
		candidate: u,
		client:    &http.Client{Timeout: cfg.Timeout},
		cfg:       cfg,
	}, nil
}

// Authorized reports whether a presented token matches the configured one.
// An unset token means replay is off, so nothing is authorized.
func (run *Runner) Authorized(presented string) bool {
	if run.cfg.Token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(run.cfg.Token)) == 1
}

func (run *Runner) Enabled() bool { return run.cfg.Token != "" }

func (run *Runner) MaxBatch() int { return run.cfg.MaxBatch }

// Summary is what the trigger gets back. Statuses and timings only: a
// replayed response body is never returned to anyone.
type Summary struct {
	BatchID      string         `json:"batch_id"`
	Requested    int            `json:"requested"`
	Replayed     int            `json:"replayed"`
	Failed       int            `json:"failed"`
	StatusCounts map[string]int `json:"status_counts"`
	DurationMs   float64        `json:"duration_ms"`
}

// Run replays up to limit recorded requests, newest first, and writes what
// happened to replay_results.
func (run *Runner) Run(ctx context.Context, limit int) (Summary, error) {
	if !run.Enabled() {
		return Summary{}, ErrDisabled
	}
	if limit <= 0 || limit > run.cfg.MaxBatch {
		limit = run.cfg.MaxBatch
	}
	if !run.running.CompareAndSwap(false, true) {
		return Summary{}, ErrBusy
	}
	defer run.running.Store(false)

	recorded, err := run.load(ctx, limit)
	if err != nil {
		return Summary{}, err
	}

	started := time.Now()
	summary := Summary{
		BatchID:      "RB" + rand.Text()[:10],
		Requested:    len(recorded),
		StatusCounts: map[string]int{},
	}
	for _, rec := range recorded {
		status, latency, replayErr := run.send(ctx, rec)
		if replayErr != nil {
			summary.Failed++
			summary.StatusCounts["error"]++
		} else {
			summary.Replayed++
			summary.StatusCounts[fmt.Sprintf("%d", status)]++
		}
		run.record(ctx, summary.BatchID, rec, status, latency, replayErr)
	}
	summary.DurationMs = float64(time.Since(started).Microseconds()) / 1000
	return summary, nil
}

type stored struct {
	Recorded
	Method string
}

func (run *Runner) load(ctx context.Context, limit int) ([]stored, error) {
	rows, err := run.pool.Query(ctx, `
		SELECT request_id, method, path, query, headers
		FROM recorded_requests
		ORDER BY recorded_at DESC, id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("load recorded requests: %w", err)
	}
	defer rows.Close()

	var out []stored
	for rows.Next() {
		var s stored
		if err := rows.Scan(&s.RequestID, &s.Method, &s.Path, &s.Query, &s.Headers); err != nil {
			return nil, fmt.Errorf("scan recorded request: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// send builds the request from the stored row and the configured candidate.
// The method check is the third place GET is enforced, after the recorder and
// the table constraint, because this is the line that would do the damage.
func (run *Runner) send(ctx context.Context, rec stored) (int, float64, error) {
	if rec.Method != http.MethodGet {
		return 0, 0, fmt.Errorf("refusing to replay a %s request", rec.Method)
	}

	target := *run.candidate
	target.Path = strings.TrimRight(run.candidate.Path, "/") + rec.Path
	target.RawQuery = rec.Query

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return 0, 0, err
	}
	for name, value := range rec.Headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("X-MirrorGate-Shadow", "true")
	req.Header.Set("X-MirrorGate-Replay", "true")
	req.Header.Set("X-MirrorGate-Replay-Of", rec.RequestID)

	started := time.Now()
	resp, err := run.client.Do(req)
	latency := float64(time.Since(started).Microseconds()) / 1000
	if err != nil {
		return 0, latency, err
	}
	defer resp.Body.Close()
	// Drained and thrown away. The response of a replay is never compared,
	// never stored, and never sent to a client.
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxReplayBody))
	return resp.StatusCode, latency, nil
}

func (run *Runner) record(ctx context.Context, batchID string, rec stored, status int, latency float64, replayErr error) {
	var statusValue any
	if status > 0 {
		statusValue = status
	}
	var errText any
	if replayErr != nil {
		errText = truncate(replayErr.Error(), 500)
	}
	// Detached from the caller: a trigger that hangs up mid-batch should not
	// also lose the record of what had already been sent.
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := run.pool.Exec(storeCtx, `
		INSERT INTO replay_results (batch_id, request_id, path, query, status, latency_ms, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		batchID, rec.RequestID, rec.Path, rec.Query, statusValue, latency, errText)
	if err != nil {
		// Logged rather than returned: the rest of the batch should still run.
		slog.Warn("store replay result", "err", err)
	}
}

// Result is one replayed request, for the internal API.
type Result struct {
	BatchID    string    `json:"batch_id"`
	RequestID  string    `json:"request_id"`
	Path       string    `json:"path"`
	Query      string    `json:"query,omitempty"`
	Status     *int      `json:"status"`
	LatencyMs  float64   `json:"latency_ms"`
	Error      *string   `json:"error,omitempty"`
	ReplayedAt time.Time `json:"replayed_at"`
}

// Results lists recent replay outcomes, newest first, optionally for one
// batch.
func (run *Runner) Results(ctx context.Context, batchID string, limit int) ([]Result, error) {
	rows, err := run.pool.Query(ctx, `
		SELECT batch_id, request_id, path, query, status, latency_ms, error, replayed_at
		FROM replay_results
		WHERE $1 = '' OR batch_id = $1
		ORDER BY replayed_at DESC, id DESC
		LIMIT $2`, batchID, limit)
	if err != nil {
		return nil, fmt.Errorf("list replay results: %w", err)
	}
	defer rows.Close()

	out := []Result{}
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.BatchID, &r.RequestID, &r.Path, &r.Query,
			&r.Status, &r.LatencyMs, &r.Error, &r.ReplayedAt); err != nil {
			return nil, fmt.Errorf("scan replay result: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
