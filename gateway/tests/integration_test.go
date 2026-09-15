package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"mirrorgate/gateway/internal/api"
	"mirrorgate/gateway/internal/proxy"
	"mirrorgate/gateway/internal/storage"
)

// These tests need a real Postgres. Each run creates its own schema so it
// doesn't touch data in the main table:
//
//	TEST_DATABASE_URL=postgres://mirrorgate:mirrorgate@localhost:5433/mirrorgate?sslmode=disable go test ./tests/
func openTestStore(t *testing.T) *storage.Store {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Logf("drop schema: %v", err)
		}
		conn.Close(context.Background())
	})

	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	store, err := storage.Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(store.Close)

	schemaSQL, err := os.ReadFile("../../db/init.sql")
	if err != nil {
		t.Fatalf("read init.sql: %v", err)
	}
	if _, err := conn.Exec(ctx, "SET search_path TO "+schema+"; "+string(schemaSQL)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return store
}

func TestMirroredRequestsAreStoredAndServedByAPI(t *testing.T) {
	store := openTestStore(t)

	stable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/products/1":
			io.WriteString(w, `{"id":1,"price":19.99}`)
		case "/api/products/12":
			io.WriteString(w, `{"id":12,"price":49.99}`)
		case "/api/products/8":
			io.WriteString(w, `{"id":8,"price":99.99}`)
		}
	}))
	defer stable.Close()

	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/products/1":
			io.WriteString(w, `{"price":19.99,"id":1}`)
		case "/api/products/12":
			io.WriteString(w, `{"id":12,"price":59.99}`)
		case "/api/products/8":
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":"inventory lookup failed"}`)
		}
	}))
	defer candidate.Close()

	mirror, err := proxy.New(proxy.Config{
		StableURL:     stable.URL,
		CandidateURL:  candidate.URL,
		ShadowTimeout: time.Second,
		MirrorMethods: []string{"GET"},
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(mirror)
	defer gateway.Close()
	admin := httptest.NewServer(api.New(store, api.Deployment{StableVersion: "v1", CandidateVersion: "v2"}).Routes())
	defer admin.Close()

	for _, path := range []string{"/api/products/1", "/api/products/12", "/api/products/8"} {
		resp, err := http.Get(gateway.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 from stable", path, resp.StatusCode)
		}
	}

	// Comparisons are saved in the background, so wait for them before
	// reading the database.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mirror.Wait(ctx); err != nil {
		t.Fatalf("waiting for shadow requests: %v", err)
	}

	var body struct {
		Deployment api.Deployment `json:"deployment"`
		Stats      storage.Stats  `json:"stats"`
	}
	getJSON(t, admin.URL+"/internal/stats", &body)
	st := body.Stats
	if st.Total != 3 || st.Matched != 1 || st.Different != 1 || st.Errors != 1 {
		t.Errorf("stats = %+v, want total=3 matched=1 different=1 errors=1", st)
	}
	if body.Deployment.CandidateVersion != "v2" {
		t.Errorf("deployment = %+v", body.Deployment)
	}

	var different []storage.Comparison
	getJSON(t, admin.URL+"/internal/comparisons?outcome=different", &different)
	if len(different) != 1 || different[0].Path != "/api/products/12" {
		t.Fatalf("different comparisons = %+v", different)
	}

	var detail storage.Comparison
	getJSON(t, fmt.Sprintf("%s/internal/comparisons/%d", admin.URL, different[0].ID), &detail)
	if detail.CandidateBody != `{"id":12,"price":59.99}` {
		t.Errorf("candidate body = %q", detail.CandidateBody)
	}
	if len(detail.Differences) != 1 || detail.Differences[0] != "price: 49.99 != 59.99" {
		t.Errorf("differences = %v", detail.Differences)
	}

	resp, err := http.Get(admin.URL + "/internal/comparisons/999999")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing comparison status = %d, want 404", resp.StatusCode)
	}
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}
