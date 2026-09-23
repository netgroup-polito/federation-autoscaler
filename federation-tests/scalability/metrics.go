/*
Copyright 2026 Politecnico di Torino - NetGroup.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
)

// Outcome buckets every request into exactly one of these states, as
// required by the experiment's evaluation/error metrics.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
	OutcomeTimeout Outcome = "timeout"
	// OutcomeCancelled is a request the harness itself cancelled (Ctrl+C,
	// SIGTERM): it says nothing about the Broker, so it is neither a success
	// nor an error. A deadline is a timeout, never a cancellation.
	OutcomeCancelled Outcome = "cancelled"
)

// Operation names the four Broker calls this harness exercises. Reservation
// creation (POST /api/v1/reservations) is intentionally absent — it is
// explicitly out of scope for this Broker-only load test.
type Operation string

const (
	OpEvaluation    Operation = "evaluation"    // GET  /api/v1/nodegroups
	OpAdvertisement Operation = "advertisement" // POST /api/v1/advertisements
	OpHeartbeat     Operation = "heartbeat"     // POST /api/v1/heartbeat
	OpInstructions  Operation = "instructions"  // GET  /api/v1/instructions
)

// Phase distinguishes warm-up traffic (retried until first success, not
// representative of steady-state performance) from measurement traffic.
// Every record is kept in the raw CSVs regardless of phase — nothing is
// discarded — but summary.go's aggregate statistics default to
// PhaseMeasurement only, so a slow Broker cold-start doesn't skew the
// latency percentiles the summary reports.
type Phase string

const (
	PhaseWarmup      Phase = "warmup"
	PhaseMeasurement Phase = "measurement"
)

// Record is one measured request. AgentRole/AgentID identify who issued it;
// StatusCode is the HTTP status when known (the agent client hard-codes 200
// on every success path in this harness's endpoints, since that's what
// every handler used here returns on success — see server.go/*.go).
type Record struct {
	Timestamp     time.Time
	AgentRole     string // "provider" | "consumer"
	AgentID       string
	Operation     Operation
	Phase         Phase
	LatencyMS     float64
	StatusCode    int
	Outcome       Outcome
	ErrorCategory string
	ErrorMessage  string
}

// recordFor builds a Record from a raw client call's outcome. err == nil is
// treated as HTTP 200 because every endpoint this harness calls
// (advertisement.go, heartbeat.go, nodegroups.go, instructions.go) writes
// http.StatusOK on its success path, and the agent client's typed success
// return discards the status code (internal/agent/client/request.go:attempt).
func recordFor(role, agentID string, op Operation, start time.Time, latency time.Duration, err error, phase Phase) Record {
	r := Record{
		Timestamp: start,
		AgentRole: role,
		AgentID:   agentID,
		Operation: op,
		Phase:     phase,
		LatencyMS: float64(latency) / float64(time.Millisecond),
	}
	if err == nil {
		r.Outcome = OutcomeSuccess
		r.StatusCode = 200
		return r
	}
	r.Outcome, r.StatusCode, r.ErrorCategory, r.ErrorMessage = classify(err)
	return r
}

// Collector accumulates Records concurrently from every provider/consumer
// goroutine. It intentionally keeps everything in memory: even the largest
// documented scale (200 agents, 10 minutes, 5s eval interval) is a few
// hundred thousand small structs, well within a modern test-runner's RAM,
// and in-memory raw data is what lets the summary recompute percentiles
// exactly rather than from a pre-aggregated approximation.
type Collector struct {
	mu      sync.Mutex
	records []Record
}

func NewCollector() *Collector { return &Collector{} }

func (c *Collector) Add(r Record) {
	c.mu.Lock()
	c.records = append(c.records, r)
	c.mu.Unlock()
}

// Snapshot returns a copy of every record collected so far. Called once,
// after every generator has stopped, so no lock is held while summary.go
// and writer.go iterate.
func (c *Collector) Snapshot() []Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Record, len(c.records))
	copy(out, c.records)
	return out
}

// classify turns the agent client's typed error (internal/agent/client/errors.go)
// into this harness's Outcome/category/message triple. err == nil is the
// success path and is handled by the caller before classify is invoked.
func classify(err error) (outcome Outcome, statusCode int, category, message string) {
	var cerr *agentclient.Error
	if !errors.As(err, &cerr) {
		if errors.Is(err, context.Canceled) {
			return OutcomeCancelled, 0, "Cancelled", err.Error()
		}
		return OutcomeFailure, 0, "unknown", err.Error()
	}
	if cerr.Status == 0 && errors.Is(cerr.Cause, context.Canceled) {
		return OutcomeCancelled, 0, "Cancelled", cerr.Message
	}
	if isTimeout(cerr) {
		return OutcomeTimeout, cerr.Status, categoryName(cerr.Category), cerr.Message
	}
	return OutcomeFailure, cerr.Status, categoryName(cerr.Category), cerr.Message
}

// isTimeout recognises both a context deadline (per-attempt RequestTimeout
// elapsed, or the whole run's ctx was cancelled while waiting) and a
// net.Error reporting Timeout()==true (the http.Client's own Timeout budget
// firing mid-request).
func isTimeout(cerr *agentclient.Error) bool {
	if cerr.Status != 0 {
		return false // a real HTTP response arrived; not a timeout.
	}
	if errors.Is(cerr.Cause, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(cerr.Cause, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

func categoryName(c agentclient.Category) string {
	switch c {
	case agentclient.CategoryTransient:
		return "Transient"
	case agentclient.CategoryBadRequest:
		return "BadRequest"
	case agentclient.CategoryUnauthenticated:
		return "Unauthenticated"
	case agentclient.CategoryForbidden:
		return "Forbidden"
	case agentclient.CategoryNotFound:
		return "NotFound"
	case agentclient.CategoryConflict:
		return "Conflict"
	case agentclient.CategoryPreconditionFailed:
		return "PreconditionFailed"
	case agentclient.CategoryTooManyRequests:
		return "TooManyRequests"
	default:
		return "Unknown"
	}
}
