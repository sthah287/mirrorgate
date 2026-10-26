package main

import (
	"context"
	"errors"
	"fmt"
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
	"mirrorgate/shared/outbox"
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
	riskWindow, err := time.ParseDuration(env("RISK_WINDOW", "10m"))
	if err != nil {
		return errors.New("RISK_WINDOW must be a duration like 10m")
	}
	riskRefresh, err := time.ParseDuration(env("RISK_REFRESH", "5s"))
	if err != nil {
		return errors.New("RISK_REFRESH must be a duration like 5s")
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

	// outbox is the real path: the event is written to Postgres and the relay
	// moves it to the topic, so a broker outage delays comparisons instead of
	// losing them. direct is the old behaviour, kept only so the experiment
	// can measure the difference between the two.
	publishMode := env("PUBLISH_MODE", "outbox")
	outboxStore := outbox.New(store.Pool())
	var publisher proxy.Publisher
	switch publishMode {
	case "outbox":
		publisher = outboxStore
	case "direct":
		producer := broker.NewProducer(brokers, topic)
		defer producer.Close()
		publisher = producer
	default:
		return fmt.Errorf("PUBLISH_MODE must be outbox or direct, got %q", publishMode)
	}

	mirror, err := proxy.New(cfg, publisher, sampler)
	if err != nil {
		return err
	}

	internalAPI := api.New(store, mirror, sampler, outboxStore, api.Deployment{
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

	sampleStats := sampler.Stats()
	// Started before the servers so the first read of the risk signals
	// happens before any traffic arrives.
	if sampleStats.Mode == sampling.Risk {
		go refreshRisk(ctx, store, sampler, riskWindow, riskRefresh)
	}

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
		"shadow_timeout", cfg.ShadowTimeout, "methods", cfg.MirrorMethods,
		"sampling_mode", sampleStats.Mode, "sampling_rate", sampleStats.Rate,
		"publish_mode", publishMode)

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

// refreshRisk keeps risk-priority sampling up to date with what the worker
// has stored. The gateway can't score an endpoint from its own memory,
// because it never sees the comparison result; that happens downstream.
func refreshRisk(ctx context.Context, store *storage.Store, sampler *sampling.Sampler, window, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	// The signals come from Postgres, but the traffic counts behind the
	// rare-endpoint rule are the gateway's own, so they have to be aged out
	// here or an endpoint that was busy once stays busy forever.
	rotate := time.NewTicker(window)
	defer rotate.Stop()

	for {
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		rows, err := store.RecentRisk(readCtx, window)
		cancel()
		if err != nil {
			slog.Warn("refresh risk signals", "err", err)
		} else {
			signals := make(map[string]sampling.Signal, len(rows))
			for _, r := range rows {
				signals[strings.ToUpper(r.Method)+" "+r.Path] = sampling.Signal{
					Mismatches: r.Mismatches,
					Errors:     r.Errors,
					Slow:       r.Slow,
				}
			}
			sampler.UpdateSignals(signals)
		}

		select {
		case <-ctx.Done():
			return
		case <-rotate.C:
			sampler.RotateTraffic()
		case <-ticker.C:
		}
	}
}
