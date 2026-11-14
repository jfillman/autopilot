// Package fakecore is a stand-in for the Clearance core's HTTP API for the gateway's own tests and its load test: it
// answers POST /access/v1/evaluation with a function the test supplies, takes outcomes and records, and counts and
// times them. It decides nothing real; the replay test and the conformance harness use the real core.
package fakecore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jfillman/clearance/client"
)

// Core is a running fake.
type Core struct {
	*httptest.Server
	// Decide answers an evaluation. nil allows everything, charging one tool call.
	Decide func(req *client.EvaluationRequest) client.EvaluationResult
	// Latency is added to every answer.
	Latency time.Duration
	// Down, when set, makes every request fail with 503.
	Down atomic.Bool

	mu        sync.Mutex
	Outcomes  []Outcome
	Records   []json.RawMessage
	Evals     atomic.Int64
	seq       atomic.Int64
	outcomeID atomic.Int64
}

// Outcome is an outcome the fake took.
type Outcome struct {
	DecisionID string
	Body       client.OutcomeRequest
}

// New starts a fake core.
func New() *Core {
	c := &Core{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /access/v1/evaluation", func(w http.ResponseWriter, r *http.Request) {
		if c.pre(w) {
			return
		}
		var req client.EvaluationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		c.Evals.Add(1)
		var res client.EvaluationResult
		if c.Decide != nil {
			res = c.Decide(&req)
		} else {
			id := fmt.Sprintf("d-%d", c.seq.Add(1))
			res = client.EvaluationResult{Decision: true, DecisionID: &id, Record: &client.RecordRef{Seq: c.seq.Load(), Hash: "h"},
				Context: client.DecisionContext{Message: "allowed", Spend: client.Spend{ToolCalls: 1}}}
		}
		write(w, 200, res)
	})
	mux.HandleFunc("POST /v1/decisions/{id}/outcome", func(w http.ResponseWriter, r *http.Request) {
		if c.pre(w) {
			return
		}
		var body client.OutcomeRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		c.mu.Lock()
		c.Outcomes = append(c.Outcomes, Outcome{r.PathValue("id"), body})
		c.mu.Unlock()
		write(w, 200, client.RecordRef{Seq: c.seq.Add(1), Hash: "h"})
	})
	mux.HandleFunc("POST /v1/records", func(w http.ResponseWriter, r *http.Request) {
		if c.pre(w) {
			return
		}
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		c.mu.Lock()
		c.Records = append(c.Records, raw)
		c.mu.Unlock()
		write(w, 200, client.RecordRef{Seq: c.seq.Add(1), Hash: "h"})
	})
	c.Server = httptest.NewServer(mux)
	return c
}

func (c *Core) pre(w http.ResponseWriter) bool {
	if c.Latency > 0 {
		time.Sleep(c.Latency)
	}
	if c.Down.Load() {
		http.Error(w, "down", 503)
		return true
	}
	return false
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Client is a client with the given timeout and no breaker, pointed at the fake.
func (c *Core) Client(timeout time.Duration) *client.Client {
	cl, err := client.New(client.Options{BaseURL: c.URL, Token: client.StaticToken("autopilot-system/autopilot-gateway"),
		Provider: "autopilot-gateway", Timeout: timeout, BreakerThreshold: -1, Retries: -1})
	if err != nil {
		panic(err)
	}
	return cl
}

// OutcomeCount is how many outcomes were taken.
func (c *Core) OutcomeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.Outcomes)
}

// LastOutcome is the newest outcome taken.
func (c *Core) LastOutcome() Outcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Outcomes[len(c.Outcomes)-1]
}

// Deny is a decision that refuses with a rule.
func Deny(rule, message string, spend client.Spend) client.EvaluationResult {
	id := "d-deny"
	return client.EvaluationResult{Decision: false, DecisionID: &id, Record: &client.RecordRef{Seq: 1, Hash: "h"},
		Context: client.DecisionContext{Rule: rule, Message: message, Spend: spend}}
}

// RecordBodies is a copy of the records the fake took, as the client sent them.
func (c *Core) RecordBodies() []json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]json.RawMessage(nil), c.Records...)
}

// OutcomeList is a copy of the outcomes the fake took.
func (c *Core) OutcomeList() []Outcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Outcome(nil), c.Outcomes...)
}
