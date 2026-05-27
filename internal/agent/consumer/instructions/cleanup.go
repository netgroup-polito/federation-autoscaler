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

package instructions

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/poller"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// CleanupConfig configures the Cleanup handler.
type CleanupConfig struct {
	// LocalClient is the consumer-cluster k8s client. Required.
	LocalClient ctrlclient.Client

	// Namespace where the Liqo CRs and kubeconfig Secret live. Required.
	Namespace string

	// Logger is the structured logger the handler logs through.
	Logger logr.Logger
}

// NewCleanupHandler returns a poller.HandlerFunc that force-removes
// the consumer-side state for a reservation. Unlike Unpeer, Cleanup
// does NOT shell out to liqoctl: the broker emits this instruction
// when the reservation has already reached a terminal phase (Released
// / Failed / Expired), so the provider may be unreachable. The
// handler does:
//
//  1. Delete the VirtualNodeState CR (idempotent on missing).
//  2. Delete the Liqo ResourceSlice (idempotent on missing).
//  3. Delete the kubeconfig Secret (idempotent on missing).
//
// NamespaceOffloading is intentionally NOT touched — it is the
// per-K8s-namespace singleton owned by the operator (see peer.go).
//
// Returns Succeeded with no payload.
func NewCleanupHandler(cfg CleanupConfig) poller.HandlerFunc {
	if cfg.Logger.GetSink() == nil {
		cfg.Logger = log.Log.WithName("consumer-handler-cleanup")
	}

	return func(ctx context.Context, in *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		if in == nil {
			return nil, errors.New("nil instruction")
		}
		if in.Kind != string(autoscalingv1alpha1.ReservationInstructionCleanup) {
			return nil, fmt.Errorf("unexpected kind %q (want %s)",
				in.Kind, autoscalingv1alpha1.ReservationInstructionCleanup)
		}
		if cfg.LocalClient == nil {
			return nil, errors.New("cleanup handler: LocalClient is nil")
		}
		if cfg.Namespace == "" {
			return nil, errors.New("cleanup handler: Namespace is required")
		}

		logger := cfg.Logger.WithValues("reservationId", in.ReservationID)

		// NamespaceOffloading is intentionally NOT deleted here — it's a
		// per-K8s-namespace singleton owned by the operator, not the
		// agent. See peer.go for the matching ensure-side rationale.
		if err := deleteVirtualNodeState(ctx, cfg.LocalClient, cfg.Namespace, in.ReservationID); err != nil {
			return nil, err
		}
		if err := deleteResourceSlice(ctx, cfg.LocalClient, cfg.Namespace, in.ReservationID); err != nil {
			return nil, err
		}
		if err := deleteKubeconfigSecret(ctx, cfg.LocalClient, cfg.Namespace, in.ReservationID); err != nil {
			return nil, err
		}

		logger.V(1).Info("consumer-side state cleared")
		return &brokerapi.InstructionResultRequest{Status: brokerapi.ResultStatusSucceeded}, nil
	}
}
