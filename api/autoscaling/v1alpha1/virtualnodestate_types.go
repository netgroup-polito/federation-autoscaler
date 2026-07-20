/*
Copyright 2026.

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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Standard Kubernetes node condition types used inside
// VirtualNodeStateStatus.Conditions. We re-declare the names here as
// constants so callers (the reconciler and the gRPC server) don't
// have to keep raw string literals in sync.
const (
	// VirtualNodeStateConditionReady is set to True once the underlying
	// Liqo VirtualNode reports NodeReady=True. The localapi exposes
	// the chunk in /local/virtual-nodes once this condition is True.
	VirtualNodeStateConditionReady = "Ready"

	// VirtualNodeStateConditionFailed marks a terminal failure of
	// peering, ResourceSlice creation, or VirtualNode materialisation.
	// Message carries the human-readable explanation.
	VirtualNodeStateConditionFailed = "Failed"
)

// VirtualNodeStateSpec describes a single virtual-node chunk on the consumer
// cluster.
//
// VirtualNodeState lives only on the consumer cluster; the gRPC server is the
// sole controller. The spec is written once (on NodeGroupIncreaseSize) and
// never edited afterwards — chunk release is driven through status.phase.
type VirtualNodeStateSpec struct {
	// ProviderClusterID is the Broker-facing identifier of the provider that
	// donated this chunk.
	// +required
	// +kubebuilder:validation:MinLength=1
	ProviderClusterID string `json:"providerClusterId"`

	// ProviderLiqoClusterID is the Liqo cluster identifier of the provider.
	// +required
	// +kubebuilder:validation:MinLength=1
	ProviderLiqoClusterID string `json:"providerLiqoClusterId"`

	// ResourceSliceName is the name of the Liqo ResourceSlice this chunk was
	// claimed with, and therefore the name of the v1.Node Liqo materialises for
	// it: Liqo propagates ResourceSlice.Name -> VirtualNode.Name -> Node.Name
	// unchanged. It is how the VirtualNodeState controller finds its node.
	//
	// This MUST NOT be inferred from ProviderLiqoClusterID. That used to work
	// only because `liqoctl peer` names the slice it creates after the provider
	// cluster, which caps a provider at one borrowed node and makes two chunks
	// from one provider collide on a single node. The consumer agent now owns
	// the slice and names it per reservation.
	// +optional
	ResourceSliceName string `json:"resourceSliceName,omitempty"`

	// NodeGroupID is the gRPC-server-generated node-group identifier this
	// chunk belongs to (one node group per provider × chunk type).
	// +required
	// +kubebuilder:validation:MinLength=1
	NodeGroupID string `json:"nodeGroupId"`

	// ChunkIndex is the position of this chunk inside its reservation
	// (0-based). Together with ReservationID it uniquely identifies a chunk.
	// A Reservation now carries exactly one chunk (N chunks = N Reservations,
	// so each maps 1:1 to one ResourceSlice and one node), which makes this
	// always 0; the field stays for wire/CRD compatibility.
	// +required
	// +kubebuilder:validation:Minimum=0
	ChunkIndex int32 `json:"chunkIndex"`

	// ReservationID is the identifier of the Broker-side Reservation that
	// owns this chunk.
	// +required
	// +kubebuilder:validation:MinLength=1
	ReservationID string `json:"reservationId"`

	// Resources is the per-chunk capacity (cpu, memory, nvidia.com/gpu, …)
	// at the moment the chunk was created. Used by the gRPC server when
	// building CA node templates for NodeGroupTemplateNodeInfo.
	// +required
	Resources corev1.ResourceList `json:"resources"`
}

// VirtualNodeStateStatus is the gRPC-server's observed state for a chunk.
type VirtualNodeStateStatus struct {
	// ObservedGeneration is the metadata.generation the controller most
	// recently reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is the lifecycle phase of the chunk; see VirtualNodeStatePhase
	// in common_types.go for the full state machine.
	// +optional
	Phase VirtualNodeStatePhase `json:"phase,omitempty"`

	// VirtualNodeName is the name of the Liqo-created v1.Node on the consumer
	// cluster. Empty while Phase is Creating; populated once Liqo materializes
	// the virtual node.
	// +optional
	VirtualNodeName string `json:"virtualNodeName,omitempty"`

	// ProviderID mirrors the materialised v1.Node's `.spec.providerID`
	// (Liqo sets this to a `liqo://…` value). Cluster Autoscaler matches
	// the cloud instances the gRPC server reports in NodeGroupNodes
	// against the registered nodes' providerIDs — NOT their names — so the
	// gRPC server must return this verbatim as the Instance Id, otherwise
	// CA never matches the node, reports it as "unregistered", and churns
	// scale-up/scale-down reservations forever. Empty until the node is
	// Ready (and providerID has been published).
	// +optional
	ProviderID string `json:"providerID,omitempty"`

	// ResourceSliceName mirrors Spec.ResourceSliceName once the controller has
	// observed the chunk, so the printcolumn and the Reconcile report can read
	// it off status like every other observed field. Empty until the Peer
	// instruction has succeeded and written the spec.
	// +optional
	ResourceSliceName string `json:"resourceSliceName,omitempty"`

	// Allocatable mirrors the Liqo VirtualNode's `.status.allocatable`
	// once the node has been materialised. Surfaced by
	// VirtualNodeStateReconciler so the gRPC server's NodeGroupNodes /
	// PricingNodePrice replies can describe the chunk to Cluster
	// Autoscaler using the resource shape Liqo actually advertised. Empty
	// while Phase is Creating or whenever the upstream VirtualNode has
	// not yet published its status.
	// +optional
	Allocatable corev1.ResourceList `json:"allocatable,omitempty"`

	// LastTransitionTime is the timestamp of the most recent phase change.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// Message is a human-readable description of the current state or of
	// the most recent failure.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions reports standard state markers:
	//   - "Ready"  — Liqo virtual node exists and is schedulable.
	//   - "Failed" — creation or deletion failed irrecoverably (see Message).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=vns
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.providerClusterId`
// +kubebuilder:printcolumn:name="NodeGroup",type=string,JSONPath=`.spec.nodeGroupId`
// +kubebuilder:printcolumn:name="Chunk",type=integer,JSONPath=`.spec.chunkIndex`
// +kubebuilder:printcolumn:name="VirtualNode",type=string,JSONPath=`.status.virtualNodeName`
// +kubebuilder:printcolumn:name="ResourceSlice",type=string,JSONPath=`.status.resourceSliceName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VirtualNodeState tracks the lifecycle of a single virtual-node chunk on the
// consumer cluster. Created by the gRPC server on NodeGroupIncreaseSize and
// removed after the chunk has been released. See docs/design.md §5.1.
type VirtualNodeState struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the immutable description of the chunk.
	// +required
	Spec VirtualNodeStateSpec `json:"spec"`

	// status is the gRPC-server's observed state for this chunk.
	// +optional
	Status VirtualNodeStateStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// VirtualNodeStateList contains a list of VirtualNodeState.
type VirtualNodeStateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualNodeState `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VirtualNodeState{}, &VirtualNodeStateList{})
}
