package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"mirrorgate/shared/broker"
	"mirrorgate/shared/event"
	"mirrorgate/shared/outbox"
)

// Where the relay's time actually goes.
//
// Progress Report 3 measured the pipeline keeping up with only about 20
// events a second and guessed the cause was the Kafka writer's 50ms batch
// timeout, because the relay publishes one message per call. That was a
// hypothesis with arithmetic behind it and no measurement, so this times the
// three candidate costs separately against the real Redpanda and Postgres:
//
//	publishing one message at a time
//	publishing the same messages in one call
//	claiming a batch of rows out of Postgres
//
// It is a diagnostic rather than a correctness test, so it stays out of the
// normal suite and only runs when asked:
//
//	docker compose run --rm -e DIAGNOSTIC=1 tests \
//	  sh -c "cd outbox-relay && go test -run Diagnostic -v ./internal/relay/"
func TestPublishThroughputDiagnostic(t *testing.T) {
	if os.Getenv("DIAGNOSTIC") == "" {
		t.Skip("set DIAGNOSTIC=1 to run the throughput diagnostic")
	}
	brokers := os.Getenv("KAFKA_BROKERS")
	if brokers == "" {
		brokers = "redpanda:9092"
	}

	const n = 100
	topic := fmt.Sprintf("mirrorgate.diagnostic.%d", time.Now().UnixNano())
	if err := broker.EnsureTopic(context.Background(), brokers, topic, 1); err != nil {
		t.Fatalf("create diagnostic topic: %v", err)
	}
	producer := broker.NewProducer(brokers, topic)
	defer producer.Close()

	payload := samplePayload(t)
	ctx := context.Background()

	// Warm the connection so the first publish doesn't pay for the dial.
	if err := producer.PublishStored(ctx, "warmup", payload); err != nil {
		t.Fatalf("warmup publish: %v", err)
	}

	start := time.Now()
	for i := 0; i < n; i++ {
		if err := producer.PublishStored(ctx, fmt.Sprintf("one-%d", i), payload); err != nil {
			t.Fatalf("single publish %d: %v", i, err)
		}
	}
	oneAtATime := time.Since(start)

	msgs := make([]broker.StoredMessage, n)
	for i := range msgs {
		msgs[i] = broker.StoredMessage{RequestID: fmt.Sprintf("batch-%d", i), Payload: payload}
	}
	start = time.Now()
	acked, err := producer.PublishStoredBatch(ctx, msgs)
	if err != nil {
		t.Fatalf("batch publish: %v", err)
	}
	batched := time.Since(start)
	if len(acked) != n {
		t.Fatalf("broker acknowledged %d of %d", len(acked), n)
	}

	t.Logf("publishing %d messages one at a time: %v total, %.1f ms each, %.0f/s",
		n, oneAtATime.Round(time.Millisecond),
		float64(oneAtATime.Milliseconds())/n, float64(n)/oneAtATime.Seconds())
	t.Logf("publishing %d messages in one call:    %v total, %.2f ms each, %.0f/s",
		n, batched.Round(time.Millisecond),
		float64(batched.Milliseconds())/n, float64(n)/batched.Seconds())
	if batched > 0 {
		t.Logf("batching is %.0fx faster on this path", float64(oneAtATime)/float64(batched))
	}
}

// How much of the relay's time is Postgres rather than Kafka.
func TestClaimCostDiagnostic(t *testing.T) {
	if os.Getenv("DIAGNOSTIC") == "" {
		t.Skip("set DIAGNOSTIC=1 to run the throughput diagnostic")
	}
	store, pool := testStore(t)
	writeEvents(t, store, 300)
	ctx := context.Background()

	start := time.Now()
	tx, pending, err := store.Claim(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	claimed := time.Since(start)

	ids := make([]string, len(pending))
	for i, p := range pending {
		ids[i] = p.RequestID
	}
	start = time.Now()
	if err := markAndCommit(ctx, tx, ids); err != nil {
		t.Fatal(err)
	}
	marked := time.Since(start)

	var left int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM comparison_outbox WHERE published_at IS NULL").Scan(&left); err != nil {
		t.Fatal(err)
	}

	t.Logf("claiming 100 rows:  %v", claimed.Round(time.Microsecond))
	t.Logf("marking 100 rows:   %v", marked.Round(time.Microsecond))
	t.Logf("rows still pending: %d (expected 200)", left)
	t.Logf("so the database side of one 100-row batch costs about %v",
		(claimed + marked).Round(time.Millisecond))
}

func samplePayload(t *testing.T) []byte {
	t.Helper()
	e := event.Comparison{
		RequestID: "diagnostic",
		Method:    "GET",
		Path:      "/api/products/12",
		Stable:    event.Response{Status: 200, Body: `{"id":12,"price":49.99}`, LatencyMs: 0.4},
		Candidate: event.Response{Status: 200, Body: `{"id":12,"price":59.99}`, LatencyMs: 0.4},
	}
	payload, err := encodeEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func encodeEvent(e event.Comparison) ([]byte, error) {
	return json.Marshal(e)
}

func markAndCommit(ctx context.Context, tx pgx.Tx, ids []string) error {
	if err := outbox.MarkPublished(ctx, tx, ids); err != nil {
		tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}
