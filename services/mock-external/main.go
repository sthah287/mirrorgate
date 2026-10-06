// A stand-in for the external services a real order would touch: a payment
// provider and an email sender. Nothing here charges anyone or sends
// anything. It exists so shadow writes have somewhere safe to go and so a
// test can prove what the candidate actually called.
//
// Live endpoints are what the stable service uses. Sandbox endpoints are what
// the candidate uses, and they are the only ones shadow traffic can reach.
package main

import (
	"crypto/rand"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Call struct {
	Mode    string    `json:"mode"` // live or sandbox
	Kind    string    `json:"kind"` // payment or email
	OrderID string    `json:"order_id"`
	Amount  float64   `json:"amount"`
	At      time.Time `json:"at"`
}

// recentCalls is how many individual calls are kept for inspection. The
// counts are exact however many there have been; only the list is trimmed.
const recentCalls = 200

type recorder struct {
	mu     sync.Mutex
	counts map[string]int
	calls  []Call
}

func newRecorder() *recorder {
	return &recorder{counts: map[string]int{}}
}

func (rec *recorder) add(c Call) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	c.At = time.Now().UTC()
	rec.counts[c.Mode+"_"+c.Kind]++
	rec.calls = append(rec.calls, c)
	if len(rec.calls) > recentCalls {
		rec.calls = rec.calls[len(rec.calls)-recentCalls:]
	}
}

func (rec *recorder) snapshotCounts() map[string]int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make(map[string]int, len(rec.counts))
	for k, v := range rec.counts {
		out[k] = v
	}
	return out
}

func (rec *recorder) snapshot() []Call {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]Call, len(rec.calls))
	copy(out, rec.calls)
	return out
}

func (rec *recorder) reset() {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.calls = nil
	rec.counts = map[string]int{}
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9003"
	}
	rec := newRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("POST /payments/charge", handle(rec, "live", "payment"))
	mux.HandleFunc("POST /notifications/email", handle(rec, "live", "email"))
	mux.HandleFunc("POST /sandbox/payments/charge", handle(rec, "sandbox", "payment"))
	mux.HandleFunc("POST /sandbox/notifications/email", handle(rec, "sandbox", "email"))

	mux.HandleFunc("GET /calls", func(w http.ResponseWriter, r *http.Request) {
		calls := rec.snapshot()
		counts := rec.snapshotCounts()
		if mode := r.URL.Query().Get("mode"); mode != "" {
			filtered := calls[:0:0]
			for _, c := range calls {
				if c.Mode == mode {
					filtered = append(filtered, c)
				}
			}
			calls = filtered
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"counts": counts,
			"calls":  calls,
			"note":   "counts are exact; calls is the most recent " + strconv.Itoa(recentCalls),
		})
	})

	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, r *http.Request) {
		rec.reset()
		writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
	})

	log.Printf("mock external services listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func handle(rec *recorder, mode, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			OrderID string  `json:"order_id"`
			Amount  float64 `json:"amount"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		rec.add(Call{Mode: mode, Kind: kind, OrderID: body.OrderID, Amount: body.Amount})

		// A sandbox reference is marked so it is obvious in a stored response
		// that no real charge happened.
		prefix := "pay_live_"
		if mode == "sandbox" {
			prefix = "pay_sbx_"
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"reference": prefix + strings.ToLower(rand.Text()[:8]),
			"status":    "accepted",
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}
