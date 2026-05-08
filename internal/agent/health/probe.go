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
//   - /readyz returns 200 only when the agent's poll loop has reported a
//     successful tick within PollStaleAfter (default 30 s). Wire the
//     poller's OnPollResult to Probe.RecordPoll so the gate flips red
//     when the broker becomes unreachable or the loop deadlocks.
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

// Options bundles construction-time settings.
type Options struct {
	// PollStaleAfter overrides DefaultPollStaleAfter.
	PollStaleAfter time.Duration

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

	pollStaleAfter time.Duration
	now            func() time.Time
}

// New returns a Probe with no successful polls recorded; /readyz starts
// red and flips green on the first RecordPoll(true).
func New(opts Options) *Probe {
	if opts.PollStaleAfter <= 0 {
		opts.PollStaleAfter = DefaultPollStaleAfter
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Probe{
		pollStaleAfter: opts.PollStaleAfter,
		now:            opts.Now,
	}
}

// RecordPoll is intended for use as the poller.Options.OnPollResult
// callback. Only success bumps the freshness timestamp; failures leave
// it untouched so the staleness check naturally trips after the
// configured window.
func (p *Probe) RecordPoll(success bool) {
	if !success {
		return
	}
	p.lastPollOK.Store(p.now().UnixNano())
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
