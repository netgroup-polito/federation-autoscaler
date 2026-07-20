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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// VirtualNodeStateReservationLabel is the label key stamped on every
// VirtualNodeState the consumer agent creates. It mirrors the label
// the agent already puts on the ResourceSlice (see liqo.go) so the
// reconciler's Liqo VirtualNode watch can correlate either side.
const VirtualNodeStateReservationLabel = "federation-autoscaler.io/reservation"

// virtualNodeStateName returns the deterministic VirtualNodeState name
// for a reservation. Matches the rs-/no-/kubeconfig- naming family so
// every consumer-side artefact for a reservation shares the same
// suffix and is therefore easy to grep for.
func virtualNodeStateName(reservationID string) string {
	return "vns-" + reservationID
}

// ensureVirtualNodeState creates the VirtualNodeState CR that pairs
// with this reservation's ResourceSlice. Idempotent on AlreadyExists —
// re-issuing the same Peer instruction (which is how the broker handles
// retries) must not flap the CR.
//
// A Reservation maps to exactly one ResourceSlice → one Liqo VirtualNode →
// one v1.Node → one VirtualNodeState. ChunkIndex is therefore always zero;
// N chunks are N Reservations, which is what lets one provider donate more
// than one node. See docs/design.md §5.1.
//
// sliceName is the ResourceSlice ensureResourceSlice just created. It is also
// the name Liqo gives the resulting node, so recording it here is what lets
// the VirtualNodeState controller find that node.
func ensureVirtualNodeState(
	ctx context.Context,
	c ctrlclient.Client,
	namespace, sliceName string,
	in *brokerapi.InstructionView,
) error {
	if in == nil {
		return fmt.Errorf("nil instruction")
	}
	chunkType := deriveChunkType(in.ResourceSliceResources)
	vns := &autoscalingv1alpha1.VirtualNodeState{
		ObjectMeta: metav1.ObjectMeta{
			Name:      virtualNodeStateName(in.ReservationID),
			Namespace: namespace,
			Labels: map[string]string{
				VirtualNodeStateReservationLabel: in.ReservationID,
			},
		},
		Spec: autoscalingv1alpha1.VirtualNodeStateSpec{
			ProviderClusterID:     in.ProviderClusterID,
			ProviderLiqoClusterID: in.ProviderLiqoClusterID,
			ResourceSliceName:     sliceName,
			NodeGroupID:           nodeGroupIDFor(in.ProviderClusterID, chunkType),
			ChunkIndex:            0,
			ReservationID:         in.ReservationID,
			Resources:             in.ResourceSliceResources,
		},
	}
	if err := c.Create(ctx, vns); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create VirtualNodeState %q: %w", vns.Name, err)
	}
	return nil
}

// deleteVirtualNodeState removes the VirtualNodeState for a
// reservation. Idempotent on missing — required because Cleanup may
// run after Unpeer has already removed the CR.
func deleteVirtualNodeState(
	ctx context.Context,
	c ctrlclient.Client,
	namespace, reservationID string,
) error {
	vns := &autoscalingv1alpha1.VirtualNodeState{
		ObjectMeta: metav1.ObjectMeta{
			Name:      virtualNodeStateName(reservationID),
			Namespace: namespace,
		},
	}
	if err := c.Delete(ctx, vns); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete VirtualNodeState %q: %w", vns.Name, err)
	}
	return nil
}

// getVirtualNodeState fetches a VirtualNodeState by reservation id. A
// helper kept here (rather than inlined) so unit tests can reach for
// the canonical lookup without re-deriving the name.
func getVirtualNodeState(
	ctx context.Context,
	c ctrlclient.Client,
	namespace, reservationID string,
) (*autoscalingv1alpha1.VirtualNodeState, error) {
	vns := &autoscalingv1alpha1.VirtualNodeState{}
	err := c.Get(ctx, types.NamespacedName{
		Name:      virtualNodeStateName(reservationID),
		Namespace: namespace,
	}, vns)
	return vns, err
}

// deriveChunkType classifies a Reservation's per-chunk resources into
// the broker-side ChunkType enum without needing to thread ChunkType
// through the instruction wire format. Presence of nvidia.com/gpu is
// the canonical GPU marker in v1; everything else falls under standard.
// If v2 ever introduces more chunk types, prefer adding ChunkType to
// InstructionView over extending this heuristic.
func deriveChunkType(resources corev1.ResourceList) brokerv1alpha1.ChunkType {
	if _, ok := resources["nvidia.com/gpu"]; ok {
		return brokerv1alpha1.ChunkTypeGPU
	}
	return brokerv1alpha1.ChunkTypeStandard
}

// nodeGroupIDFor mirrors the broker's nodeGroupID formula (see
// internal/broker/api/reservation.go) so the consumer agent and the
// broker stay in lock-step on group identifiers. Duplicated here to
// keep the agent free of any internal/broker/api dependency — the wire
// format is the only allowed coupling between the two packages.
func nodeGroupIDFor(providerClusterID string, chunkType brokerv1alpha1.ChunkType) string {
	return fmt.Sprintf("ng-%s-%s", providerClusterID, string(chunkType))
}
