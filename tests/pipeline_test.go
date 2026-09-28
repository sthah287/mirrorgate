// Package tests checks MirrorGate from the outside, against the containers
// that docker compose actually starts. Everything in here goes through the
// gateway's public port and the internal API, so it exercises the real
// gateway, Redpanda, comparison worker, and Postgres rather than a copy of
// them wired up inside the test.
//
//	docker compose up -d
//	docker compose run --rm tests
package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Only the fields these tests look at. Keeping it local means the test also
// checks that the JSON the dashboard consumes still has these names.
type comparison struct {
	ID            int64    `json:"id"`
	RequestID     string   `json:"request_id"`
	TraceID       string   `json:"trace_id"`
	Path          string   `json:"path"`
	Outcome       string   `json:"outcome"`
	CandidateSlow bool     `json:"candidate_slow"`
	Differences   []string `json:"differences"`
	CandidateErr  string   `json:"candidate_error"`
	StableBody    string   `json:"stable_body"`
	CandidateBody string   `json:"candidate_body"`
}

type statsResponse struct {
	Gateway struct {
		Eligible      int64 `json:"eligible"`
		Mirrored      int64 `json:"mirrored"`
		Published     int64 `json:"published"`
		PublishFailed int64 `json:"publish_failed"`
	} `json:"gateway"`
	Sampling struct {
		Mode     string  `json:"mode"`
		Rate     float64 `json:"rate"`
		Mirrored int64   `json:"mirrored"`
		Skipped  int64   `json:"skipped"`
	} `json:"sampling"`
}

func urls(t *testing.T) (gateway, admin string) {
	t.Helper()
	gateway = os.Getenv("TEST_GATEWAY_URL")
	admin = os.Getenv("TEST_ADMIN_URL")
	if gateway == "" || admin == "" {
		t.Skip("TEST_GATEWAY_URL and TEST_ADMIN_URL not set; start the stack with docker compose up -d")
	}
	return gateway, admin
}

// send returns the status the client saw, the body it got, and the request id
// the gateway assigned, which is how the stored comparison is found later.
func send(t *testing.T, gateway, path string) (int, string, string) {
	t.Helper()
	resp, err := http.Get(gateway + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	requestID := resp.Header.Get("X-Request-ID")
	if requestID == "" {
		t.Fatalf("GET %s returned no X-Request-ID", path)
	}
	return resp.StatusCode, string(body[:n]), requestID
}

// waitFor polls the internal API until the comparison for this request id
// shows up. The whole point of the milestone is that this is asynchronous
// now, so the test has to wait rather than read straight after the response.
func waitFor(t *testing.T, admin, requestID string) comparison {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var list []comparison
		getJSON(t, admin+"/internal/comparisons?limit=200", &list)
		for _, c := range list {
			if c.RequestID == requestID {
				return c
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("comparison for request %s never reached Postgres", requestID)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

// Every regression the candidate service fakes on purpose should come out
// the other end of the pipeline with the right classification.
func TestKnownRegressionsAreDetectedThroughThePipeline(t *testing.T) {
	gateway, admin := urls(t)

	cases := []struct {
		name     string
		path     string
		status   int
		outcome  string
		slow     bool
		contains string
	}{
		{name: "normal request", path: "/api/products/1", status: 200, outcome: "match"},
		{name: "price regression", path: "/api/products/12", status: 200, outcome: "different", contains: "price: 49.99 != 59.99"},
		{name: "candidate 500", path: "/api/products/8", status: 200, outcome: "error", contains: "status: 200 != 500"},
		{name: "slow candidate", path: "/api/products?category=audio", status: 200, outcome: "match", slow: true},
		{name: "candidate timeout", path: "/api/users/7", status: 200, outcome: "error", contains: "timed out"},
		{name: "reordered json", path: "/api/users/3", status: 200, outcome: "match"},
		{name: "404 from both", path: "/api/products/99", status: 404, outcome: "match"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body, requestID := send(t, gateway, tc.path)
			if status != tc.status {
				t.Fatalf("client got %d, want %d (body %s)", status, tc.status, body)
			}

			c := waitFor(t, admin, requestID)
			if c.Outcome != tc.outcome {
				t.Errorf("outcome = %s, want %s (differences %v, error %q)",
					c.Outcome, tc.outcome, c.Differences, c.CandidateErr)
			}
			if c.CandidateSlow != tc.slow {
				t.Errorf("candidate_slow = %v, want %v", c.CandidateSlow, tc.slow)
			}
			if tc.contains != "" && !hasText(c, tc.contains) {
				t.Errorf("expected %q in differences %v or error %q", tc.contains, c.Differences, c.CandidateErr)
			}
			// Tracing runs in compose, so a stored row should be findable in
			// Jaeger by its trace id.
			if c.TraceID == "" {
				t.Error("no trace id was stored with the comparison")
			}
		})
	}
}

func hasText(c comparison, want string) bool {
	if strings.Contains(c.CandidateErr, want) {
		return true
	}
	for _, d := range c.Differences {
		if strings.Contains(d, want) {
			return true
		}
	}
	return false
}

// The candidate's response must never be what the client sees, even when the
// candidate answers first.
func TestClientAlwaysGetsTheStableResponse(t *testing.T) {
	gateway, admin := urls(t)

	status, body, requestID := send(t, gateway, "/api/products/12")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(body, "49.99") {
		t.Errorf("client body = %s, want the stable price 49.99", body)
	}
	if strings.Contains(body, "59.99") {
		t.Fatalf("client body = %s, that is the candidate response", body)
	}

	// The stored comparison proves the candidate really did answer 59.99, so
	// the test above isn't passing just because the candidate was down. The
	// list endpoint leaves bodies out, so this reads the detail view.
	c := waitFor(t, admin, requestID)
	var detail comparison
	getJSON(t, fmt.Sprintf("%s/internal/comparisons/%d", admin, c.ID), &detail)
	if !strings.Contains(detail.CandidateBody, "59.99") || !strings.Contains(detail.StableBody, "49.99") {
		t.Errorf("stored bodies = %q / %q", detail.StableBody, detail.CandidateBody)
	}
}

// Whatever the gateway published should end up as a row, and the sampling
// counters should agree with what the proxy counted.
func TestGatewayCountersAgreeWithStoredComparisons(t *testing.T) {
	gateway, admin := urls(t)

	var before statsResponse
	getJSON(t, admin+"/internal/stats", &before)

	const requests = 12
	ids := make([]string, 0, requests)
	for i := 0; i < requests; i++ {
		_, _, requestID := send(t, gateway, "/api/products/1")
		ids = append(ids, requestID)
	}
	for _, id := range ids {
		waitFor(t, admin, id)
	}

	var after statsResponse
	getJSON(t, admin+"/internal/stats", &after)

	if got := after.Gateway.Eligible - before.Gateway.Eligible; got != requests {
		t.Errorf("eligible went up by %d, want %d", got, requests)
	}
	mirrored := after.Gateway.Mirrored - before.Gateway.Mirrored
	published := after.Gateway.Published - before.Gateway.Published
	if mirrored != published {
		t.Errorf("mirrored %d but published %d", mirrored, published)
	}
	if after.Gateway.PublishFailed != before.Gateway.PublishFailed {
		t.Errorf("publish failures went up by %d", after.Gateway.PublishFailed-before.Gateway.PublishFailed)
	}

	// The sampler and the proxy count the same decisions from two places, so
	// a mismatch means one of them is being bypassed.
	sampled := (after.Sampling.Mirrored + after.Sampling.Skipped) - (before.Sampling.Mirrored + before.Sampling.Skipped)
	if eligible := after.Gateway.Eligible - before.Gateway.Eligible; sampled != eligible {
		t.Errorf("sampler saw %d decisions but %d requests were eligible", sampled, eligible)
	}
	if after.Sampling.Mode == "" {
		t.Error("sampling mode is missing from /internal/stats")
	}
}

// /health is mirrored like anything else under the default config, but the
// endpoint rules in the README switch it off. This checks the endpoint the
// dashboard reads for its sampling panel is actually populated.
func TestSamplingIsReportedForTheDashboard(t *testing.T) {
	_, admin := urls(t)

	var stats statsResponse
	getJSON(t, admin+"/internal/stats", &stats)

	switch stats.Sampling.Mode {
	case "full", "random", "endpoint":
	default:
		t.Fatalf("sampling mode = %q", stats.Sampling.Mode)
	}
	if stats.Sampling.Rate < 0 || stats.Sampling.Rate > 1 {
		t.Errorf("sampling rate = %v", stats.Sampling.Rate)
	}
	fmt.Printf("sampling: mode=%s rate=%v mirrored=%d skipped=%d\n",
		stats.Sampling.Mode, stats.Sampling.Rate, stats.Sampling.Mirrored, stats.Sampling.Skipped)
}
