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
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// DefaultInterval is the cadence at which the poller calls
// GET /api/v1/instructions when Options.Interval is unset.
//
// 1 s is the measured knee of the latency-vs-request-rate curve, not a guess.
// Sweeping 5s / 2s / 1s / 500ms / 250ms over 100 runs (the harness's sweep.sh):
//
//	5s → 2s   saves 3.68 s of control-plane latency for +0.73 req/s   (5.04 s per req/s)
//	2s → 1s   saves 1.20 s                          for +1.22 req/s   (0.98 s per req/s)
//	1s → 500ms saves 0.24 s                         for +2.43 req/s   (0.10 s per req/s)
//
// Marginal value collapses by an order of magnitude at each step past 1 s. The
// cost side stays benign well beyond it — no 429 was observed even at 250 ms,
// where an agent draws 4 req/s against its 5 req/s per-cluster budget — so the
// stopping point is set by diminishing returns rather than by a limit.
//
// A work order waits on average half this interval before an agent picks it up,
// and a cold scale-up pays that twice (provider hop, then consumer hop). Lower
// values are safe down to ~200 ms, where the rate limiter finally binds.
const DefaultInterval = time.Second

// Options bundles the construction-time settings of a Poller.
type Options struct {
	// Client is the Broker HTTP client built in step 7a/7b. Required.
	Client *client.Client

	// Sources, when set, lists every broker to poll, polled one after the
	// other on each tick (the consumer's home broker first, then the region
	// orchestrators it has booked at). Each instruction's result goes back to
	// the broker that sent it, and handlers still run one at a time. Nil:
	// only Client.
	Sources func() []*client.Client

	// Registry holds the per-role handler set. Required, and must
	// contain at least one entry — an empty registry would just spam the
	// broker with "no handler" Failed results.
	Registry *Registry

	// Interval overrides DefaultInterval. Mainly useful in tests.
	Interval time.Duration

	// Logger is the structured logger the poll loop logs through.
	// Defaults to controller-runtime's logger named "agent-poller".
	Logger logr.Logger

	// OnPollResult, if non-nil, is invoked after every GetInstructions
	// call with the success/failure outcome. The agent's health probe
	// (step 7d) wires its readiness gate through this hook so /readyz
	// flips red when the broker becomes unreachable.
	OnPollResult func(success bool)

	// OnHandlerActive, if non-nil, is invoked with true immediately before
	// an instruction handler runs and false once it returns. Handlers are
	// dispatched synchronously on the poll goroutine, so a slow one
	// (liqoctl peer takes 40-90 s) blocks the next poll entirely; the
	// agent's health probe wires this to RecordHandlerActive so a working
	// loop is not mistaken for a stalled one.
	OnHandlerActive func(active bool)
}

// Poller drives the 5 s GET /api/v1/instructions loop. A single Run
// goroutine is the only entry point; reflecting the single-replica
// Recreate invariant of agent Deployments, Poller is NOT safe for
// concurrent invocation.
type Poller struct {
	client          *client.Client
	sources         func() []*client.Client
	registry        *Registry
	interval        time.Duration
	log             logr.Logger
	onPollResult    func(success bool)
	onHandlerActive func(active bool)
}

// New validates opts and returns a Poller ready to Run. It performs no
// I/O.
//
// An empty Registry is permitted: the poller still drives the
// GET-instructions / readiness signal even before the agent's role
// (step 8 / 9) has registered handlers. Any instruction the broker
// returns in that state is reported back as Failed with "no handler for
// kind X" so operators see the misconfiguration.
func New(opts Options) (*Poller, error) {
	if opts.Client == nil {
		return nil, errors.New("poller: Client is required")
	}
	if opts.Registry == nil {
		return nil, errors.New("poller: Registry is required")
	}

	logger := opts.Logger
	if logger.GetSink() == nil {
		logger = log.Log.WithName("agent-poller")
	}

	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}

	return &Poller{
		client:          opts.Client,
		sources:         opts.Sources,
		registry:        opts.Registry,
		interval:        interval,
		log:             logger,
		onPollResult:    opts.OnPollResult,
		onHandlerActive: opts.OnHandlerActive,
	}, nil
}

// Run blocks until ctx is cancelled. It performs an initial poll before
// the first interval elapses so the agent's readyz gate can flip green
// without waiting a full tick at start-up.
func (p *Poller) Run(ctx context.Context) {
	p.log.Info("starting poll loop", "interval", p.interval, "handlers", p.registry.Len())

	t := time.NewTicker(p.interval)
	defer t.Stop()

	p.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			p.log.Info("poll loop stopped", "reason", ctx.Err())
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}

// tick executes one fetch-dispatch-report cycle. Errors are logged but
// not propagated: the loop must keep running across transient broker
// outages or per-instruction failures so a returning broker finds a
// healthy agent waiting.
func (p *Poller) tick(ctx context.Context) {
	sources := []*client.Client{p.client}
	if p.sources != nil {
		sources = p.sources()
	}
	for i, c := range sources {
		if ctx.Err() != nil {
			return
		}
		// The first broker (the home broker) drives the health probe and the
		// provider's reserved-chunk count; the others are best effort.
		p.pollOne(ctx, c, i == 0)
	}
}

// pollOne fetches one broker's instructions and runs them, answering on the
// same broker.
func (p *Poller) pollOne(ctx context.Context, c *client.Client, home bool) {
	resp, err := c.GetInstructions(ctx)
	if err != nil {
		if home {
			p.notifyPollResult(false)
		}
		// Cancellations are an expected stop signal, not a fault.
		if ctx.Err() == nil {
			p.log.V(1).Info("GetInstructions failed", "home", home, "err", err.Error())
		}
		return
	}
	if home {
		p.notifyPollResult(true)
		// Tell the role about the reserved capacity before the handlers run: a
		// handler can take a long time and the role should not wait for it.
		if resp.ReservedChunks != nil {
			p.registry.reservedChunks(*resp.ReservedChunks)
		}
	}

	for i := range resp.Instructions {
		// Re-check ctx between instructions so a cancellation mid-tick
		// is honoured promptly without dispatching the rest of the batch.
		if ctx.Err() != nil {
			return
		}
		p.dispatch(ctx, c, &resp.Instructions[i])
	}
}

// dispatch resolves the right handler for in.Kind, runs it under a
// panic guard, and posts the result back to the broker. A handler that
// returns nil-result-and-nil-error gets a default Succeeded posted on
// its behalf.
func (p *Poller) dispatch(ctx context.Context, c *client.Client, in *brokerapi.InstructionView) {
	// Bracket the WHOLE dispatch, not just the handler call, so every return
	// path stays balanced — including the "no handler registered" early exit
	// and the postResult round-trip below.
	p.notifyHandlerActive(true)
	defer p.notifyHandlerActive(false)

	logger := p.log.WithValues("instructionId", in.ID, "kind", in.Kind, "reservationId", in.ReservationID)

	// Timing pair (received / handled). The broker stamps
	// Status.LastDeliveredAt when it hands an instruction out and
	// Status.LastUpdateTime when the result lands, which already bounds the
	// handler — but both are metav1.Time, i.e. whole seconds. These two lines
	// carry the agent's millisecond clock, so the benchmark harness can separate the
	// poll wait from the handler run at the resolution the fast phases need.
	// "received" is emitted for every kind, including ones with no handler, so
	// a misconfigured agent still shows up on the timeline.
	dispatchStart := time.Now()
	logger.Info("timing", "event", "instruction.received")

	handler, ok := p.registry.Lookup(in.Kind)
	if !ok {
		logger.Info("no handler registered for instruction kind")
		p.postResult(ctx, c, in.ID, &brokerapi.InstructionResultRequest{
			Status: brokerapi.ResultStatusFailed,
			Error: &brokerapi.ErrorEnvelope{
				Code:    brokerapi.ErrCodeInvalidRequest,
				Message: fmt.Sprintf("agent has no handler for kind %q", in.Kind),
			},
		}, logger)
		return
	}

	result, err := safeHandle(ctx, handler, in)
	switch {
	case err != nil:
		logger.Info("handler returned error", "err", err.Error())
		result = &brokerapi.InstructionResultRequest{
			Status: brokerapi.ResultStatusFailed,
			Error: &brokerapi.ErrorEnvelope{
				Code:    brokerapi.ErrCodeInternalError,
				Message: err.Error(),
			},
		}
	case result == nil:
		// Handler signalled "nothing to report, no payload" — default
		// to a bare Succeeded. Saves every handler from constructing
		// the same envelope.
		result = &brokerapi.InstructionResultRequest{Status: brokerapi.ResultStatusSucceeded}
	}

	logger.Info("timing",
		"event", "instruction.handled",
		"status", string(result.Status),
		"elapsedMs", time.Since(dispatchStart).Milliseconds())

	p.postResult(ctx, c, in.ID, result, logger)
}

// postResult sends the result envelope to the broker. Failures here are
// logged but not retried at this layer — the broker keeps re-emitting
// the instruction until Status.Enforced flips, so the next tick will
// pick it up again.
func (p *Poller) postResult(
	ctx context.Context, c *client.Client, instructionID string,
	result *brokerapi.InstructionResultRequest, logger logr.Logger,
) {
	if err := c.PostInstructionResult(ctx, instructionID, result); err != nil {
		logger.Info("post instruction result failed", "err", err.Error())
	}
}

func (p *Poller) notifyPollResult(success bool) {
	if p.onPollResult == nil {
		return
	}
	defer func() { _ = recover() }() // never let a misbehaving callback kill the loop
	p.onPollResult(success)
}

// notifyHandlerActive reports that an instruction handler started (true) or
// returned (false). Mirrors notifyPollResult: nil callback is a no-op, and a
// panicking callback can never kill the poll loop.
func (p *Poller) notifyHandlerActive(active bool) {
	if p.onHandlerActive == nil {
		return
	}
	defer func() { _ = recover() }() // never let a misbehaving callback kill the loop
	p.onHandlerActive(active)
}

// safeHandle wraps a handler call in a panic recovery so a single bad
// instruction (or buggy handler) never brings the agent down. Any panic
// is converted into an error the dispatcher then reports as Failed.
func safeHandle(
	ctx context.Context, h HandlerFunc, in *brokerapi.InstructionView,
) (result *brokerapi.InstructionResultRequest, err error) {
	defer func() {
		if r := recover(); r != nil {
			result = nil
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return h(ctx, in)
}
