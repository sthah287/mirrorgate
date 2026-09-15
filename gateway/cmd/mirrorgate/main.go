package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mirrorgate/gateway/internal/api"
	"mirrorgate/gateway/internal/proxy"
	"mirrorgate/gateway/internal/storage"
)

func main() {
	if err := run(); err != nil {
		slog.Error("mirrorgate stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	shadowTimeout, err := time.ParseDuration(env("SHADOW_TIMEOUT", "2s"))
	if err != nil {
		return errors.New("SHADOW_TIMEOUT must be a duration like 2s or 500ms")
	}
	maxInFlight, err := strconv.Atoi(env("MAX_SHADOW_IN_FLIGHT", "100"))
	if err != nil {
		return errors.New("MAX_SHADOW_IN_FLIGHT must be a number")
	}

	cfg := proxy.Config{
		StableURL:     env("STABLE_URL", "http://localhost:9001"),
		CandidateURL:  env("CANDIDATE_URL", "http://localhost:9002"),
		ShadowTimeout: shadowTimeout,
		MirrorMethods: strings.Split(env("MIRROR_METHODS", "GET"), ","),
		MaxInFlight:   maxInFlight,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := storage.Open(ctx, env("DATABASE_URL", "postgres://mirrorgate:mirrorgate@localhost:5433/mirrorgate?sslmode=disable"))
	if err != nil {
		return err
	}
	defer store.Close()

	mirror, err := proxy.New(cfg, store)
	if err != nil {
		return err
	}

	internalAPI := api.New(store, api.Deployment{
		StableVersion:    env("STABLE_VERSION", "v1"),
		CandidateVersion: env("CANDIDATE_VERSION", "v2"),
		StableURL:        cfg.StableURL,
		CandidateURL:     cfg.CandidateURL,
	})

	// The internal API is on its own port so it can't collide with paths
	// that belong to the service being mirrored.
	proxyServer := &http.Server{Addr: ":" + env("PORT", "8080"), Handler: mirror, ReadHeaderTimeout: 5 * time.Second}
	adminServer := &http.Server{Addr: ":" + env("ADMIN_PORT", "8081"), Handler: internalAPI.Routes(), ReadHeaderTimeout: 5 * time.Second}

	errs := make(chan error, 2)
	for _, srv := range []*http.Server{proxyServer, adminServer} {
		go func() {
			slog.Info("listening", "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}()
	}
	slog.Info("mirroring traffic", "stable", cfg.StableURL, "candidate", cfg.CandidateURL,
		"shadow_timeout", cfg.ShadowTimeout, "methods", cfg.MirrorMethods)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := proxyServer.Shutdown(shutdownCtx); err != nil {
		slog.Warn("proxy shutdown", "err", err)
	}
	// Let shadow requests that are already running finish and get saved.
	if err := mirror.Wait(shutdownCtx); err != nil {
		slog.Warn("shadow requests still running at shutdown", "err", err)
	}
	return adminServer.Shutdown(shutdownCtx)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
