// Package broker wraps the Kafka client so the gateway and the comparison
// worker agree on message format, headers, and topic without each one
// repeating the setup.
package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"

	"mirrorgate/shared/event"
)

type Producer struct {
	writer *kafka.Writer
}

func NewProducer(brokers string, topic string) *Producer {
	return &Producer{writer: &kafka.Writer{
		Addr:  kafka.TCP(splitBrokers(brokers)...),
		Topic: topic,
		// Keying by request id spreads events over partitions while keeping
		// retries of the same request on one partition.
		Balancer: &kafka.Hash{},
		// Wait for all in-sync replicas. With the single-broker dev cluster
		// this is the same as RequireOne, but the outbox only promises
		// delivery once the broker has acknowledged, so ask for the strongest
		// ack this setup can give.
		RequiredAcks:           kafka.RequireAll,
		AllowAutoTopicCreation: true,
		BatchTimeout:           50 * time.Millisecond,
	}}
}

// Publish sends one comparison event. It blocks until the broker acks, so
// callers must run it off the request path.
func (p *Producer) Publish(ctx context.Context, e event.Comparison) error {
	value, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}

	msg := kafka.Message{Key: []byte(e.RequestID), Value: value}
	injectTrace(ctx, &msg)
	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", e.RequestID, err)
	}
	return nil
}

// PublishStored sends a payload that was serialized earlier. The trace
// context comes from ctx, which the relay rebuilds from the traceparent it
// saved with the row, so the message still belongs to the original request's
// trace even though the publish happens much later.
func (p *Producer) PublishStored(ctx context.Context, requestID string, payload []byte) error {
	msg := kafka.Message{Key: []byte(requestID), Value: payload}
	injectTrace(ctx, &msg)
	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", requestID, err)
	}
	return nil
}

func (p *Producer) Close() error {
	return p.writer.Close()
}

func splitBrokers(brokers string) []string {
	var out []string
	for _, b := range strings.Split(brokers, ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return out
}
