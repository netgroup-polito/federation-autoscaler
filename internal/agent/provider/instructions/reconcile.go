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
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/provider/snapshot"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// ReconcileConfig configures the Reconcile handler. The broker may
// queue this instruction at any time to ask the provider to re-publish
// its current snapshot — typically right after a broker restart so the
// freshly-loaded ConsumerRegistry can converge on the truth without
// waiting for the next 30 s advertisement tick.
type ReconcileConfig struct {
	// LocalClient reads local Nodes for the snapshot. Required.
	LocalClient ctrlclient.Client

	// ClusterID and LiqoClusterID are stamped on the
	// AdvertisementRequest payload. Both required.
	ClusterID     string
	LiqoClusterID string

	// Logger is the structured logger the handler logs through.
	Logger logr.Logger
}

// NewReconcileHandler returns a poller.HandlerFunc that walks the
// local cluster's Nodes (via snapshot.Take), packages the result as an
// AdvertisementRequest, and returns it inside a ReconcilePayload. The
// broker treats the payload identically to a regular advertisement —
// it stamps it onto the ClusterAdvertisement CR and recomputes chunks.
func NewReconcileHandler(cfg ReconcileConfig) poller.HandlerFunc {
	// LocalClient / ClusterID / LiqoClusterID are validated at dispatch
	// time so the wiring in provider.Run stays symmetric with the
	// shell-out handlers that fail-late on missing inputs.
	if cfg.Logger.GetSink() == nil {
		cfg.Logger = log.Log.WithName("provider-handler-reconcile")
	}

	return func(ctx context.Context, in *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		if in == nil {
			return nil, errors.New("nil instruction")
		}
		if in.Kind != string(autoscalingv1alpha1.ProviderInstructionReconcile) {
			return nil, fmt.Errorf("unexpected kind %q (want %s)",
				in.Kind, autoscalingv1alpha1.ProviderInstructionReconcile)
		}
		if cfg.LocalClient == nil {
			return nil, errors.New("reconcile handler: LocalClient is nil")
		}
		if cfg.ClusterID == "" || cfg.LiqoClusterID == "" {
			return nil, errors.New("reconcile handler: ClusterID and LiqoClusterID are required")
		}

		snap, err := snapshot.Take(ctx, cfg.LocalClient)
		if err != nil {
			return nil, fmt.Errorf("snapshot: %w", err)
		}

		cfg.Logger.V(1).Info("reconciling",
			"reservationId", in.ReservationID,
			"countedNodes", snap.CountedNodes)

		return &brokerapi.InstructionResultRequest{
			Status: brokerapi.ResultStatusSucceeded,
			Payload: &brokerapi.ResultPayload{
				Kind: brokerapi.PayloadKindReconcile,
				Advertisement: &brokerapi.AdvertisementRequest{
					ClusterID:     cfg.ClusterID,
					LiqoClusterID: cfg.LiqoClusterID,
					Resources:     snap.Allocatable,
				},
			},
		}, nil
	}
}
