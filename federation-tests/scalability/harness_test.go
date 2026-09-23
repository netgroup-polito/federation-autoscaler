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
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
)

// A request the harness cancelled is not a Broker error; a deadline is a
// timeout; an HTTP error is a failure.
func TestClassify(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want Outcome
	}{
		"cancelled in flight": {&agentclient.Error{Category: agentclient.CategoryTransient,
			Cause: fmt.Errorf("Post: %w", context.Canceled)}, OutcomeCancelled},
		"cancelled during backoff": {&agentclient.Error{Category: agentclient.CategoryTransient,
			Message: "context cancelled during retry backoff", Cause: context.Canceled}, OutcomeCancelled},
		"deadline": {&agentclient.Error{Category: agentclient.CategoryTransient,
			Cause: fmt.Errorf("Post: %w", context.DeadlineExceeded)}, OutcomeTimeout},
		"HTTP 500":    {&agentclient.Error{Category: agentclient.CategoryTransient, Status: 500}, OutcomeFailure},
		"plain error": {errors.New("boom"), OutcomeFailure},
	} {
		t.Run(name, func(t *testing.T) {
			if got, _, _, _ := classify(tc.err); got != tc.want {
				t.Errorf("classify = %s, want %s", got, tc.want)
			}
		})
	}
}

// Cancelled requests are counted, but neither as attempts nor as errors.
func TestComputeStats_CancelledIsNotAnError(t *testing.T) {
	rec := func(o Outcome) Record {
		return Record{Operation: OpEvaluation, Phase: PhaseMeasurement, Outcome: o, LatencyMS: 1}
	}
	records := []Record{rec(OutcomeSuccess), rec(OutcomeSuccess), rec(OutcomeFailure), rec(OutcomeCancelled)}
	st := computeStats(records, OpEvaluation, time.Minute)
	if st.Attempts != 3 || st.Cancelled != 1 || st.Failures != 1 {
		t.Errorf("attempts %d, cancelled %d, failures %d; want 3, 1, 1", st.Attempts, st.Cancelled, st.Failures)
	}
	if math.Abs(st.ErrorRate-1.0/3) > 1e-9 {
		t.Errorf("error rate = %v, want 1/3", st.ErrorRate)
	}
}

// Agents started together must not fire together: each loop starts at its
// own point of the first interval, reproducibly from the seed.
func TestStartOffset(t *testing.T) {
	const interval = 5 * time.Second
	seen := map[time.Duration]bool{}
	for i := 1; i <= 50; i++ {
		off := startOffset(42, "consumer", i, OpEvaluation, interval)
		if off < 0 || off >= interval {
			t.Fatalf("agent %d: offset %s outside [0, %s)", i, off, interval)
		}
		if off != startOffset(42, "consumer", i, OpEvaluation, interval) {
			t.Fatalf("agent %d: the same seed gave a different offset", i)
		}
		seen[off] = true
	}
	if len(seen) < 45 {
		t.Errorf("50 agents share only %d distinct offsets: they would still fire together", len(seen))
	}
	if startOffset(42, "consumer", 1, OpEvaluation, interval) == startOffset(42, "consumer", 1, OpHeartbeat, interval) &&
		startOffset(42, "consumer", 2, OpEvaluation, interval) == startOffset(42, "consumer", 2, OpHeartbeat, interval) {
		t.Error("an agent's operations must not all start at the same offset")
	}
}

func TestStaggeredTicker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	c := staggeredTicker(ctx, 40*time.Millisecond, 100*time.Millisecond)

	first := <-c
	if got := first.Sub(start); got < 90*time.Millisecond {
		t.Errorf("first tick after %s, want about the 100ms offset", got)
	}
	second := <-c
	// Generous upper bound: CI runners are shared and can be slow to schedule.
	if got := second.Sub(first); got < 30*time.Millisecond || got > time.Second {
		t.Errorf("second tick %s after the first, want about the 40ms interval", got)
	}

	cancel()
	time.Sleep(100 * time.Millisecond)
	select {
	case <-c: // at most one tick may have been buffered before the cancel
	default:
	}
	select {
	case <-c:
		t.Error("the ticker kept ticking after its context ended")
	case <-time.After(150 * time.Millisecond):
	}
}

// The end of the run waits for a request in flight through every attempt the
// client can make, with the client's own rule for a retry count of 0.
func TestDrainTimeout(t *testing.T) {
	cfg := &Config{RequestTimeout: 10 * time.Second}
	// 0 means the client's default: 3 retries, so 4 attempts and 3 backoffs.
	want := 4*10*time.Second + 3*agentclient.DefaultMaxBackoff + 5*time.Second
	if got := drainTimeout(cfg); got != want {
		t.Errorf("0 retries: drainTimeout = %s, want %s", got, want)
	}
	cfg.ClientMaxRetries = 2
	want = 3*10*time.Second + 2*agentclient.DefaultMaxBackoff + 5*time.Second
	if got := drainTimeout(cfg); got != want {
		t.Errorf("2 retries: drainTimeout = %s, want %s", got, want)
	}
}

// /proc/<pid>/stat: the command name may contain spaces and parentheses, so
// utime and stime are counted from the last ')'.
func TestProcCPUTicksAndRSS(t *testing.T) {
	files := map[string]string{
		"/proc/7/stat":   "7 (my (weird) broker) S 1 7 7 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 12 0 1000 1 1",
		"/proc/7/status": "Name:\tbroker\nVmPeak:\t  900000 kB\nVmRSS:\t  51200 kB\nThreads:\t12\n",
	}
	defer swapProcReadFile(files)()

	ticks, err := procCPUTicks(7)
	if err != nil || ticks != 300 {
		t.Errorf("ticks = %d, %v; want utime 250 + stime 50 = 300", ticks, err)
	}
	rss, err := procRSSMiB(7)
	if err != nil || rss != 50 {
		t.Errorf("rss = %v MiB, %v; want 51200 kB = 50 MiB", rss, err)
	}
	if _, err := procCPUTicks(8); err == nil {
		t.Error("a missing process must be an error, not a zero")
	}
}

// CPU is what the process used between two readings: 150 ticks (1.5 s of
// CPU) over 3 s of wall time is half a core, not a lifetime average.
func TestCPUCores(t *testing.T) {
	if got := cpuCores(1000, 1150, 3*time.Second); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("cpuCores = %v, want 0.5", got)
	}
	if got := cpuCores(1000, 1000, 0); got != 0 {
		t.Errorf("no elapsed time must give 0, got %v", got)
	}
}

func TestProcessSampler(t *testing.T) {
	stat := func(utime int) string {
		return fmt.Sprintf("9 (broker) S 1 9 9 0 -1 0 0 0 0 0 %d 0 0 0 20 0 1 0 1 1 1", utime)
	}
	files := map[string]string{"/proc/9/stat": stat(100), "/proc/9/status": "VmRSS:\t 2048 kB\n"}
	defer swapProcReadFile(files)()

	sample, err := processSampler(9)
	if err != nil {
		t.Fatal(err)
	}
	files["/proc/9/stat"] = stat(150)
	time.Sleep(20 * time.Millisecond) // some wall time between readings (Windows clocks are coarse)
	s, err := sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUUnit != "cores" || s.CPUValue <= 0 || s.MemMiB != 2 {
		t.Errorf("sample = %+v, want a positive CPU in cores and 2 MiB", s)
	}

	if _, err := processSampler(10); err == nil || !strings.Contains(err.Error(), "needs Linux") {
		t.Errorf("a PID with no /proc entry must fail up front with a clear message, got %v", err)
	}
}

func swapProcReadFile(files map[string]string) func() {
	prev := procReadFile
	procReadFile = func(name string) ([]byte, error) {
		if s, ok := files[name]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	return func() { procReadFile = prev }
}

// The end of the measurement stops new requests but lets the one in flight
// finish: it must be recorded as the success it is, not as a Broker error.
func TestRunProvider_StopLetsTheRequestInFlightFinish(t *testing.T) {
	var calls atomic.Int32
	inFlight, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 { // the warm-up answers at once; the next one is held open
			once.Do(func() { close(inFlight) })
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	prev := newAgentClient
	newAgentClient = func(cfg *Config, _ agentIdentity, _ string) (*agentclient.Client, error) {
		return agentclient.New(agentclient.Options{BrokerURL: srv.URL, Transport: srv.Client().Transport,
			RequestTimeout: cfg.RequestTimeout, MaxRetries: 1})
	}
	defer func() { newAgentClient = prev }()

	cfg := &Config{AdvertisementInterval: 20 * time.Millisecond, WarmupTimeout: 5 * time.Second,
		RequestTimeout: 5 * time.Second, ProviderCPU: "4", ProviderMemory: "8Gi", Seed: 1}
	genCtx, stopGenerators := context.WithCancel(context.Background())
	collector := NewCollector()
	warmup := make(chan warmupOutcome, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProvider(genCtx, context.Background(), cfg, agentIdentity{ClusterID: "scaltest-provider-001"}, 1,
			collector, warmup)
	}()

	select {
	case <-inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("no measurement request reached the server")
	}
	stopGenerators() // the measurement ends while that request is still open
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the provider loop did not return after the stop")
	}

	var measured int
	for _, r := range collector.Snapshot() {
		if r.Outcome != OutcomeSuccess {
			t.Errorf("record %s/%s ended %s (%s): the stop must not turn it into an error",
				r.Operation, r.Phase, r.Outcome, r.ErrorMessage)
		}
		if r.Phase == PhaseMeasurement {
			measured++
		}
	}
	if measured != 1 {
		t.Errorf("%d measurement records, want exactly the one in flight at the stop", measured)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("the server saw %d requests, want the warm-up and the one in flight: none may start after the stop", n)
	}
}
