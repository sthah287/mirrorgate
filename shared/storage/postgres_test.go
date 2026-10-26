package storage

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The relay delivers at least once: a crash between publishing and marking a
// row sends the same event again. The worker has to be able to process that
// twice without creating a second comparison, so this checks the insert
// really is idempotent on request_id.
//
//	TEST_DATABASE_URL=postgres://... go test ./storage/
func testStore(t *testing.T) *Store {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; start the stack with docker compose up -d")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := fmt.Sprintf("storage_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close(context.Background())
	})

	schemaSQL, err := os.ReadFile("../../db/init.sql")
	if err != nil {
		t.Fatalf("read init.sql: %v", err)
	}
	if _, err := admin.Exec(ctx, "SET search_path TO "+schema+"; "+string(schemaSQL)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	store, err := Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func TestSaveComparisonIsIdempotent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	c := Comparison{
		RequestID:    "REQ-DUPLICATE",
		RequestSeq:   42,
		Method:       "GET",
		Path:         "/api/products/12",
		StableStatus: 200,
		Outcome:      "different",
		Differences:  []string{"price: 49.99 != 59.99"},
		ReceivedAt:   time.Now(),
		DetectedAt:   time.Now(),
	}

	for i := 0; i < 3; i++ {
		if err := store.SaveComparison(ctx, c); err != nil {
			t.Fatalf("save %d: %v", i+1, err)
		}
	}

	list, err := store.ListComparisons(ctx, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("%d rows after saving the same event three times, want 1", len(list))
	}
	if list[0].RequestSeq != 42 {
		t.Errorf("request_seq = %d, want the exact position 42", list[0].RequestSeq)
	}
	if list[0].DetectedAt.IsZero() {
		t.Error("detected_at was not stored")
	}
}
