package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"mirrorgate/gateway/internal/proxy"
	"mirrorgate/gateway/internal/replay"
	"mirrorgate/gateway/internal/sampling"
	"mirrorgate/shared/compare"
	"mirrorgate/shared/outbox"
	"mirrorgate/shared/storage"
)

const maxExplainedEndpoints = 50

type Deployment struct {
	StableVersion    string `json:"stable_version"`
	CandidateVersion string `json:"candidate_version"`
	StableURL        string `json:"stable_url"`
	CandidateURL     string `json:"candidate_url"`
}

// gateway exposes the counters the database can't answer: how much traffic
// came in and how much of it was actually mirrored.
type gateway interface {
	Stats() proxy.Stats
}

type sampler interface {
	Stats() sampling.Stats
	Explain() []sampling.EndpointRate
}

// outboxStats is how far behind the relay is. Pending depth and the age of
// the oldest unpublished row are what show a broker outage as a delay.
type outboxStats interface {
	Stats(ctx context.Context) (outbox.Stats, error)
}

// Recorder reports how much read-only traffic has been kept for replay. Nil
// unless recording is turned on.
type Recorder interface {
	Stats(ctx context.Context) replay.Stats
}

// Replayer sends recorded requests at the candidate. Nil unless replay is
// configured. The interface has no way to pass a target: the runner builds
// every request from a row it stored itself.
type Replayer interface {
	Enabled() bool
	Authorized(token string) bool
	MaxBatch() int
	Run(ctx context.Context, limit int) (replay.Summary, error)
	Results(ctx context.Context, batchID string, limit int) ([]replay.Result, error)
}

type API struct {
	store      *storage.Store
	gateway    gateway
	sampler    sampler
	outbox     outboxStats
	recorder   Recorder
	replayer   Replayer
	deployment Deployment
}

func New(store *storage.Store, gw gateway, sm sampler, ob outboxStats,
	rec Recorder, rp Replayer, deployment Deployment) *API {
	return &API{store: store, gateway: gw, sampler: sm, outbox: ob,
		recorder: rec, replayer: rp, deployment: deployment}
}

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/health", a.health)
	mux.HandleFunc("GET /internal/stats", a.stats)
	mux.HandleFunc("GET /internal/sampling", a.sampling)
	mux.HandleFunc("GET /internal/comparisons", a.listComparisons)
	mux.HandleFunc("GET /internal/comparisons/{id}", a.getComparison)
	mux.HandleFunc("POST /internal/replay", a.startReplay)
	mux.HandleFunc("GET /internal/replay/results", a.replayResults)
	return mux
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "database unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	st, err := a.store.Stats(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	ob, err := a.outbox.Stats(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	body := map[string]any{
		"deployment": a.deployment,
		"stats":      st,
		"gateway":    a.gateway.Stats(),
		"sampling":   a.sampler.Stats(),
		"outbox":     ob,
	}
	if a.recorder != nil {
		body["recording"] = a.recorder.Stats(r.Context())
	}
	writeJSON(w, http.StatusOK, body)
}

// sampling explains the current per-endpoint rates. It is empty unless the
// gateway is running in risk mode, where the rate moves on its own.
func (a *API) sampling(w http.ResponseWriter, r *http.Request) {
	endpoints := a.sampler.Explain()
	if endpoints == nil {
		endpoints = []sampling.EndpointRate{}
	}
	// Explain is sorted by rate, so the interesting endpoints are at the
	// front. Capped because a service with ids in the path has one entry per
	// id and the dashboard only shows the top few anyway.
	if len(endpoints) > maxExplainedEndpoints {
		endpoints = endpoints[:maxExplainedEndpoints]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sampling":  a.sampler.Stats(),
		"endpoints": endpoints,
	})
}

func (a *API) listComparisons(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be between 1 and 500"})
			return
		}
		limit = n
	}

	outcome := r.URL.Query().Get("outcome")
	switch outcome {
	case "", compare.Match, compare.Different, compare.Error:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "outcome must be match, different, or error"})
		return
	}

	comparisons, err := a.store.ListComparisons(r.Context(), limit, outcome)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, comparisons)
}

func (a *API) getComparison(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	c, err := a.store.GetComparison(r.Context(), id)
	if errors.Is(err, storage.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "comparison not found"})
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// startReplay sends a batch of recorded GETs at the candidate.
//
// The caller chooses how many, and nothing else. There is no target in the
// request, so this cannot be used to make the gateway fetch an arbitrary url,
// and the runner only ever talks to the candidate. It needs a token even
// though it lives on the internal port, because the internal port is reachable
// from a browser on the same machine as the dashboard.
func (a *API) startReplay(w http.ResponseWriter, r *http.Request) {
	if a.replayer == nil || !a.replayer.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "replay is not configured"})
		return
	}
	if !a.replayer.Authorized(r.Header.Get("X-Replay-Token")) {
		slog.Warn("rejected replay trigger", "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	limit := a.replayer.MaxBatch()
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > a.replayer.MaxBatch() {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("limit must be between 1 and %d", a.replayer.MaxBatch()),
			})
			return
		}
		limit = n
	}

	summary, err := a.replayer.Run(r.Context(), limit)
	if errors.Is(err, replay.ErrBusy) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// replayResults lists what came back, which is statuses and timings. The
// replayed bodies are not stored and are not available here.
func (a *API) replayResults(w http.ResponseWriter, r *http.Request) {
	if a.replayer == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "replay is not configured"})
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be between 1 and 500"})
			return
		}
		limit = n
	}

	results, err := a.replayer.Results(r.Context(), r.URL.Query().Get("batch"), limit)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, results)
}

func serverError(w http.ResponseWriter, err error) {
	slog.Error("internal api", "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("encode response", "err", err)
	}
}
