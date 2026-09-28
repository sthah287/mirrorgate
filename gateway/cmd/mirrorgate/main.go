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

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"mirrorgate/gateway/internal/api"
	"mirrorgate/gateway/internal/proxy"
	"mirrorgate/gateway/internal/sampling"
	"mirrorgate/shared/broker"
	"mirrorgate/shared/event"
	"mirrorgate/shared/storage"
	"mirrorgate/shared/tracing"
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

	sampler, err := sampling.Parse(os.Getenv("SAMPLING_MODE"), os.Getenv("SAMPLING_RATE"), os.Getenv("SAMPLING_RULES"))
	if err != nil {
		return err
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

	shutdownTracing, err := tracing.Init(ctx, "gateway")
	if err != nil {
		return err
	}
	defer shutdownTracing(context.Background())

	// The gateway still reads from Postgres for the dashboard API, but it no
	// longer writes comparisons. Only the worker does that now.
	store, err := storage.Open(ctx, env("DATABASE_URL", "postgres://mirrorgate:mirrorgate@localhost:5433/mirrorgate?sslmode=disable"))
	if err != nil {
		return err
	}
	defer store.Close()

	brokers := env("KAFKA_BROKERS", "localhost:19092")
	topic := env("KAFKA_TOPIC", event.Topic)
	if err := broker.EnsureTopic(ctx, brokers, topic, 1); err != nil {
		return err
	}
	producer := broker.NewProducer(brokers, topic)
	defer producer.Close()

	mirror, err := proxy.New(cfg, producer, sampler)
	if err != nil {
		return err
	}

	internalAPI := api.New(store, mirror, sampler, api.Deployment{
		StableVersion:    env("STABLE_VERSION", "v1"),
		CandidateVersion: env("CANDIDATE_VERSION", "v2"),
		StableURL:        cfg.StableURL,
		CandidateURL:     cfg.CandidateURL,
	})

	// The internal API is on its own port so it can't collide with paths
	// that belong to the service being mirrored.
	// otelhttp starts the trace for every incoming request; everything the
	// gateway does for that request hangs off this span.
	handler := otelhttp.NewHandler(mirror, "mirrorgate")
	proxyServer := &http.Server{Addr: ":" + env("PORT", "8080"), Handler: handler, ReadHeaderTimeout: 5 * time.Second}
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
	sampleStats := sampler.Stats()
	slog.Info("mirroring traffic", "stable", cfg.StableURL, "candidate", cfg.CandidateURL,
		"shadow_timeout", cfg.ShadowTimeout, "methods", cfg.MirrorMethods,
		"sampling_mode", sampleStats.Mode, "sampling_rate", sampleStats.Rate)

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
	// Let shadow requests that are already running finish and get published.
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
