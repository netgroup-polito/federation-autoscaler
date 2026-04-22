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

// ReservationSpec is the desired state of a Reservation.
//
// A Reservation is created by the Broker in response to a Consumer Agent's
// POST /api/v1/reservations (see docs/design.md §7.3.5). It binds a specific
// number of chunks of a single provider to a single consumer. The spec is
// immutable once the Broker has chosen a provider; chunk release is driven
// through DELETE /api/v1/reservations/{id} and reflected in status.phase.
type ReservationSpec struct {
	// ConsumerClusterID is the Broker-facing identifier of the consumer cluster
	// that requested this reservation. Matches the CN of the consumer agent's
	// mTLS client certificate.
	// +required
	// +kubebuilder:validation:MinLength=1
	ConsumerClusterID string `json:"consumerClusterId"`

	// ConsumerLiqoClusterID is the Liqo cluster identifier of the consumer,
	// used when generating the peering-user kubeconfig on the provider side.
	// +required
	// +kubebuilder:validation:MinLength=1
	ConsumerLiqoClusterID string `json:"consumerLiqoClusterId"`

	// ProviderClusterID is the Broker-facing identifier of the provider cluster
	// chosen by the decision engine.
	// +required
	// +kubebuilder:validation:MinLength=1
	ProviderClusterID string `json:"providerClusterId"`

	// ProviderLiqoClusterID is the Liqo cluster identifier of the provider,
	// used when the consumer runs liqoctl peer.
	// +required
	// +kubebuilder:validation:MinLength=1
	ProviderLiqoClusterID string `json:"providerLiqoClusterId"`

	// ChunkCount is the number of chunks reserved. A Reservation may span
	// multiple chunks but is always a single provider × single chunk type.
	// +required
	// +kubebuilder:validation:Minimum=1
	ChunkCount int32 `json:"chunkCount"`

	// ChunkType classifies the reserved chunks (standard or gpu); the per-chunk
	// resource amounts are derived from the chunk-config ConfigMap.
	// +required
	ChunkType ChunkType `json:"chunkType"`

	// Resources is the per-chunk capacity at the moment the reservation was
	// made. Surfaced to Cluster Autoscaler so it can build correct node
	// templates. Each value is one chunk's worth (not ChunkCount × chunk).
	// +required
	Resources corev1.ResourceList `json:"resources"`

	// Namespaces is the set of namespaces the consumer wants to offload on the
	// reserved virtual nodes. Used by the Consumer Agent to create or update
	// the Liqo NamespaceOffloading CR during the Peer instruction.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`
}

// ReservationStatus is the Broker's observed state for a Reservation.
type ReservationStatus struct {
	// ObservedGeneration is the metadata.generation the Broker most recently
	// reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is the lifecycle phase of the Reservation. See ReservationPhase in
	// common_types.go for the full state machine.
	// +optional
	Phase ReservationPhase `json:"phase,omitempty"`

	// CreatedAt is when the Broker accepted the reservation. Equal to
	// metadata.creationTimestamp except for older reservations migrated from
	// an earlier version.
	// +optional
	CreatedAt *metav1.Time `json:"createdAt,omitempty"`

	// ExpiresAt is the deadline by which the reservation must reach Peered.
	// Computed as CreatedAt + reservation-timeout (default 5m, from the
	// chunk-config ConfigMap). After ExpiresAt the Broker marks the
	// reservation Expired and releases its chunks.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// VirtualNodeNames is the list of Liqo-created virtual node names on the
	// consumer cluster, reported by the Consumer Agent in its PeerPayload.
	// One entry per reserved chunk.
	// +optional
	VirtualNodeNames []string `json:"virtualNodeNames,omitempty"`

	// Message is a human-readable description of the most recent transition or
	// of the current failure, if any.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions reports standard state markers:
	//   - "Ready"     — all chunks peered; virtual nodes visible to CA.
	//   - "Failed"    — an irrecoverable error occurred (see Message).
	//   - "Expired"   — reservation-timeout elapsed before Peered.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=resv
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Consumer",type=string,JSONPath=`.spec.consumerClusterId`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.providerClusterId`
// +kubebuilder:printcolumn:name="Chunks",type=integer,JSONPath=`.spec.chunkCount`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.chunkType`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.expiresAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Reservation is a Broker-owned binding of a set of chunks from a single
// provider cluster to a single consumer cluster. Its lifecycle is described
// in docs/design.md §5.3 and §8.
type Reservation struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the requested reservation (written once by the Broker).
	// +required
	Spec ReservationSpec `json:"spec"`

	// status is the Broker's observed state for this reservation.
	// +optional
	Status ReservationStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ReservationList contains a list of Reservation.
type ReservationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Reservation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Reservation{}, &ReservationList{})
}
