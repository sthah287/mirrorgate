package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"
)

// EnsureTopic creates the topic if it isn't there yet and waits for the
// broker to become reachable. Auto-creation is on in development, but doing
// it up front means the worker doesn't sit in a "topic not found" loop when
// it starts before any traffic has arrived.
//
// A topic that already exists with fewer partitions is grown rather than left
// alone, so the number of comparison workers that can run at once is a
// setting rather than something fixed on the day the topic was first created.
// Growing changes which partition a key lands on from then on, which would
// matter if ordering per key mattered; here each request id produces exactly
// one event and the worker's insert is idempotent, so it does not.
func EnsureTopic(ctx context.Context, brokers, topic string, partitions int) error {
	client := &kafka.Client{Addr: kafka.TCP(splitBrokers(brokers)...), Timeout: 10 * time.Second}
	config := kafka.TopicConfig{Topic: topic, NumPartitions: partitions, ReplicationFactor: 1}

	var lastErr error
	for attempt := 1; attempt <= 10; attempt++ {
		resp, err := client.CreateTopics(ctx, &kafka.CreateTopicsRequest{Topics: []kafka.TopicConfig{config}})
		if err == nil {
			topicErr := resp.Errors[topic]
			if topicErr != nil && !errors.Is(topicErr, kafka.TopicAlreadyExists) {
				return fmt.Errorf("create topic %s: %w", topic, topicErr)
			}
			if topicErr == nil {
				return nil
			}
			return growPartitions(ctx, client, topic, partitions)
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("create topic %s: %w", topic, lastErr)
}

// Partitions reports how many partitions a topic currently has.
func Partitions(ctx context.Context, brokers, topic string) (int, error) {
	client := &kafka.Client{Addr: kafka.TCP(splitBrokers(brokers)...), Timeout: 10 * time.Second}
	return currentPartitions(ctx, client, topic)
}

func growPartitions(ctx context.Context, client *kafka.Client, topic string, want int) error {
	have, err := currentPartitions(ctx, client, topic)
	if err != nil {
		return err
	}
	if have >= want {
		return nil
	}

	resp, err := client.CreatePartitions(ctx, &kafka.CreatePartitionsRequest{
		Topics: []kafka.TopicPartitionsConfig{{Name: topic, Count: int32(want)}},
	})
	if err != nil {
		return fmt.Errorf("grow %s to %d partitions: %w", topic, want, err)
	}
	if topicErr := resp.Errors[topic]; topicErr != nil {
		return fmt.Errorf("grow %s to %d partitions: %w", topic, want, topicErr)
	}
	slog.Info("grew topic", "topic", topic, "from", have, "to", want)
	return nil
}

func currentPartitions(ctx context.Context, client *kafka.Client, topic string) (int, error) {
	meta, err := client.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{topic}})
	if err != nil {
		return 0, fmt.Errorf("read metadata for %s: %w", topic, err)
	}
	for _, t := range meta.Topics {
		if t.Name != topic {
			continue
		}
		if t.Error != nil {
			return 0, fmt.Errorf("read metadata for %s: %w", topic, t.Error)
		}
		return len(t.Partitions), nil
	}
	return 0, fmt.Errorf("topic %s is missing from the broker metadata", topic)
}
