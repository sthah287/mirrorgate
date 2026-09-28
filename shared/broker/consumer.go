package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"

	"mirrorgate/shared/event"
)

type Consumer struct {
	reader *kafka.Reader
}

func NewConsumer(brokers, topic, group string) *Consumer {
	return &Consumer{reader: kafka.NewReader(kafka.ReaderConfig{
		Brokers:  splitBrokers(brokers),
		Topic:    topic,
		GroupID:  group,
		MinBytes: 1,
		MaxBytes: 10 << 20,
		MaxWait:  500 * time.Millisecond,
		// Anything already in the topic when the worker starts still gets
		// processed, which is what makes the "worker was down" case work.
		StartOffset: kafka.FirstOffset,
	})}
}

// Handler processes one event. The offset is only committed when it returns
// nil, so a failed save is retried after a restart instead of being dropped.
type Handler func(ctx context.Context, e event.Comparison) error

// Run reads events until ctx is canceled.
func (c *Consumer) Run(ctx context.Context, handle Handler) error {
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("fetch: %w", err)
		}

		var e event.Comparison
		if err := json.Unmarshal(msg.Value, &e); err != nil {
			// A message we can't parse will never parse. Committing it keeps
			// the group from getting stuck on the same offset forever.
			if commitErr := c.reader.CommitMessages(ctx, msg); commitErr != nil {
				return fmt.Errorf("commit unparseable message: %w", commitErr)
			}
			return fmt.Errorf("decode event at offset %d: %w", msg.Offset, err)
		}

		if err := handle(extractTrace(ctx, msg), e); err != nil {
			return err
		}
		if err := c.reader.CommitMessages(ctx, msg); err != nil {
			return fmt.Errorf("commit offset %d: %w", msg.Offset, err)
		}
	}
}

func (c *Consumer) Close() error {
	return c.reader.Close()
}
