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

package localapi

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
)

// VirtualNodeView is the local-API representation of one consumer-side
// VirtualNodeState — one entry per Liqo virtual node materialised on
// the consumer cluster. The gRPC server consumes this view when
// answering Cluster Autoscaler's NodeGroupTargetSize / NodeGroupNodes /
// NodeGroupTemplateNodeInfo RPCs. The VirtualNodeStateReconciler keeps
// the underlying CRs in sync with the corresponding Liqo VirtualNode,
// and the consumer agent's Peer / Unpeer / Cleanup handlers create and
// delete them as reservations move through the broker phase machine.
type VirtualNodeView struct {
	// Name is the Liqo VirtualNode object name (== virtual-node name
	// surfaced to the local cluster's scheduler).
	Name string `json:"name"`

	// ReservationID is the broker-side reservation this virtual node
	// belongs to. Multiple VirtualNodeViews may share a reservation
	// when a Reservation has ChunkCount > 1.
	ReservationID string `json:"reservationId"`

	// NodeGroupID matches the gRPC server's NodeGroup.id (the wire
	// identifier CA uses when scaling up).
	NodeGroupID string `json:"nodeGroupId,omitempty"`

	// ProviderClusterID and ProviderLiqoClusterID identify the upstream
	// provider for log/diagnostic purposes.
	ProviderClusterID     string `json:"providerClusterId,omitempty"`
	ProviderLiqoClusterID string `json:"providerLiqoClusterId,omitempty"`

	// Phase mirrors VirtualNodeState.status.phase verbatim.
	Phase autoscalingv1alpha1.VirtualNodeStatePhase `json:"phase,omitempty"`

	// Allocatable mirrors the corresponding Liqo VirtualNode's
	// allocatable resources, surfaced by the reconciler. Empty until
	// the node is Ready.
	Allocatable corev1.ResourceList `json:"allocatable,omitempty"`

	// LastTransitionTime is the timestamp of the most recent phase
	// change. Useful when the gRPC server reports node-group status to
	// Cluster Autoscaler.
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// VirtualNodeListResponse is the body of GET /local/virtual-nodes.
type VirtualNodeListResponse struct {
	VirtualNodes []VirtualNodeView `json:"virtualNodes"`
}
