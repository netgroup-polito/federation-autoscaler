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

// Package poller implements the agent-side instruction-poll loop. It
// wakes every interval, asks the Broker for outstanding instructions
// targeted at this caller, and dispatches each one to a handler the
// agent role registered at startup. The result is posted back to the
// Broker so the corresponding instruction CR's Status.Enforced flips to
// true and the broker stops re-emitting the same instruction.
//
// The poller is the only component that runs a goroutine in the agent;
// keeping a single instance per process is what makes the idempotency
// model in docs/design.md §7.0 hold (the agent Deployment is
// strategy=Recreate, single-replica — never add another poller).
package poller

import (
	"context"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// HandlerFunc executes one Broker-issued instruction. Returning a nil
// result with nil error is shorthand for "Succeeded with no payload".
// Returning a non-nil error is reported to the Broker as
// ResultStatusFailed; returning a non-nil result is sent verbatim, so a
// handler can choose to return Failed itself when it needs to attach a
// specific error code or payload (e.g. to surface a parseable
// liqoctl-error envelope).
type HandlerFunc func(ctx context.Context, instruction *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error)

// Registry maps a Broker InstructionView.Kind string to the agent-side
// handler that should execute it. The provider role registers
// GenerateKubeconfig / Cleanup / Reconcile; the consumer role registers
// Peer / Unpeer / Cleanup / Reconcile. The two enums overlap on Cleanup
// and Reconcile by name, but a single Registry is owned by a single-role
// agent at startup so the overlap is invisible at runtime.
type Registry struct {
	handlers map[string]HandlerFunc
}

// NewRegistry returns an empty Registry. The agent's role-specific
// bootstrap (steps 8 / 9) populates it via the Register* helpers below
// before the Poller starts.
func NewRegistry() *Registry {
	return &Registry{handlers: map[string]HandlerFunc{}}
}

// Register stores h under kind, overriding any previous handler. Used by
// tests and by callers that need to bind a kind not modeled by the typed
// helpers below (none today; here for forward compatibility).
func (r *Registry) Register(kind string, h HandlerFunc) {
	r.handlers[kind] = h
}

// RegisterProvider is the type-safe registration helper for the
// provider-role agent. It rejects an empty kind so a misuse fails at
// startup, not on the first instruction the broker emits.
func (r *Registry) RegisterProvider(kind autoscalingv1alpha1.ProviderInstructionKind, h HandlerFunc) {
	r.handlers[string(kind)] = h
}

// RegisterReservation is the type-safe registration helper for the
// consumer-role agent.
func (r *Registry) RegisterReservation(kind autoscalingv1alpha1.ReservationInstructionKind, h HandlerFunc) {
	r.handlers[string(kind)] = h
}

// Lookup returns the handler registered for kind, or (nil, false) if no
// handler is registered. The Poller treats unknown kinds as a Failed
// result rather than a silent drop so the broker (and operators) see
// the misconfiguration.
func (r *Registry) Lookup(kind string) (HandlerFunc, bool) {
	h, ok := r.handlers[kind]
	return h, ok
}

// Len returns the number of registered handlers; primarily used by tests
// and by the agent's start-up logging.
func (r *Registry) Len() int { return len(r.handlers) }
