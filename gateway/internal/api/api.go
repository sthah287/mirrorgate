package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"mirrorgate/gateway/internal/proxy"
	"mirrorgate/gateway/internal/sampling"
	"mirrorgate/shared/compare"
	"mirrorgate/shared/storage"
)

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
}

type API struct {
	store      *storage.Store
	gateway    gateway
	sampler    sampler
	deployment Deployment
}

func New(store *storage.Store, gw gateway, sm sampler, deployment Deployment) *API {
	return &API{store: store, gateway: gw, sampler: sm, deployment: deployment}
}

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/health", a.health)
	mux.HandleFunc("GET /internal/stats", a.stats)
	mux.HandleFunc("GET /internal/comparisons", a.listComparisons)
	mux.HandleFunc("GET /internal/comparisons/{id}", a.getComparison)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"deployment": a.deployment,
		"stats":      st,
		"gateway":    a.gateway.Stats(),
		"sampling":   a.sampler.Stats(),
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
