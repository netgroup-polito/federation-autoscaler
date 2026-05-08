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
// GET /api/v1/instructions when Options.Interval is unset (docs/design.md
// §7.3.6 fixes this at 5 s as an upper-bound fallback to the
// piggyback-on-advertisement path).
const DefaultInterval = 5 * time.Second

// Options bundles the construction-time settings of a Poller.
type Options struct {
	// Client is the Broker HTTP client built in step 7a/7b. Required.
	Client *client.Client

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
}

// Poller drives the 5 s GET /api/v1/instructions loop. A single Run
// goroutine is the only entry point; reflecting the single-replica
// Recreate invariant of agent Deployments, Poller is NOT safe for
// concurrent invocation.
type Poller struct {
	client       *client.Client
	registry     *Registry
	interval     time.Duration
	log          logr.Logger
	onPollResult func(success bool)
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
		client:       opts.Client,
		registry:     opts.Registry,
		interval:     interval,
		log:          logger,
		onPollResult: opts.OnPollResult,
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
	resp, err := p.client.GetInstructions(ctx)
	if err != nil {
		p.notifyPollResult(false)
		// Cancellations are an expected stop signal, not a fault.
		if ctx.Err() == nil {
			p.log.V(1).Info("GetInstructions failed", "err", err.Error())
		}
		return
	}
	p.notifyPollResult(true)

	for i := range resp.Instructions {
		// Re-check ctx between instructions so a cancellation mid-tick
		// is honoured promptly without dispatching the rest of the batch.
		if ctx.Err() != nil {
			return
		}
		p.dispatch(ctx, &resp.Instructions[i])
	}
}

// dispatch resolves the right handler for in.Kind, runs it under a
// panic guard, and posts the result back to the broker. A handler that
// returns nil-result-and-nil-error gets a default Succeeded posted on
// its behalf.
func (p *Poller) dispatch(ctx context.Context, in *brokerapi.InstructionView) {
	logger := p.log.WithValues("instructionId", in.ID, "kind", in.Kind, "reservationId", in.ReservationID)

	handler, ok := p.registry.Lookup(in.Kind)
	if !ok {
		logger.Info("no handler registered for instruction kind")
		p.postResult(ctx, in.ID, &brokerapi.InstructionResultRequest{
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

	p.postResult(ctx, in.ID, result, logger)
}

// postResult sends the result envelope to the broker. Failures here are
// logged but not retried at this layer — the broker keeps re-emitting
// the instruction until Status.Enforced flips, so the next tick will
// pick it up again.
func (p *Poller) postResult(ctx context.Context, instructionID string, result *brokerapi.InstructionResultRequest, logger logr.Logger) {
	if err := p.client.PostInstructionResult(ctx, instructionID, result); err != nil {
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
