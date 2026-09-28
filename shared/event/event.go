// Package event holds the message the gateway publishes after a shadow
// request finishes. It is the only thing the gateway and the comparison
// worker have to agree on, so it stays deliberately small.
package event

import "time"

// Topic is where comparison events are published. One topic is enough at
// this stage; every consumer wants the same records.
const Topic = "mirrorgate.comparisons"

// Comparison carries both responses as they came off the wire. The worker
// decides whether they match, not the gateway.
type Comparison struct {
	RequestID  string    `json:"request_id"`
	TraceID    string    `json:"trace_id,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Query      string    `json:"query,omitempty"`

	Stable    Response `json:"stable"`
	Candidate Response `json:"candidate"`

	// Set when the candidate never answered: timeout, connection refused,
	// unreadable body. The status and body fields are meaningless then.
	CandidateError string `json:"candidate_error,omitempty"`
}

type Response struct {
	Status    int     `json:"status"`
	LatencyMs float64 `json:"latency_ms"`
	Body      string  `json:"body"`
}
