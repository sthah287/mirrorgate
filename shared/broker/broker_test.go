package broker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"mirrorgate/shared/event"
)

// The gateway and the worker only agree through this JSON, so anything that
// silently drops a field here would quietly break the dashboard.
func TestEventRoundTrip(t *testing.T) {
	want := event.Comparison{
		RequestID:  "MGCDWJRPV2UREPOMTAJJBBXUEG",
		TraceID:    "4bf92f3577b34da6a3ce929d0e0e4736",
		ReceivedAt: time.Date(2026, 9, 20, 14, 30, 0, 0, time.UTC),
		Method:     "GET",
		Path:       "/api/products/12",
		Query:      "currency=usd",
		Stable:     event.Response{Status: 200, LatencyMs: 1.309, Body: `{"price":49.99}`},
		Candidate:  event.Response{Status: 200, LatencyMs: 1.016, Body: `{"price":59.99}`},
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got event.Comparison
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}

	if got.RequestID != want.RequestID || got.TraceID != want.TraceID {
		t.Errorf("ids = %s / %s", got.RequestID, got.TraceID)
	}
	if !got.ReceivedAt.Equal(want.ReceivedAt) {
		t.Errorf("received_at = %s, want %s", got.ReceivedAt, want.ReceivedAt)
	}
	if got.Stable != want.Stable || got.Candidate != want.Candidate {
		t.Errorf("responses = %+v / %+v", got.Stable, got.Candidate)
	}
	if got.Method != want.Method || got.Path != want.Path || got.Query != want.Query {
		t.Errorf("request = %s %s?%s", got.Method, got.Path, got.Query)
	}
}

func TestCandidateErrorSurvivesEncoding(t *testing.T) {
	encoded, err := json.Marshal(event.Comparison{CandidateError: "timed out after 2s"})
	if err != nil {
		t.Fatal(err)
	}
	var got event.Comparison
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.CandidateError != "timed out after 2s" {
		t.Errorf("candidate_error = %q", got.CandidateError)
	}
	// An event with no error should not carry an empty candidate_error key.
	clean, _ := json.Marshal(event.Comparison{RequestID: "X"})
	if strings.Contains(string(clean), "candidate_error") {
		t.Errorf("encoded event = %s, want candidate_error omitted", clean)
	}
}

// Kafka is the one hop where the trace context has to travel by hand.
func TestTraceContextTravelsThroughHeaders(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})

	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36},
		SpanID:     trace.SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	producerCtx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	msg := kafka.Message{Value: []byte(`{}`)}
	injectTrace(producerCtx, &msg)
	if len(msg.Headers) == 0 {
		t.Fatal("no trace headers were added to the message")
	}

	// A fresh context, the way the worker gets it.
	got := trace.SpanContextFromContext(extractTrace(context.Background(), msg))
	if got.TraceID() != spanCtx.TraceID() {
		t.Errorf("trace id = %s, want %s", got.TraceID(), spanCtx.TraceID())
	}
	if got.SpanID() != spanCtx.SpanID() {
		t.Errorf("span id = %s, want %s", got.SpanID(), spanCtx.SpanID())
	}
}

func TestHeaderCarrierReplacesInsteadOfDuplicating(t *testing.T) {
	msg := kafka.Message{}
	c := headerCarrier{msg: &msg}
	c.Set("traceparent", "first")
	c.Set("traceparent", "second")

	if len(msg.Headers) != 1 {
		t.Fatalf("headers = %d, want 1", len(msg.Headers))
	}
	if c.Get("traceparent") != "second" {
		t.Errorf("traceparent = %q, want second", c.Get("traceparent"))
	}
	if keys := c.Keys(); len(keys) != 1 || keys[0] != "traceparent" {
		t.Errorf("keys = %v", keys)
	}
}
