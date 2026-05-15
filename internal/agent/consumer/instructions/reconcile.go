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

// ReconcileConfig configures the Reconcile handler.
type ReconcileConfig struct {
	// LocalClient lists the consumer-cluster's VirtualNodeState CRs.
	// Required.
	LocalClient ctrlclient.Client

	// Namespace constrains the List; empty means "all namespaces".
	Namespace string

	// Logger is the structured logger the handler logs through.
	Logger logr.Logger
}

// NewReconcileHandler returns a poller.HandlerFunc that lists every
// VirtualNodeState CR on the consumer cluster and reports them inline
// in the result payload (docs/design.md §7.3.7). The broker may queue
// this instruction at any time to catch up after its own restart, so
// the handler must be cheap and idempotent — a single List call plus
// projection.
//
// VirtualNodeState CRs are created by the consumer agent's Peer
// handler and kept in sync by the VirtualNodeStateReconciler; an empty
// slice here simply means no chunks have been peered yet on this
// consumer, which is valid state, not an error.
func NewReconcileHandler(cfg ReconcileConfig) poller.HandlerFunc {
	if cfg.Logger.GetSink() == nil {
		cfg.Logger = log.Log.WithName("consumer-handler-reconcile")
	}

	return func(ctx context.Context, in *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		if in == nil {
			return nil, errors.New("nil instruction")
		}
		if in.Kind != string(autoscalingv1alpha1.ReservationInstructionReconcile) {
			return nil, fmt.Errorf("unexpected kind %q (want %s)",
				in.Kind, autoscalingv1alpha1.ReservationInstructionReconcile)
		}
		if cfg.LocalClient == nil {
			return nil, errors.New("reconcile handler: LocalClient is nil")
		}

		var list autoscalingv1alpha1.VirtualNodeStateList
		listOpts := []ctrlclient.ListOption{}
		if cfg.Namespace != "" {
			listOpts = append(listOpts, ctrlclient.InNamespace(cfg.Namespace))
		}
		if err := cfg.LocalClient.List(ctx, &list, listOpts...); err != nil {
			return nil, fmt.Errorf("list VirtualNodeState: %w", err)
		}

		views := make([]brokerapi.ReconcileVirtualNodeStateView, 0, len(list.Items))
		for i := range list.Items {
			views = append(views, viewFromVNS(&list.Items[i]))
		}

		cfg.Logger.V(1).Info("reconcile snapshot",
			"reservationId", in.ReservationID, "virtualNodeStates", len(views))

		return &brokerapi.InstructionResultRequest{
			Status: brokerapi.ResultStatusSucceeded,
			Payload: &brokerapi.ResultPayload{
				Kind:              brokerapi.PayloadKindReconcile,
				VirtualNodeStates: views,
			},
		}, nil
	}
}

// viewFromVNS projects a VirtualNodeState CR onto the wire shape the
// broker expects.
func viewFromVNS(v *autoscalingv1alpha1.VirtualNodeState) brokerapi.ReconcileVirtualNodeStateView {
	return brokerapi.ReconcileVirtualNodeStateView{
		ReservationID:   v.Spec.ReservationID,
		ChunkIndex:      v.Spec.ChunkIndex,
		NodeGroupID:     v.Spec.NodeGroupID,
		VirtualNodeName: v.Status.VirtualNodeName,
		ResourceSlice:   v.Status.ResourceSliceName,
		Phase:           v.Status.Phase,
		LastTransition:  v.Status.LastTransitionTime,
	}
}
