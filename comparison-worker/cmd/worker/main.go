package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"mirrorgate/comparison-worker/internal/consume"
	"mirrorgate/shared/broker"
	"mirrorgate/shared/event"
	"mirrorgate/shared/storage"
	"mirrorgate/shared/tracing"
)

func main() {
	if err := run(); err != nil {
		slog.Error("comparison worker stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := tracing.Init(ctx, "comparison-worker")
	if err != nil {
		return err
	}
	defer shutdownTracing(context.Background())

	store, err := storage.Open(ctx, env("DATABASE_URL", "postgres://mirrorgate:mirrorgate@localhost:5433/mirrorgate?sslmode=disable"))
	if err != nil {
		return err
	}
	defer store.Close()

	brokers := env("KAFKA_BROKERS", "localhost:19092")
	topic := env("KAFKA_TOPIC", event.Topic)
	group := env("KAFKA_GROUP", "mirrorgate-comparison-worker")

	if err := broker.EnsureTopic(ctx, brokers, topic, 1); err != nil {
		return err
	}
	consumer := broker.NewConsumer(brokers, topic, group)
	defer consumer.Close()

	handler := consume.New(store)
	slog.Info("consuming comparison events", "brokers", brokers, "topic", topic, "group", group)

	if err := consumer.Run(ctx, handler.Handle); err != nil {
		return err
	}
	slog.Info("shutting down")
	return nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
