// Package replay records a sample of read-only traffic and sends it to the
// candidate later, away from the request path.
//
// Shadow traffic only ever covers what happens to be arriving. Replay covers
// the rest: a recorded request can be sent at a candidate that was not
// running at the time, or sent again after a fix. The cost of keeping request
// copies around is that they are a small archive of real traffic, so
// recording is deliberately narrow:
//
//   - GET only, enforced here and by a constraint on the table, so a replay
//     cannot charge a card, send an email, or write to the stable
//     application's database.
//   - only the headers on a short allow list are kept. Authorization,
//     cookies, and anything else carrying a credential are never stored, and
//     neither is anything else nobody thought about.
//   - the table has a row cap and a retention window, so it cannot grow
//     without bound or keep traffic indefinitely.
//   - the whole thing is off unless it is turned on.
package replay

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Headers worth keeping to reproduce a GET. An allow list rather than a deny
// list: a header nobody thought about is dropped instead of stored.
var safeHeaders = []string{"Accept", "Accept-Language", "User-Agent"}

type Config struct {
	Enabled bool
	// Rate is the share of eligible GET requests to record.
	Rate float64
	// MaxRecords caps the table. Recording stops at the cap rather than
	// evicting, so a batch is made of requests the sampler chose and not of
	// whatever happened to survive.
	MaxRecords int
	// Retention is how long a recorded request is kept.
	Retention time.Duration
	// MaxURLBytes rejects unusually long urls instead of storing them.
	MaxURLBytes int
	// QueueSize bounds what the recorder holds in memory while the writer is
	// behind. A full queue drops the request rather than slowing the client.
	QueueSize int
}

// Recorded is one captured request. There is no body: GET is the only method
// that gets recorded.
type Recorded struct {
	RequestID string
	Path      string
	Query     string
	Headers   map[string]string
}

type Recorder struct {
	pool      *pgxpool.Pool
	cfg       Config
	randFloat func() float64
	queue     chan Recorded

	recorded    atomic.Int64
	notSampled  atomic.Int64
	droppedFull atomic.Int64
	droppedCap  atomic.Int64
}

func NewRecorder(pool *pgxpool.Pool, cfg Config, randFloat func() float64) *Recorder {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 256
	}
	return &Recorder{
		pool:      pool,
		cfg:       cfg,
		randFloat: randFloat,
		queue:     make(chan Recorded, cfg.QueueSize),
	}
}

// Observe runs in the request path, so it never blocks and never touches the
// database. It decides whether to keep the request and hands it to the writer.
func (rec *Recorder) Observe(r *http.Request, requestID string) {
	if !rec.cfg.Enabled || r.Method != http.MethodGet {
		return
	}
	if len(r.URL.Path)+len(r.URL.RawQuery) > rec.cfg.MaxURLBytes {
		return
	}
	if rec.cfg.Rate < 1 && rec.randFloat() >= rec.cfg.Rate {
		rec.notSampled.Add(1)
		return
	}

	select {
	case rec.queue <- Recorded{
		RequestID: requestID,
		Path:      r.URL.Path,
		Query:     r.URL.RawQuery,
		Headers:   SafeHeaders(r.Header),
	}:
	default:
		rec.droppedFull.Add(1)
	}
}

// SafeHeaders keeps the few headers that change what a GET returns and drops
// everything else, credentials included.
func SafeHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(safeHeaders))
	for _, name := range safeHeaders {
		if v := h.Get(name); v != "" {
			out[name] = v
		}
	}
	return out
}

// Run writes queued requests and ages out old ones. It returns when the
// context is cancelled, after writing what is already queued.
func (rec *Recorder) Run(ctx context.Context) {
	if !rec.cfg.Enabled {
		return
	}
	cleanup := time.NewTicker(rec.cleanupInterval())
	defer cleanup.Stop()

	for {
		select {
		case <-ctx.Done():
			rec.drain()
			return
		case r := <-rec.queue:
			rec.write(ctx, r)
		case <-cleanup.C:
			rec.Cleanup(ctx)
		}
	}
}

func (rec *Recorder) cleanupInterval() time.Duration {
	if rec.cfg.Retention > 0 && rec.cfg.Retention < time.Minute {
		return rec.cfg.Retention
	}
	return time.Minute
}

// drain writes what is already queued at shutdown. Anything still arriving is
// dropped: the queue is a sample, not a ledger.
func (rec *Recorder) drain() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		select {
		case r := <-rec.queue:
			rec.write(ctx, r)
		default:
			return
		}
	}
}

// write enforces the row cap inside the insert, so two writers cannot both
// decide there is room for the same last row.
func (rec *Recorder) write(ctx context.Context, r Recorded) {
	tag, err := rec.pool.Exec(ctx, `
		INSERT INTO recorded_requests (request_id, method, path, query, headers)
		SELECT $1, 'GET', $2, $3, $4
		WHERE (SELECT count(*) FROM recorded_requests) < $5
		ON CONFLICT (request_id) DO NOTHING`,
		r.RequestID, r.Path, r.Query, r.Headers, rec.cfg.MaxRecords)
	if err != nil {
		slog.Warn("record request", "err", err)
		return
	}
	if tag.RowsAffected() == 0 {
		rec.droppedCap.Add(1)
		return
	}
	rec.recorded.Add(1)
}

// Cleanup drops recordings past the retention window.
func (rec *Recorder) Cleanup(ctx context.Context) {
	tag, err := rec.pool.Exec(ctx,
		"DELETE FROM recorded_requests WHERE recorded_at < now() - make_interval(secs => $1)",
		rec.cfg.Retention.Seconds())
	if err != nil {
		slog.Warn("recorded request cleanup", "err", err)
		return
	}
	if tag.RowsAffected() > 0 {
		slog.Info("recorded request cleanup", "deleted", tag.RowsAffected())
	}
}

type Stats struct {
	Enabled          bool    `json:"enabled"`
	Rate             float64 `json:"rate"`
	MaxRecords       int     `json:"max_records"`
	RetentionSeconds float64 `json:"retention_seconds"`
	Recorded         int64   `json:"recorded"`
	NotSampled       int64   `json:"not_sampled"`
	DroppedQueueFull int64   `json:"dropped_queue_full"`
	DroppedAtCap     int64   `json:"dropped_at_cap"`
	Stored           int     `json:"stored"`
}

func (rec *Recorder) Stats(ctx context.Context) Stats {
	st := Stats{
		Enabled:          rec.cfg.Enabled,
		Rate:             rec.cfg.Rate,
		MaxRecords:       rec.cfg.MaxRecords,
		RetentionSeconds: rec.cfg.Retention.Seconds(),
		Recorded:         rec.recorded.Load(),
		NotSampled:       rec.notSampled.Load(),
		DroppedQueueFull: rec.droppedFull.Load(),
		DroppedAtCap:     rec.droppedCap.Load(),
	}
	if err := rec.pool.QueryRow(ctx, "SELECT count(*) FROM recorded_requests").Scan(&st.Stored); err != nil {
		slog.Warn("recorded request count", "err", err)
	}
	return st
}

// EnsureSchema creates the recording tables. The comparison tables come from
// db/init.sql, which only runs the first time the database volume is created,
// so these are created at startup the way the two app services create theirs.
func EnsureSchema(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, strings.Join([]string{
		// The method is constrained rather than merely documented: a replay
		// must not be able to repeat a write even if some later change
		// starts recording one.
		`CREATE TABLE IF NOT EXISTS recorded_requests (
			id          BIGSERIAL PRIMARY KEY,
			request_id  TEXT NOT NULL UNIQUE,
			method      TEXT NOT NULL DEFAULT 'GET' CHECK (method = 'GET'),
			path        TEXT NOT NULL,
			query       TEXT NOT NULL DEFAULT '',
			headers     JSONB NOT NULL DEFAULT '{}',
			recorded_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_recorded_requests_recorded_at
			ON recorded_requests (recorded_at DESC)`,
		// Replay outcomes stay out of request_comparisons: a replay is not a
		// comparison between the two services, and mixing them would change
		// what every earlier measurement counted.
		`CREATE TABLE IF NOT EXISTS replay_results (
			id          BIGSERIAL PRIMARY KEY,
			batch_id    TEXT NOT NULL,
			request_id  TEXT NOT NULL,
			path        TEXT NOT NULL,
			query       TEXT NOT NULL DEFAULT '',
			status      INTEGER,
			latency_ms  DOUBLE PRECISION NOT NULL DEFAULT 0,
			error       TEXT,
			replayed_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_replay_results_batch
			ON replay_results (batch_id, replayed_at DESC)`,
	}, ";\n"))
	return err
}
