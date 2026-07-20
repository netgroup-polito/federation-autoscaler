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

// Package health is the agent's liveness/readiness probe. Both endpoints
// are served by a single Probe instance:
//
//   - /healthz returns 200 as long as the process is up.
//   - /readyz returns 200 while the agent has had a successful broker
//     contact within PollStaleAfter (default 30 s), so the gate flips red
//     when the broker becomes unreachable or the poll loop deadlocks.
//
// "Broker contact" is deliberately broader than the poll loop: RecordPoll
// is wired from the poller's OnPollResult (5 s) AND from the role's own
// background loop — the consumer heartbeat (15 s) or the provider
// advertisement publisher (30 s). Any of them refreshes the gate, so the
// threshold must stay comfortably above the SLOWEST contact for the role;
// cmd/agent/main.go sizes it per role for exactly that reason.
//
// One wrinkle the timestamp alone cannot express: instruction handlers run
// synchronously on the poll goroutine, and `liqoctl peer` can hold it for
// minutes. That is the loop working, not stalling, so RecordHandlerActive
// brackets each handler and suppresses the staleness check while one is in
// flight — bounded by MaxHandlerInFlight so a genuinely wedged handler
// still surfaces.
//
// The probe is a single-instance singleton owned by cmd/agent/main.go; it
// holds no Kubernetes client and runs an embedded http.Server, mirroring
// the pattern used elsewhere in the repo for cmd-side health probes.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// DefaultPollStaleAfter is the readiness threshold for the poll loop.
// 30 s ≈ 6 missed polls at the 5 s default cadence — long enough to
// absorb a broker restart, short enough to fail fast on a hard outage.
const DefaultPollStaleAfter = 30 * time.Second

// DefaultMaxHandlerInFlight bounds how long a single in-flight instruction
// handler may suppress the poll-staleness check. Sized generously above the
// longest handler timeout in the tree (consumer Peer's
// instructions.DefaultPeerExecTimeout = 10 m) so a legitimately slow
// `liqoctl peer` never trips it, while a handler wedged beyond any plausible
// runtime still turns the gate red — Live() always returns nil, so this is
// the only signal that would ever surface one.
const DefaultMaxHandlerInFlight = 15 * time.Minute

// Options bundles construction-time settings.
type Options struct {
	// PollStaleAfter overrides DefaultPollStaleAfter.
	PollStaleAfter time.Duration

	// MaxHandlerInFlight overrides DefaultMaxHandlerInFlight.
	MaxHandlerInFlight time.Duration

	// Now overrides time.Now in tests; nil falls back to time.Now.
	Now func() time.Time
}

// Probe holds the freshness state surfaced through /healthz and /readyz.
// All public methods are safe for concurrent use.
type Probe struct {
	// lastPollOK is the unix-nano timestamp of the most recent
	// successful poll, or 0 if no poll has succeeded yet. atomic so the
	// poller goroutine and the HTTP handlers can race without a mutex.
	lastPollOK atomic.Int64

	// inFlight counts instruction handlers currently executing on the poll
	// goroutine. A count rather than a flag: dispatch is strictly serial
	// today, but a counter costs nothing and stays correct if that changes.
	inFlight atomic.Int64

	// handlerSince is the unix-nano start of the CURRENT in-flight stretch
	// (stamped when inFlight goes 0 -> 1), or 0 when idle. It bounds the
	// staleness suppression in Ready.
	handlerSince atomic.Int64

	pollStaleAfter     time.Duration
	maxHandlerInFlight time.Duration
	now                func() time.Time
}

// New returns a Probe with no successful polls recorded; /readyz starts
// red and flips green on the first RecordPoll(true).
func New(opts Options) *Probe {
	if opts.PollStaleAfter <= 0 {
		opts.PollStaleAfter = DefaultPollStaleAfter
	}
	if opts.MaxHandlerInFlight <= 0 {
		opts.MaxHandlerInFlight = DefaultMaxHandlerInFlight
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Probe{
		pollStaleAfter:     opts.PollStaleAfter,
		maxHandlerInFlight: opts.MaxHandlerInFlight,
		now:                opts.Now,
	}
}

// RecordPoll records a broker contact. It is wired to
// poller.Options.OnPollResult and, per role, to the consumer heartbeat's
// OnHeartbeatResult or the provider publisher's OnPublishResult — any of
// them refreshes the readiness gate. Only success bumps the freshness
// timestamp; failures leave it untouched so the staleness check naturally
// trips after the configured window.
func (p *Probe) RecordPoll(success bool) {
	if !success {
		return
	}
	p.lastPollOK.Store(p.now().UnixNano())
}

// RecordHandlerActive brackets an instruction handler running on the poll
// goroutine; it is wired to poller.Options.OnHandlerActive. Handlers are
// dispatched synchronously, so a slow one (liqoctl peer takes 40-90 s, and
// may run to its 10 m timeout) blocks the next poll entirely. That is the
// loop working, not stalling, so Ready suppresses the staleness check while
// a handler is in flight.
//
// Finishing a handler ALSO refreshes the freshness timestamp: returning from
// one is itself proof the loop is alive, and without it the gap between two
// instructions in the same batch — where inFlight briefly drops to 0 while
// lastPollOK is still old — would flap the gate mid-batch.
//
// Calls must be balanced; poller.dispatch guarantees that with a defer. The
// decrement clamps at zero so an unbalanced call can never wedge the counter
// negative and permanently disable the suppression.
func (p *Probe) RecordHandlerActive(active bool) {
	if active {
		if p.inFlight.Add(1) == 1 {
			p.handlerSince.Store(p.now().UnixNano())
		}
		return
	}
	p.lastPollOK.Store(p.now().UnixNano())
	if p.inFlight.Add(-1) <= 0 {
		p.inFlight.Store(0)
		p.handlerSince.Store(0)
	}
}

// Live always returns nil — the process is up by virtue of this code
// running. Future versions may surface a non-nil error if the agent
// detects a fatal in-process condition (e.g. a stuck handler that
// can't be cancelled).
func (p *Probe) Live() error { return nil }

// Ready returns nil when /readyz should respond 200, or an error
// describing why the gate is closed. Errors are wire-stable enough for
// tests to assert on substrings.
func (p *Probe) Ready() error {
	last := p.lastPollOK.Load()
	if last == 0 {
		return errors.New("agent has not completed a successful poll yet")
	}
	age := p.now().Sub(time.Unix(0, last))
	if age > p.pollStaleAfter {
		// A handler executing on the poll goroutine legitimately prevents the
		// next poll from being recorded — suppress the staleness check while
		// one is in flight, but only up to maxHandlerInFlight so a wedged
		// handler still turns the gate red.
		if since := p.handlerSince.Load(); p.inFlight.Load() > 0 && since != 0 {
			busy := p.now().Sub(time.Unix(0, since))
			if busy <= p.maxHandlerInFlight {
				return nil
			}
			return fmt.Errorf("instruction handler has been running %s (max %s); last successful poll was %s ago",
				busy.Round(time.Millisecond), p.maxHandlerInFlight, age.Round(time.Millisecond))
		}
		return fmt.Errorf("last successful poll was %s ago (threshold %s)",
			age.Round(time.Millisecond), p.pollStaleAfter)
	}
	return nil
}

// Handler returns an http.Handler exposing /healthz and /readyz on a
// new ServeMux. Caller mounts it on whatever address the agent's
// --health-probe-bind-address flag resolves to. Use Serve below if you
// want the standard "blocks until ctx cancel, with graceful shutdown"
// behaviour the agent main wants.
func (p *Probe) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", p.handleLive)
	mux.HandleFunc("/readyz", p.handleReady)
	return mux
}

// Serve binds an http.Server on addr and blocks until ctx is cancelled.
// Returns nil on graceful shutdown, the listener error otherwise.
func (p *Probe) Serve(ctx context.Context, addr string, logger logr.Logger) error {
	if logger.GetSink() == nil {
		logger = log.Log.WithName("agent-health")
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           p.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	shutdown := make(chan error, 1)
	go func() {
		<-ctx.Done()
		// Brief grace period for in-flight handlers; nothing here is
		// long-running so 3 s is plenty.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shutdown <- srv.Shutdown(shutdownCtx)
	}()

	logger.Info("health probe listening", "address", addr)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return <-shutdown
	}
	return err
}

func (p *Probe) handleLive(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (p *Probe) handleReady(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := p.Ready(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "unavailable",
			"reason": err.Error(),
		})
		return
	}
	_, _ = w.Write([]byte(`{"status":"ready"}`))
}
