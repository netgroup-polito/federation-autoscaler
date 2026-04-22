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

// ReservationInstructionSpec is the desired state of a ReservationInstruction.
//
// ReservationInstructions live on the central (Broker) cluster and represent
// work the Broker wants a Consumer Agent to perform. The Consumer Agent
// discovers them by polling GET /api/v1/instructions every 5 s; the agent
// never queries the CRD directly. For Peer instructions the Broker inlines
// the peering-user kubeconfig **only in the polling response** — it is never
// stored in this CRD's status (see docs/design.md §10.4). The spec carries
// only a reference (KubeconfigRef) to the internal Secret on the Broker
// cluster where the kubeconfig is parked until the next poll cycle.
type ReservationInstructionSpec struct {
	// ReservationID is the Broker-side Reservation this instruction belongs
	// to. Together with Kind it serves as the idempotency key on the
	// Consumer Agent side (see docs/design.md §7.0).
	// +required
	// +kubebuilder:validation:MinLength=1
	ReservationID string `json:"reservationId"`

	// Kind is the operation the Consumer Agent must perform. See
	// ReservationInstructionKind in common_types.go for the full list.
	// +required
	Kind ReservationInstructionKind `json:"kind"`

	// TargetClusterID is the Broker-facing identifier of the Consumer
	// cluster that must execute this instruction. Used by the Broker's
	// instruction handler to filter the per-cluster response to
	// GET /api/v1/instructions.
	// +required
	// +kubebuilder:validation:MinLength=1
	TargetClusterID string `json:"targetClusterId"`

	// ProviderClusterID is the Broker-facing identifier of the provider
	// cluster the consumer must peer with (or unpeer from). Required for
	// Peer and Unpeer; optional for Cleanup / Reconcile.
	// +optional
	ProviderClusterID string `json:"providerClusterId,omitempty"`

	// ProviderLiqoClusterID is the Liqo cluster identifier of the provider.
	// Passed to `liqoctl peer` or used to identify the remote side of an
	// existing peering for Unpeer / Cleanup.
	// +optional
	ProviderLiqoClusterID string `json:"providerLiqoClusterId,omitempty"`

	// ResourceSliceNames lists the ResourceSlices the Consumer Agent must
	// create (Peer) or delete (Unpeer). Empty for Peer if names are chosen
	// by the agent and reported back via the instruction-result payload.
	// +optional
	ResourceSliceNames []string `json:"resourceSliceNames,omitempty"`

	// ChunkCount is the number of chunks this instruction applies to.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ChunkCount int32 `json:"chunkCount,omitempty"`

	// LastChunk is true when the instruction releases (or represents) the
	// last remaining chunk of the reservation. On Unpeer, this tells the
	// Consumer Agent it must also run `liqoctl unpeer`.
	// +optional
	LastChunk bool `json:"lastChunk,omitempty"`

	// KubeconfigRef is an internal reference to the Kubernetes Secret on
	// the Broker cluster that holds the peering-user kubeconfig for Peer
	// instructions. The bytes themselves are inlined in the polling
	// response — never in this spec, status, or etcd watch streams.
	// +optional
	KubeconfigRef string `json:"kubeconfigRef,omitempty"`

	// ExpiresAt is the deadline after which the Broker considers the
	// instruction unactionable. Matches the owning Reservation's ExpiresAt
	// unless the instruction was generated after Peered.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// ReservationInstructionStatus is the Broker's observed state for the
// instruction. Updated exclusively on receipt of
// POST /api/v1/instructions/{id}/result (see docs/design.md §7.3.7).
type ReservationInstructionStatus struct {
	// ObservedGeneration is the metadata.generation the Broker most recently
	// reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Enforced is true once the Consumer Agent has successfully reported the
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
	// (typically the moment the Consumer Agent's result was accepted).
	// +optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// Attempts counts how many times the Consumer Agent has reported a
	// terminal result for this instruction (successful or failed).
	// +optional
	// +kubebuilder:validation:Minimum=0
	Attempts int32 `json:"attempts,omitempty"`

	// Message is a short, human-readable description of the current state.
	// Payload data (virtual node names, ResourceSlice names, reconcile
	// snapshots) are not stored here; they flow only through the
	// instruction-result handler and are reflected on the Reservation.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=resvinst
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Consumer",type=string,JSONPath=`.spec.targetClusterId`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.providerClusterId`
// +kubebuilder:printcolumn:name="Reservation",type=string,JSONPath=`.spec.reservationId`
// +kubebuilder:printcolumn:name="Enforced",type=boolean,JSONPath=`.status.enforced`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.spec.expiresAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ReservationInstruction is a Broker-owned work item targeted at a specific
// Consumer Agent. See docs/design.md §5.5.
type ReservationInstruction struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the work item description.
	// +required
	Spec ReservationInstructionSpec `json:"spec"`

	// status is the Broker's observed state for this instruction.
	// +optional
	Status ReservationInstructionStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ReservationInstructionList contains a list of ReservationInstruction.
type ReservationInstructionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReservationInstruction `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReservationInstruction{}, &ReservationInstructionList{})
}
