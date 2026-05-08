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

package poller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// fakeBroker is a tiny httptest-backed broker shape that records every
// instruction-result POST and serves a configurable list of pending
// instructions on each GET. Goroutine-safe.
type fakeBroker struct {
	t *testing.T

	srv *httptest.Server

	mu                     sync.Mutex
	pendingByPoll          [][]brokerapi.InstructionView // each call to GetInstructions consumes one entry
	receivedResults        []resultRecord
	getInstructionsCalls   int32
	postInstructionResults int32

	// failGetInstructions, if non-zero, makes the next N GetInstructions
	// calls return 503 to exercise the failure path.
	failGetInstructions int32
}

type resultRecord struct {
	instructionID string
	body          brokerapi.InstructionResultRequest
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	fb := &fakeBroker{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/instructions", fb.handleGetInstructions)
	mux.HandleFunc("POST /api/v1/instructions/{id}/result", fb.handlePostResult)
	fb.srv = httptest.NewServer(mux)
	t.Cleanup(fb.srv.Close)
	return fb
}

func (fb *fakeBroker) handleGetInstructions(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&fb.getInstructionsCalls, 1)

	if atomic.LoadInt32(&fb.failGetInstructions) > 0 {
		atomic.AddInt32(&fb.failGetInstructions, -1)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	fb.mu.Lock()
	var batch []brokerapi.InstructionView
	if len(fb.pendingByPoll) > 0 {
		batch = fb.pendingByPoll[0]
		fb.pendingByPoll = fb.pendingByPoll[1:]
	}
	fb.mu.Unlock()

	w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(brokerapi.InstructionsResponse{Instructions: batch})
}

func (fb *fakeBroker) handlePostResult(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&fb.postInstructionResults, 1)
	id := r.PathValue("id")
	body, _ := io.ReadAll(r.Body)
	var req brokerapi.InstructionResultRequest
	_ = json.Unmarshal(body, &req)

	fb.mu.Lock()
	fb.receivedResults = append(fb.receivedResults, resultRecord{instructionID: id, body: req})
	fb.mu.Unlock()

	w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(brokerapi.InstructionResultResponse{Accepted: true})
}

// queue appends a batch to be returned by the next GetInstructions call.
// Empty (nil) batches are valid and represent "no pending instructions".
func (fb *fakeBroker) queue(batch []brokerapi.InstructionView) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.pendingByPoll = append(fb.pendingByPoll, batch)
}

// snapshotResults returns a copy of every result the broker has seen.
func (fb *fakeBroker) snapshotResults() []resultRecord {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	out := make([]resultRecord, len(fb.receivedResults))
	copy(out, fb.receivedResults)
	return out
}

func (fb *fakeBroker) buildClient(t *testing.T) *client.Client {
	t.Helper()
	u, err := url.Parse(fb.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// MaxRetries=1 with microsecond backoff keeps retries effectively
	// instant so a 200 ms test budget covers many ticks. The poller's
	// callback fires once per poll regardless of internal retries.
	c, err := client.New(client.Options{
		BrokerURL:      "https://" + u.Host,
		Transport:      rewriteScheme{base: fb.srv.Client().Transport, target: u},
		MaxRetries:     1,
		InitialBackoff: time.Microsecond,
		MaxBackoff:     time.Microsecond,
		RequestTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// rewriteScheme forwards a request whose URL the test client built with
// scheme=https to the underlying httptest.Server's actual http URL.
type rewriteScheme struct {
	base   http.RoundTripper
	target *url.URL
}

func (rt rewriteScheme) RoundTrip(req *http.Request) (*http.Response, error) {
	cp := *req.URL
	cp.Scheme = rt.target.Scheme
	cp.Host = rt.target.Host
	r2 := req.Clone(req.Context())
	r2.URL = &cp
	return rt.base.RoundTrip(r2)
}

// runOnce builds and starts a Poller, lets it execute exactly one tick,
// then cancels and waits for Run to return. Time-budget keeps tests
// fast — we use a long Interval so the second tick never fires.
func runOnce(t *testing.T, p *Poller) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	// The poll loop runs once immediately on start. Give it a moment to
	// finish, then cancel; Run returns on the next select.
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poll loop did not stop within 1s of ctx cancel")
	}
}

// -----------------------------------------------------------------------------
// Registry
// -----------------------------------------------------------------------------

func TestRegistry_RegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	if r.Len() != 0 {
		t.Fatalf("empty registry: want 0 entries, got %d", r.Len())
	}

	hit := false
	r.RegisterReservation(autoscalingv1alpha1.ReservationInstructionPeer, func(context.Context, *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		hit = true
		return nil, nil
	})
	if got := r.Len(); got != 1 {
		t.Errorf("Len: want 1, got %d", got)
	}

	h, ok := r.Lookup(string(autoscalingv1alpha1.ReservationInstructionPeer))
	if !ok || h == nil {
		t.Fatal("Lookup returned no handler for registered kind")
	}
	_, _ = h(context.Background(), nil)
	if !hit {
		t.Fatal("registered handler was not invoked")
	}

	if _, ok := r.Lookup("Unknown"); ok {
		t.Error("Lookup returned a handler for an unregistered kind")
	}
}

// -----------------------------------------------------------------------------
// New (constructor) validation
// -----------------------------------------------------------------------------

func TestNew_ValidationErrors(t *testing.T) {
	r := NewRegistry()
	r.Register("X", func(context.Context, *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		return nil, nil
	})

	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"missing client", Options{Registry: r}, "Client is required"},
		{"missing registry", Options{Client: &client.Client{}}, "Registry is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Tick: dispatch paths
// -----------------------------------------------------------------------------

func TestPoller_DispatchesRegisteredHandler_AndPostsSuccess(t *testing.T) {
	fb := newFakeBroker(t)
	fb.queue([]brokerapi.InstructionView{
		{ID: "i-1", Kind: string(autoscalingv1alpha1.ReservationInstructionPeer), ReservationID: "res-1"},
	})

	var sawInstruction *brokerapi.InstructionView
	r := NewRegistry()
	r.RegisterReservation(autoscalingv1alpha1.ReservationInstructionPeer, func(_ context.Context, in *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		sawInstruction = in
		return &brokerapi.InstructionResultRequest{
			Status: brokerapi.ResultStatusSucceeded,
			Payload: &brokerapi.ResultPayload{
				Kind:             brokerapi.PayloadKindPeer,
				VirtualNodeNames: []string{"vn-1"},
			},
		}, nil
	})

	p, err := New(Options{Client: fb.buildClient(t), Registry: r, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	runOnce(t, p)

	if sawInstruction == nil || sawInstruction.ID != "i-1" {
		t.Fatalf("handler did not see the instruction: %+v", sawInstruction)
	}
	results := fb.snapshotResults()
	if len(results) != 1 {
		t.Fatalf("want 1 result posted, got %d", len(results))
	}
	if results[0].instructionID != "i-1" || results[0].body.Status != brokerapi.ResultStatusSucceeded {
		t.Errorf("unexpected result record: %+v", results[0])
	}
	if results[0].body.Payload == nil || len(results[0].body.Payload.VirtualNodeNames) != 1 {
		t.Errorf("payload not propagated: %+v", results[0].body.Payload)
	}
}

func TestPoller_NilResult_DefaultsToSucceeded(t *testing.T) {
	fb := newFakeBroker(t)
	fb.queue([]brokerapi.InstructionView{
		{ID: "i-2", Kind: string(autoscalingv1alpha1.ReservationInstructionUnpeer), ReservationID: "res-2"},
	})

	r := NewRegistry()
	r.RegisterReservation(autoscalingv1alpha1.ReservationInstructionUnpeer, func(context.Context, *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		return nil, nil // shorthand for "Succeeded, no payload"
	})

	p, _ := New(Options{Client: fb.buildClient(t), Registry: r, Interval: time.Hour})
	runOnce(t, p)

	results := fb.snapshotResults()
	if len(results) != 1 || results[0].body.Status != brokerapi.ResultStatusSucceeded {
		t.Fatalf("want one Succeeded result, got %+v", results)
	}
	if results[0].body.Payload != nil {
		t.Errorf("expected nil payload, got %+v", results[0].body.Payload)
	}
}

func TestPoller_HandlerError_ReportedAsFailed(t *testing.T) {
	fb := newFakeBroker(t)
	fb.queue([]brokerapi.InstructionView{
		{ID: "i-3", Kind: string(autoscalingv1alpha1.ReservationInstructionPeer)},
	})

	r := NewRegistry()
	r.RegisterReservation(autoscalingv1alpha1.ReservationInstructionPeer, func(context.Context, *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		return nil, &handlerErr{msg: "boom: liqoctl peer crashed"}
	})

	p, _ := New(Options{Client: fb.buildClient(t), Registry: r, Interval: time.Hour})
	runOnce(t, p)

	results := fb.snapshotResults()
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].body.Status != brokerapi.ResultStatusFailed {
		t.Errorf("expected Failed, got %s", results[0].body.Status)
	}
	if results[0].body.Error == nil || !strings.Contains(results[0].body.Error.Message, "boom") {
		t.Errorf("error envelope missing or wrong: %+v", results[0].body.Error)
	}
}

func TestPoller_HandlerPanic_DoesNotKillLoop(t *testing.T) {
	fb := newFakeBroker(t)
	// First tick has the panicking instruction; second tick has a clean one.
	// runOnce only triggers one tick, so we'll do a longer multi-tick test.
	fb.queue([]brokerapi.InstructionView{
		{ID: "i-panic", Kind: string(autoscalingv1alpha1.ReservationInstructionPeer)},
	})
	fb.queue([]brokerapi.InstructionView{
		{ID: "i-after", Kind: string(autoscalingv1alpha1.ReservationInstructionPeer)},
	})

	var afterSeen atomic.Bool
	r := NewRegistry()
	r.RegisterReservation(autoscalingv1alpha1.ReservationInstructionPeer, func(_ context.Context, in *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		if in.ID == "i-panic" {
			panic("handler exploded")
		}
		afterSeen.Store(true)
		return nil, nil
	})

	p, _ := New(Options{Client: fb.buildClient(t), Registry: r, Interval: 50 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	// Give enough time for two ticks (initial + one ticker fire).
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	if !afterSeen.Load() {
		t.Fatal("loop did not survive the panicking handler — second tick never ran")
	}
	results := fb.snapshotResults()
	// Two results posted: a Failed for i-panic, a Succeeded for i-after.
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d: %+v", len(results), results)
	}
	if results[0].instructionID != "i-panic" || results[0].body.Status != brokerapi.ResultStatusFailed {
		t.Errorf("first result should be Failed for i-panic: %+v", results[0])
	}
	if results[1].instructionID != "i-after" || results[1].body.Status != brokerapi.ResultStatusSucceeded {
		t.Errorf("second result should be Succeeded for i-after: %+v", results[1])
	}
}

func TestPoller_UnknownKind_ReportedAsFailed(t *testing.T) {
	fb := newFakeBroker(t)
	fb.queue([]brokerapi.InstructionView{
		{ID: "i-mystery", Kind: "MysteryKind"},
	})

	r := NewRegistry()
	r.RegisterReservation(autoscalingv1alpha1.ReservationInstructionPeer, func(context.Context, *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		return nil, nil
	})

	p, _ := New(Options{Client: fb.buildClient(t), Registry: r, Interval: time.Hour})
	runOnce(t, p)

	results := fb.snapshotResults()
	if len(results) != 1 || results[0].body.Status != brokerapi.ResultStatusFailed {
		t.Fatalf("want one Failed result, got %+v", results)
	}
	if !strings.Contains(results[0].body.Error.Message, "MysteryKind") {
		t.Errorf("error message should reference the unknown kind, got %q", results[0].body.Error.Message)
	}
}

func TestPoller_OnPollResultCallback(t *testing.T) {
	fb := newFakeBroker(t)
	// With MaxRetries=1 each poll makes up to 2 raw HTTP attempts. Two
	// raw failures means poll #1 fails entirely; poll #2+ succeed.
	atomic.StoreInt32(&fb.failGetInstructions, 2)

	r := NewRegistry()
	r.RegisterReservation(autoscalingv1alpha1.ReservationInstructionPeer, func(context.Context, *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		return nil, nil
	})

	var seen []bool
	var mu sync.Mutex
	p, _ := New(Options{
		Client:   fb.buildClient(t),
		Registry: r,
		Interval: 30 * time.Millisecond,
		OnPollResult: func(success bool) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, success)
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	// Allow several ticks: one failure, then successes.
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("want >=2 callback invocations, got %d", len(seen))
	}
	if seen[0] {
		t.Errorf("expected first callback to report failure, got %v", seen)
	}
	// At least one later callback must have reported success.
	sawSuccess := false
	for _, ok := range seen[1:] {
		if ok {
			sawSuccess = true
			break
		}
	}
	if !sawSuccess {
		t.Errorf("expected at least one later callback to report success, got %v", seen)
	}
}

func TestPoller_ContextCancellation_StopsPromptly(t *testing.T) {
	fb := newFakeBroker(t)

	r := NewRegistry()
	r.RegisterReservation(autoscalingv1alpha1.ReservationInstructionPeer, func(context.Context, *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		return nil, nil
	})

	p, _ := New(Options{Client: fb.buildClient(t), Registry: r, Interval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s of ctx cancel")
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

type handlerErr struct{ msg string }

func (e *handlerErr) Error() string { return e.msg }
