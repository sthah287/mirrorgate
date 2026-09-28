package broker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
)

// EnsureTopic creates the topic if it isn't there yet and waits for the
// broker to become reachable. Auto-creation is on in development, but doing
// it up front means the worker doesn't sit in a "topic not found" loop when
// it starts before any traffic has arrived.
func EnsureTopic(ctx context.Context, brokers, topic string, partitions int) error {
	client := &kafka.Client{Addr: kafka.TCP(splitBrokers(brokers)...), Timeout: 10 * time.Second}
	config := kafka.TopicConfig{Topic: topic, NumPartitions: partitions, ReplicationFactor: 1}

	var lastErr error
	for attempt := 1; attempt <= 10; attempt++ {
		resp, err := client.CreateTopics(ctx, &kafka.CreateTopicsRequest{Topics: []kafka.TopicConfig{config}})
		if err == nil {
			if topicErr := resp.Errors[topic]; topicErr != nil && !errors.Is(topicErr, kafka.TopicAlreadyExists) {
				return fmt.Errorf("create topic %s: %w", topic, topicErr)
			}
			return nil
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
