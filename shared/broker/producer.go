// Package broker wraps the Kafka client so the gateway and the comparison
// worker agree on message format, headers, and topic without each one
// repeating the setup.
package broker

import (
	"context"
	"encoding/json"
	"errors"
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

// StoredMessage is one outbox row on its way to the topic. The traceparent
// travels with the message rather than coming from the caller's context,
// because a batch holds events from many different requests and each one
// belongs to its own trace.
type StoredMessage struct {
	RequestID   string
	Payload     []byte
	Traceparent string
}

// PublishStoredBatch sends a whole batch in one call and reports which
// messages the broker acknowledged.
//
// The relay used to publish one message per call, and kafka-go waits out the
// writer's batch timeout before flushing a single message, so the cost per
// event was roughly that timeout. Handing it the whole batch lets it send one
// request instead of N.
//
// Partial failures matter here: an event marked published when the broker
// never took it would be lost. kafka-go reports one error per message when
// several are written together, so that case returns exactly the ids that
// were acknowledged. If the failure arrives without per-message detail there
// is no way to tell which ones landed, so none are reported as acknowledged
// and the whole batch is retried. The worker drops the duplicates.
func (p *Producer) PublishStoredBatch(ctx context.Context, msgs []StoredMessage) ([]string, error) {
	if len(msgs) == 0 {
		return nil, nil
	}

	kmsgs := make([]kafka.Message, len(msgs))
	for i, m := range msgs {
		km := kafka.Message{Key: []byte(m.RequestID), Value: m.Payload}
		if m.Traceparent != "" {
			km.Headers = append(km.Headers, kafka.Header{Key: "traceparent", Value: []byte(m.Traceparent)})
		}
		kmsgs[i] = km
	}

	err := p.writer.WriteMessages(ctx, kmsgs...)
	if err == nil {
		acked := make([]string, len(msgs))
		for i, m := range msgs {
			acked[i] = m.RequestID
		}
		return acked, nil
	}

	var perMessage kafka.WriteErrors
	if errors.As(err, &perMessage) {
		var acked []string
		var firstErr error
		for i := range msgs {
			if i >= len(perMessage) {
				break
			}
			if perMessage[i] == nil {
				acked = append(acked, msgs[i].RequestID)
			} else if firstErr == nil {
				firstErr = perMessage[i]
			}
		}
		if firstErr == nil {
			firstErr = err
		}
		return acked, fmt.Errorf("publish batch of %d, %d acknowledged: %w", len(msgs), len(acked), firstErr)
	}

	return nil, fmt.Errorf("publish batch of %d, acknowledgement unknown: %w", len(msgs), err)
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
