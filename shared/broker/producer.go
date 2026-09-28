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
		Balancer:               &kafka.Hash{},
		RequiredAcks:           kafka.RequireOne,
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
