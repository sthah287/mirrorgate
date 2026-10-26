package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"mirrorgate/outbox-relay/internal/relay"
	"mirrorgate/shared/broker"
	"mirrorgate/shared/event"
	"mirrorgate/shared/outbox"
	"mirrorgate/shared/tracing"
)

func main() {
	if err := run(); err != nil {
		slog.Error("outbox relay stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := tracing.Init(ctx, "outbox-relay")
	if err != nil {
		return err
	}
	defer shutdownTracing(context.Background())

	pool, err := openPool(ctx, env("DATABASE_URL", "postgres://mirrorgate:mirrorgate@localhost:5433/mirrorgate?sslmode=disable"))
	if err != nil {
		return err
	}
	defer pool.Close()

	brokers := env("KAFKA_BROKERS", "localhost:19092")
	topic := env("KAFKA_TOPIC", event.Topic)
	producer := broker.NewProducer(brokers, topic)
	defer producer.Close()

	cfg := relay.Config{
		BatchSize:      intEnv("OUTBOX_BATCH_SIZE", 100),
		PollInterval:   durationEnv("OUTBOX_POLL_INTERVAL", 200*time.Millisecond),
		MaxBackoff:     durationEnv("OUTBOX_MAX_BACKOFF", 10*time.Second),
		Retention:      durationEnv("OUTBOX_RETENTION", time.Hour),
		PublishTimeout: durationEnv("OUTBOX_PUBLISH_TIMEOUT", 10*time.Second),
	}

	slog.Info("relaying outbox events", "brokers", brokers, "topic", topic,
		"batch", cfg.BatchSize, "poll", cfg.PollInterval, "retention", cfg.Retention)

	return relay.New(outbox.New(pool), producer, cfg).Run(ctx)
}

// The database container is usually still starting when this runs.
func openPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	var pingErr error
	for attempt := 1; attempt <= 15; attempt++ {
		if pingErr = pool.Ping(ctx); pingErr == nil {
			return pool, nil
		}
		time.Sleep(time.Second)
	}
	pool.Close()
	return nil, pingErr
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func intEnv(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		slog.Warn("ignoring unparseable setting", "key", key, "value", v)
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		slog.Warn("ignoring unparseable setting", "key", key, "value", v)
	}
	return fallback
}
