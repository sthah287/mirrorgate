package broker

import (
	"context"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
)

// Kafka breaks the normal chain of HTTP calls, so the trace context is
// copied into message headers on the way out and read back on the way in.
// Without this the worker's spans start a brand new trace.
type headerCarrier struct {
	msg *kafka.Message
}

func (c headerCarrier) Get(key string) string {
	for _, h := range c.msg.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func (c headerCarrier) Set(key, value string) {
	for i, h := range c.msg.Headers {
		if h.Key == key {
			c.msg.Headers[i].Value = []byte(value)
			return
		}
	}
	c.msg.Headers = append(c.msg.Headers, kafka.Header{Key: key, Value: []byte(value)})
}

func (c headerCarrier) Keys() []string {
	keys := make([]string, 0, len(c.msg.Headers))
	for _, h := range c.msg.Headers {
		keys = append(keys, h.Key)
	}
	return keys
}

func injectTrace(ctx context.Context, msg *kafka.Message) {
	otel.GetTextMapPropagator().Inject(ctx, headerCarrier{msg: msg})
}

func extractTrace(ctx context.Context, msg kafka.Message) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, headerCarrier{msg: &msg})
}
