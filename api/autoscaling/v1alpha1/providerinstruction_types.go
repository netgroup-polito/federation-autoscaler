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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProviderInstructionSpec is the desired state of a ProviderInstruction.
//
// ProviderInstructions live on the central (Broker) cluster and represent
// work the Broker wants a Provider Agent to perform. The Provider Agent
// discovers them by polling GET /api/v1/instructions every 5 s (or as part
// of the piggybacked response to POST /api/v1/advertisements); the agent
// never queries the CRD directly. See docs/design.md §5.4 and §7.3.6.
type ProviderInstructionSpec struct {
	// ReservationID is the Broker-side Reservation this instruction belongs
	// to. The Provider Agent uses it as the idempotency key together with
	// Kind (see docs/design.md §7.0).
	// +required
	// +kubebuilder:validation:MinLength=1
	ReservationID string `json:"reservationId"`

	// Kind is the operation the Provider Agent must perform. See
	// ProviderInstructionKind in common_types.go for the full list.
	// +required
	Kind ProviderInstructionKind `json:"kind"`

	// TargetClusterID is the Broker-facing identifier of the Provider cluster
	// that must execute this instruction. Used by the Broker's instruction
	// handler to filter the per-cluster response to GET /api/v1/instructions.
	// +required
	// +kubebuilder:validation:MinLength=1
	TargetClusterID string `json:"targetClusterId"`

	// ConsumerClusterID is the Broker-facing identifier of the consumer that
	// will peer against this provider. Required for GenerateKubeconfig; may
	// be empty for Reconcile.
	// +optional
	ConsumerClusterID string `json:"consumerClusterId,omitempty"`

	// ConsumerLiqoClusterID is the Liqo cluster identifier of the consumer.
	// Passed verbatim to `liqoctl generate peering-user --consumer-cluster-id`.
	// +optional
	ConsumerLiqoClusterID string `json:"consumerLiqoClusterId,omitempty"`

	// ChunkCount is the number of chunks this instruction applies to. Used
	// by the Broker for bookkeeping; the Provider Agent does not act on it
	// directly.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ChunkCount int32 `json:"chunkCount,omitempty"`

	// LastChunk is true when executing this instruction releases the last
	// remaining chunk of the reservation, allowing the Provider Agent to
	// tear down the peering-user kubeconfig.
	// +optional
	LastChunk bool `json:"lastChunk,omitempty"`

	// ExpiresAt is the deadline after which the Broker considers the
	// instruction unactionable. Matches the owning Reservation's ExpiresAt
	// unless the instruction was generated after Peered.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// ProviderInstructionStatus is the Broker's observed state for the instruction.
//
// The Broker updates this subresource exclusively on receipt of
// POST /api/v1/instructions/{id}/result (see docs/design.md §7.3.7).
type ProviderInstructionStatus struct {
	// ObservedGeneration is the metadata.generation the Broker most recently
	// reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Enforced is true once the Provider Agent has successfully reported the
	// outcome via POST /api/v1/instructions/{id}/result. Once Enforced is
	// true the instruction is no longer returned by GET /api/v1/instructions.
	Enforced bool `json:"enforced"`

	// IssuedAt is the timestamp at which the Broker first queued the
	// instruction for delivery.
	// +optional
	IssuedAt *metav1.Time `json:"issuedAt,omitempty"`

	// LastDeliveredAt is the timestamp of the most recent poll response in
	// which this instruction was included.
	// +optional
	LastDeliveredAt *metav1.Time `json:"lastDeliveredAt,omitempty"`

	// LastUpdateTime is the timestamp of the most recent status write
	// (typically the moment the Provider Agent's result was accepted).
	// +optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// Attempts counts how many times the Provider Agent has reported a
	// terminal result for this instruction (successful or failed).
	// +optional
	// +kubebuilder:validation:Minimum=0
	Attempts int32 `json:"attempts,omitempty"`

	// Message is a short, human-readable description of the current state.
	// Payload data (kubeconfig bytes, reconcile snapshots) are not stored
	// here; they flow only through the Broker's instruction-result handler.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=provinst
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.targetClusterId`
// +kubebuilder:printcolumn:name="Reservation",type=string,JSONPath=`.spec.reservationId`
// +kubebuilder:printcolumn:name="Enforced",type=boolean,JSONPath=`.status.enforced`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.spec.expiresAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ProviderInstruction is a Broker-owned work item targeted at a specific
// Provider Agent. See docs/design.md §5.4.
type ProviderInstruction struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the work item description.
	// +required
	Spec ProviderInstructionSpec `json:"spec"`

	// status is the Broker's observed state for this instruction.
	// +optional
	Status ProviderInstructionStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ProviderInstructionList contains a list of ProviderInstruction.
type ProviderInstructionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProviderInstruction `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ProviderInstruction{}, &ProviderInstructionList{})
}
