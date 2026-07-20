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

package health

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// fakeClock returns successive timestamps controlled by the test. It is
// safe for concurrent use; each call advances the clock by `step`.
type fakeClock struct {
	now  atomic.Int64 // unix nanos
	step time.Duration
}

func newFakeClock(start time.Time, step time.Duration) *fakeClock {
	c := &fakeClock{step: step}
	c.now.Store(start.UnixNano())
	return c
}

func (c *fakeClock) Now() time.Time {
	return time.Unix(0, c.now.Load())
}

// Advance pushes the clock forward by d.
func (c *fakeClock) Advance(d time.Duration) {
	c.now.Add(d.Nanoseconds())
}

// -----------------------------------------------------------------------------
// Ready / RecordPoll behaviour
// -----------------------------------------------------------------------------

func TestProbe_NotReadyBeforeFirstPoll(t *testing.T) {
	p := New(Options{})
	if err := p.Ready(); err == nil {
		t.Fatal("expected error before any successful poll")
	} else if !strings.Contains(err.Error(), "not completed a successful poll") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestProbe_ReadyAfterSuccessfulPoll(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0), time.Second)
	p := New(Options{Now: clk.Now, PollStaleAfter: 10 * time.Second})

	p.RecordPoll(true)
	if err := p.Ready(); err != nil {
		t.Fatalf("expected ready after successful poll, got: %v", err)
	}
}

func TestProbe_NotReadyAfterStalenessElapsed(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0), time.Second)
	p := New(Options{Now: clk.Now, PollStaleAfter: 5 * time.Second})

	p.RecordPoll(true)
	if err := p.Ready(); err != nil {
		t.Fatalf("should be ready right after poll: %v", err)
	}

	clk.Advance(6 * time.Second)
	err := p.Ready()
	if err == nil {
		t.Fatal("expected stale error after 6s with 5s threshold")
	}
	if !strings.Contains(err.Error(), "threshold 5s") {
		t.Errorf("error should mention threshold; got %q", err.Error())
	}
}

func TestProbe_RecordPoll_FalseDoesNotUpdate(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0), time.Second)
	p := New(Options{Now: clk.Now, PollStaleAfter: 5 * time.Second})

	p.RecordPoll(true)
	clk.Advance(3 * time.Second)
	p.RecordPoll(false) // must NOT bump the freshness timestamp
	clk.Advance(3 * time.Second)
	// Now 6s past the original successful poll → should be stale.
	if err := p.Ready(); err == nil {
		t.Fatal("expected stale error; RecordPoll(false) must not refresh the timestamp")
	}
}

func TestProbe_DefaultPollStaleAfter(t *testing.T) {
	p := New(Options{})
	// We can't read the field directly; check via threshold message.
	// Burn the timestamp to "very old", then read the error.
	p.lastPollOK.Store(time.Unix(0, 0).UnixNano() + 1)
	err := p.Ready()
	if err == nil || !strings.Contains(err.Error(), "30s") {
		t.Fatalf("default threshold should be 30s; got %v", err)
	}
}

func TestProbe_LiveAlwaysOK(t *testing.T) {
	p := New(Options{})
	if err := p.Live(); err != nil {
		t.Fatalf("Live should always return nil; got %v", err)
	}
}

// -----------------------------------------------------------------------------
// RecordHandlerActive: a working poll loop must not read as a stalled one
// -----------------------------------------------------------------------------

// Instruction handlers run synchronously on the poll goroutine, so a slow one
// starves RecordPoll for its whole duration. That must not close the readiness
// gate — losing the agent's Service endpoint mid-scale-up costs CA its entire
// provider surface.
func TestProbe_ReadyWhileHandlerInFlight(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0), time.Second)
	p := New(Options{Now: clk.Now, PollStaleAfter: 5 * time.Second})

	p.RecordPoll(true)
	p.RecordHandlerActive(true) // e.g. liqoctl peer starts

	clk.Advance(31 * time.Second) // far past the staleness threshold
	if err := p.Ready(); err != nil {
		t.Fatalf("a poll loop blocked RUNNING a handler must stay ready; got %v", err)
	}
}

// The gap between two instructions of one batch: inFlight returns to 0 while
// the last real poll is still stale. Finishing a handler is itself proof the
// loop is alive, so the gate must not flap red in that window.
func TestProbe_HandlerFinishRefreshesFreshness(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0), time.Second)
	p := New(Options{Now: clk.Now, PollStaleAfter: 5 * time.Second})

	p.RecordPoll(true)
	p.RecordHandlerActive(true)
	clk.Advance(31 * time.Second)
	p.RecordHandlerActive(false) // returned; the next instruction hasn't started

	if err := p.Ready(); err != nil {
		t.Fatalf("finishing a handler must refresh freshness; got %v", err)
	}

	// ...and the refreshed stamp still decays normally once the loop is idle.
	clk.Advance(31 * time.Second)
	if err := p.Ready(); err == nil {
		t.Fatal("an idle loop must still go stale after the threshold")
	}
}

// Live() always returns nil, so the bounded suppression is the only signal that
// would ever surface a handler wedged beyond any plausible runtime.
func TestProbe_WedgedHandlerEventuallyNotReady(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0), time.Second)
	p := New(Options{
		Now:                clk.Now,
		PollStaleAfter:     5 * time.Second,
		MaxHandlerInFlight: time.Minute,
	})

	p.RecordPoll(true)
	p.RecordHandlerActive(true)

	clk.Advance(59 * time.Second)
	if err := p.Ready(); err != nil {
		t.Fatalf("still inside MaxHandlerInFlight, must be ready; got %v", err)
	}

	clk.Advance(2 * time.Second) // 61s > the 1m bound
	err := p.Ready()
	if err == nil {
		t.Fatal("a handler wedged past MaxHandlerInFlight must close the gate")
	}
	if !strings.Contains(err.Error(), "handler has been running") {
		t.Errorf("error should name the wedged handler; got %q", err.Error())
	}
}

func TestProbe_HandlerCounterBalances(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0), time.Second)
	p := New(Options{Now: clk.Now, PollStaleAfter: 5 * time.Second})

	p.RecordPoll(true)
	p.RecordHandlerActive(true)
	p.RecordHandlerActive(true) // overlapping/nested
	p.RecordHandlerActive(false)

	clk.Advance(31 * time.Second)
	if err := p.Ready(); err != nil {
		t.Fatalf("one handler is still in flight, must stay ready; got %v", err)
	}

	p.RecordHandlerActive(false) // now idle
	clk.Advance(31 * time.Second)
	if err := p.Ready(); err == nil {
		t.Fatal("expected stale once every handler finished and the loop went idle")
	}

	// An unbalanced extra finish must not drive the counter negative — that
	// would permanently disable the suppression for the process lifetime.
	p.RecordHandlerActive(false)
	p.RecordHandlerActive(true)
	clk.Advance(31 * time.Second)
	if err := p.Ready(); err != nil {
		t.Fatalf("counter must clamp at zero so suppression still works; got %v", err)
	}
}

// -----------------------------------------------------------------------------
// HTTP behaviour
// -----------------------------------------------------------------------------

func TestProbe_HTTPHealthz(t *testing.T) {
	p := New(Options{})
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestProbe_HTTPReadyz_NotReady(t *testing.T) {
	p := New(Options{})
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 before any poll, got %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "unavailable" || !strings.Contains(body["reason"], "not completed") {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestProbe_HTTPReadyz_Ready(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0), 0)
	p := New(Options{Now: clk.Now, PollStaleAfter: 10 * time.Second})
	p.RecordPoll(true)

	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after successful poll, got %d", resp.StatusCode)
	}
}

func TestProbe_HTTPReadyz_FlipsRedAfterStaleness(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0), 0)
	p := New(Options{Now: clk.Now, PollStaleAfter: 5 * time.Second})
	p.RecordPoll(true)
	clk.Advance(6 * time.Second)

	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 after staleness, got %d", resp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// Serve lifecycle
// -----------------------------------------------------------------------------

func TestProbe_Serve_StartsAndStops(t *testing.T) {
	p := New(Options{})
	addr := pickAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- p.Serve(ctx, addr, logr.Discard())
	}()

	// Wait briefly for the listener to bind.
	if err := waitForReady(addr, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get("http://" + addr + "/healthz") //nolint:noctx
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error on shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit within 2s of ctx cancel")
	}
}

// pickAddr returns a free localhost address. We bind, capture the port,
// and immediately close so Serve can rebind it. Tests using this helper
// race against any other process grabbing the port in between, but the
// window is sub-millisecond on Linux and acceptable for unit tests.
func pickAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// waitForReady polls until /healthz responds 200 or timeout elapses.
func waitForReady(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz") //nolint:noctx
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("server did not become ready within timeout")
}
