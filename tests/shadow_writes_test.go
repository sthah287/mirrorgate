package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// R5 orders product 4, which costs the same in both versions, so the only
// difference in the response is the one the candidate's total bug causes.
const orderBody = `{"customer":"sam@example.com","items":[{"product_id":4,"quantity":1}]}`

type orderResponse struct {
	OrderID          string  `json:"order_id"`
	Subtotal         float64 `json:"subtotal"`
	Shipping         float64 `json:"shipping"`
	Total            float64 `json:"total"`
	PaymentReference string  `json:"payment_reference"`
}

type ordersView struct {
	Sandbox bool `json:"sandbox"`
	Count   int  `json:"count"`
	Orders  []struct {
		OrderID          string  `json:"order_id"`
		Total            float64 `json:"total"`
		PaymentReference string  `json:"payment_reference"`
		Shadow           bool    `json:"shadow"`
	} `json:"orders"`
}

type mockCalls struct {
	Counts map[string]int `json:"counts"`
	Calls  []struct {
		Mode    string  `json:"mode"`
		Kind    string  `json:"kind"`
		OrderID string  `json:"order_id"`
		Amount  float64 `json:"amount"`
	} `json:"calls"`
}

func sandboxURLs(t *testing.T) (stable, candidate, mock string) {
	t.Helper()
	stable = env(t, "TEST_STABLE_URL")
	candidate = env(t, "TEST_CANDIDATE_URL")
	mock = env(t, "TEST_MOCK_URL")
	return stable, candidate, mock
}

// These tests send one request and expect it to be mirrored, so they only
// mean anything at 100% sampling. Running them against a gateway left in
// another mode by an experiment would fail for the wrong reason.
func requireFullSampling(t *testing.T, admin string) {
	t.Helper()
	var stats statsResponse
	getJSON(t, admin+"/internal/stats", &stats)
	if stats.Sampling.Mode != "full" {
		t.Skipf("gateway is in %q sampling mode; restart it with SAMPLING_MODE=full to run the write tests", stats.Sampling.Mode)
	}
}

func env(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set; start the stack with docker compose up -d", key)
	}
	return v
}

// postOrder sends an order through the gateway and returns what the client
// saw plus the request id, the same way send does for GETs.
func postOrder(t *testing.T, gateway string) (int, orderResponse, string) {
	t.Helper()
	resp, err := http.Post(gateway+"/api/orders", "application/json", bytes.NewReader([]byte(orderBody)))
	if err != nil {
		t.Fatalf("POST /api/orders: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read order response: %v", err)
	}
	var order orderResponse
	if resp.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(body, &order); err != nil {
			t.Fatalf("decode order response %q: %v", body, err)
		}
	}
	return resp.StatusCode, order, resp.Header.Get("X-Request-ID")
}

// One mirrored order should produce exactly one real order and exactly one
// live charge, with the candidate's copy confined to the sandbox.
func TestShadowWriteStaysInTheSandbox(t *testing.T) {
	gateway, admin := urls(t)
	stableURL, candidateURL, mockURL := sandboxURLs(t)
	requireFullSampling(t, admin)

	resetMock(t, mockURL)
	before := ordersCount(t, stableURL)
	beforeSandbox := ordersCount(t, candidateURL)

	status, order, requestID := postOrder(t, gateway)
	if status != http.StatusCreated {
		t.Fatalf("client status = %d, want 201", status)
	}

	// The client gets the stable answer: shipping included in the total.
	if order.Total != order.Subtotal+order.Shipping {
		t.Errorf("client total = %.2f, want subtotal %.2f + shipping %.2f",
			order.Total, order.Subtotal, order.Shipping)
	}
	if order.Total != 184.99 {
		t.Errorf("client total = %.2f, want the stable total 184.99", order.Total)
	}
	if !strings.HasPrefix(order.PaymentReference, "pay_live_") {
		t.Errorf("payment reference = %q, the client should have the live one", order.PaymentReference)
	}

	// One row on each side, and the candidate's is marked as shadow traffic.
	stable := orders(t, stableURL)
	if stable.Count != before+1 {
		t.Errorf("stable orders went from %d to %d, want exactly one more", before, stable.Count)
	}
	if stable.Sandbox {
		t.Error("the stable service reports itself as a sandbox")
	}
	if stable.Orders[0].OrderID != order.OrderID {
		t.Errorf("newest stable order = %s, want the one the client got (%s)", stable.Orders[0].OrderID, order.OrderID)
	}
	if stable.Orders[0].Shadow {
		t.Error("the real order was stored as shadow traffic")
	}

	sandbox := orders(t, candidateURL)
	if sandbox.Count != beforeSandbox+1 {
		t.Errorf("sandbox orders went from %d to %d, want exactly one more", beforeSandbox, sandbox.Count)
	}
	if !sandbox.Orders[0].Shadow {
		t.Error("the candidate's order was not marked as shadow traffic")
	}
	if sandbox.Orders[0].OrderID == order.OrderID {
		t.Error("the candidate reused the stable order id; the two writes are not independent")
	}

	// The candidate's order must not exist in the stable database.
	for _, o := range stable.Orders {
		if o.OrderID == sandbox.Orders[0].OrderID {
			t.Fatalf("candidate order %s was written to the stable database", o.OrderID)
		}
	}

	// Side effects: one live charge for the real order, and the candidate's
	// charge only ever hit the sandbox endpoint.
	calls := externalCalls(t, mockURL)
	if calls.Counts["live_payment"] != 1 {
		t.Errorf("live payments = %d, want exactly 1", calls.Counts["live_payment"])
	}
	if calls.Counts["live_email"] != 1 {
		t.Errorf("live emails = %d, want exactly 1", calls.Counts["live_email"])
	}
	if calls.Counts["sandbox_payment"] != 1 {
		t.Errorf("sandbox payments = %d, want exactly 1", calls.Counts["sandbox_payment"])
	}
	for _, c := range calls.Calls {
		if c.Mode == "live" && c.OrderID != order.OrderID {
			t.Errorf("a live %s call was made for order %s, which is not the real order", c.Kind, c.OrderID)
		}
	}

	// And the comparison still catches R5.
	c := waitFor(t, admin, requestID)
	if c.Outcome != "different" {
		t.Errorf("outcome = %s, want different (differences: %v)", c.Outcome, c.Differences)
	}
	if len(c.Differences) != 1 || c.Differences[0] != "total: 184.99 != 179.99" {
		t.Errorf("differences = %v, want only the total", c.Differences)
	}
}

// The generated id, timestamp, and payment reference are different on every
// order. The ignore rules are what keep those from being reported.
func TestDynamicOrderFieldsAreIgnored(t *testing.T) {
	gateway, admin := urls(t)
	sandboxURLs(t)
	requireFullSampling(t, admin)

	_, _, requestID := postOrder(t, gateway)
	c := waitFor(t, admin, requestID)

	// The list endpoint leaves the bodies out, so the detail view is what
	// shows whether the ignored fields really were different.
	var detail comparison
	getJSON(t, fmt.Sprintf("%s/internal/comparisons/%d", admin, c.ID), &detail)

	for _, d := range c.Differences {
		for _, ignored := range []string{"order_id", "created_at", "payment_reference"} {
			if strings.HasPrefix(d, ignored) {
				t.Errorf("%q should have been ignored, differences were %v", ignored, c.Differences)
			}
		}
	}
	if detail.StableBody == detail.CandidateBody {
		t.Error("the two bodies were identical, so this test proved nothing")
	}
	if !strings.Contains(detail.CandidateBody, "pay_sbx_") {
		t.Errorf("candidate body %q should carry a sandbox payment reference", detail.CandidateBody)
	}
}

// The candidate is a sandbox deployment. A write that isn't shadow traffic
// should be refused rather than stored.
func TestCandidateRefusesNonShadowWrites(t *testing.T) {
	_, candidateURL, _ := sandboxURLs(t)

	before := ordersCount(t, candidateURL)

	resp, err := http.Post(candidateURL+"/api/orders", "application/json", bytes.NewReader([]byte(orderBody)))
	if err != nil {
		t.Fatalf("POST to candidate: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a write without the shadow header", resp.StatusCode)
	}
	if after := ordersCount(t, candidateURL); after != before {
		t.Errorf("sandbox orders went from %d to %d; the refused write was stored anyway", before, after)
	}
}

// A candidate that fails on a write must not change what the client gets.
func TestCandidateWriteFailureDoesNotAffectTheClient(t *testing.T) {
	gateway, _ := urls(t)
	stableURL, _, _ := sandboxURLs(t)

	// An unknown product is rejected by both versions, but the client's
	// answer still has to come from stable.
	bad := `{"customer":"sam@example.com","items":[{"product_id":999,"quantity":1}]}`
	resp, err := http.Post(gateway+"/api/orders", "application/json", bytes.NewReader([]byte(bad)))
	if err != nil {
		t.Fatalf("POST /api/orders: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 from the stable service", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("unknown product")) {
		t.Errorf("body = %q, want the stable error message", body)
	}

	// The stable service is still able to take a good order afterwards.
	before := ordersCount(t, stableURL)
	if status, _, _ := postOrder(t, gateway); status != http.StatusCreated {
		t.Fatalf("status = %d after the failed order, want 201", status)
	}
	if after := ordersCount(t, stableURL); after != before+1 {
		t.Errorf("stable orders went from %d to %d, want one more", before, after)
	}
}

func orders(t *testing.T, serviceURL string) ordersView {
	t.Helper()
	var view ordersView
	getJSON(t, serviceURL+"/internal/orders", &view)
	if view.Count > 0 && len(view.Orders) == 0 {
		t.Fatalf("%s reported %d orders but returned none", serviceURL, view.Count)
	}
	return view
}

func ordersCount(t *testing.T, serviceURL string) int {
	t.Helper()
	return orders(t, serviceURL).Count
}

func externalCalls(t *testing.T, mockURL string) mockCalls {
	t.Helper()
	var calls mockCalls
	getJSON(t, mockURL+"/calls", &calls)
	return calls
}

func resetMock(t *testing.T, mockURL string) {
	t.Helper()
	resp, err := http.Post(mockURL+"/reset", "application/json", nil)
	if err != nil {
		t.Fatalf("reset mock: %v", err)
	}
	resp.Body.Close()
}
