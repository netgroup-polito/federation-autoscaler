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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceRequestSpec is a user-declared request for federated capacity. It is
// the manual counterpart to a Cluster-Autoscaler-driven scale-up: instead of
// waiting for pending pods, an operator (or a batch job that knows its shape)
// reserves capacity up front by creating this object. The consumer-side
// controller sizes the request into chunks, reserves them on a broker-chosen
// provider, and drives the same Liqo peering the autoscaler path uses. Deleting
// the object releases the reservation (unpeer + free capacity).
type ResourceRequestSpec struct {
	// Resources is the total capacity to reserve (cpu / memory / nvidia.com/gpu).
	// The controller rounds each resource UP to whole node-sized chunks, because
	// capacity is only ever borrowed a node at a time. At least one resource must
	// be set.
	// +required
	Resources corev1.ResourceList `json:"resources"`

	// Duration is an optional lifetime hint as a Go duration string (e.g. "2h").
	// v1 records it but does not yet enforce a per-request TTL — the broker's
	// global --reservation-timeout applies; delete the ResourceRequest to release
	// early. A future revision will translate this into a per-reservation expiry.
	// +optional
	Duration string `json:"duration,omitempty"`

	// Priority is an optional preference (higher = preferred) reserved for the
	// standard composite placement policy. v1 records it but does not yet act on
	// it.
	// +optional
	Priority int32 `json:"priority,omitempty"`
}

// ResourceRequestPhase is the coarse lifecycle state of a manual request.
type ResourceRequestPhase string

const (
	// ResourceRequestPending means the request is waiting for a provider with
	// enough free capacity; the controller keeps retrying.
	ResourceRequestPending ResourceRequestPhase = "Pending"

	// ResourceRequestReserved means the reservation was created at the broker and
	// the normal peering flow is under way (mirrors the autoscaler path).
	ResourceRequestReserved ResourceRequestPhase = "Reserved"

	// ResourceRequestActive means the borrowed virtual node(s) exist and have
	// been marked so the Cluster Autoscaler will not scale them down — the manual
	// reservation is held until the ResourceRequest is deleted.
	ResourceRequestActive ResourceRequestPhase = "Active"

	// ResourceRequestMigrating means periodic re-evaluation found a better provider
	// and the reservation is being moved there (break-before-make): the old
	// reservation has been released and a new one (a fresh id) is being created on
	// the better provider. Only manual reservations under a stable-metric policy
	// (Price/Eco/Latency) ever enter this phase.
	ResourceRequestMigrating ResourceRequestPhase = "Migrating"

	// ResourceRequestFailed means the reservation could not be created; Message
	// carries the reason.
	ResourceRequestFailed ResourceRequestPhase = "Failed"
)

// ResourceRequestStatus is the controller's observed state for one request.
type ResourceRequestStatus struct {
	// Phase is the coarse lifecycle state.
	// +optional
	Phase ResourceRequestPhase `json:"phase,omitempty"`

	// ReservationID is the broker reservation this request currently holds. The
	// first reservation is "mr-<uid>" (deterministic); each migration to a better
	// provider (feature 7) uses a FRESH id "mr-<uid>-m<MigrationCount>" so the new
	// provider's reservation + consumer artifacts don't collide with the old one.
	// Used to release the current reservation on delete.
	// +optional
	ReservationID string `json:"reservationId,omitempty"`

	// ProviderClusterID is the provider the broker chose for this request.
	// +optional
	ProviderClusterID string `json:"providerClusterId,omitempty"`

	// ChunkCount is the number of node-sized chunks reserved.
	// +optional
	ChunkCount int32 `json:"chunkCount,omitempty"`

	// MigrationCount is how many times this reservation has been migrated to a
	// better provider (feature 7). 0 for a reservation that never moved. It seeds
	// the fresh reservation id on each migration ("mr-<uid>-m<MigrationCount>").
	// +optional
	MigrationCount int32 `json:"migrationCount,omitempty"`

	// Message is a human-readable status detail (e.g. why it is still Pending).
	// +optional
	Message string `json:"message,omitempty"`

	// LastTransitionTime is when Phase last changed.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=rr
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.status.providerClusterId`
// +kubebuilder:printcolumn:name="Chunks",type=integer,JSONPath=`.status.chunkCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ResourceRequest is a consumer-cluster-local, user-created request for
// federated capacity — a manual reservation. It reuses the exact reservation +
// Liqo-peering path the Cluster Autoscaler drives, so applying it peers with a
// broker-chosen provider and deleting it releases the capacity. It lives only on
// the consumer cluster; the grpc-server controller drives it via the co-located
// consumer agent's loopback API (the agent-initiated, no-broker-dial-in model).
type ResourceRequest struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the desired capacity to reserve.
	// +required
	Spec ResourceRequestSpec `json:"spec"`

	// status is the controller's observed state.
	// +optional
	Status ResourceRequestStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ResourceRequestList contains a list of ResourceRequest.
type ResourceRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourceRequest{}, &ResourceRequestList{})
}
