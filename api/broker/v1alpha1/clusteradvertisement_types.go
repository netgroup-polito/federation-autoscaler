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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AdvertisedResources is the snapshot of capacity a provider advertises to the
// Broker. "Allocatable" follows the usual Kubernetes meaning: resources that
// the provider is willing to donate, already net of system overhead and of any
// usage the provider chooses to keep to itself.
type AdvertisedResources struct {
	// Allocatable is the set of donatable resources. The standard keys are cpu,
	// memory, and nvidia.com/gpu, but any Kubernetes resource name is accepted
	// (the Broker routes sizing decisions through the chunk-config ConfigMap).
	// +required
	Allocatable corev1.ResourceList `json:"allocatable"`
}

// ClusterAdvertisementSpec is the desired state of a ClusterAdvertisement.
//
// The Provider Agent on each provider cluster upserts this object every 30 s
// via POST /api/v1/advertisements (see docs/design.md §7.3.1). The Broker is
// the controller; consumers never write to this object.
type ClusterAdvertisementSpec struct {
	// ClusterID is the unique identifier of the provider cluster. It MUST match
	// the CN of the agent's mTLS client certificate (see docs/design.md §10.3).
	// +required
	// +kubebuilder:validation:MinLength=1
	ClusterID string `json:"clusterId"`

	// LiqoClusterID is the Liqo cluster identifier of the provider. Consumer
	// Agents use this value when executing liqoctl peer.
	// +required
	// +kubebuilder:validation:MinLength=1
	LiqoClusterID string `json:"liqoClusterId"`

	// ClusterType classifies the provider's capacity for chunk sizing; see
	// ChunkType in common_types.go.
	// +required
	ClusterType ChunkType `json:"clusterType"`

	// Resources is the currently allocatable capacity donated by the provider.
	// +required
	Resources AdvertisedResources `json:"resources"`

	// Topology is optional and is used by the Broker's decision engine and by
	// the gRPC server when stamping Liqo virtual-node labels.
	// +optional
	Topology *Topology `json:"topology,omitempty"`

	// Price is the cost per chunk-hour advertised by the provider. Surfaced to
	// the Cluster Autoscaler via the gRPC server's PricingNodePrice method to
	// enable CA's price Expander. Non-negative.
	// +optional
	Price *resource.Quantity `json:"price,omitempty"`
}

// ClusterAdvertisementStatus is the Broker's observed view of the advertisement.
type ClusterAdvertisementStatus struct {
	// ObservedGeneration is the metadata.generation the Broker most recently
	// reconciled. Used to detect stale status writes after concurrent updates.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastSeen is the timestamp of the latest advertisement received from this
	// cluster. Updated on every successful POST /api/v1/advertisements (30 s
	// cadence).
	// +optional
	LastSeen *metav1.Time `json:"lastSeen,omitempty"`

	// Available is true when LastSeen is newer than agent-heartbeat-timeout
	// (default 30 s, from the chunk-config ConfigMap). Unavailable providers
	// are excluded from /nodegroups responses and from new reservation decisions.
	Available bool `json:"available"`

	// TotalChunks is the number of chunks computed from the latest advertisement
	// using the chunk-config ConfigMap (see docs/design.md §6).
	// +optional
	// +kubebuilder:validation:Minimum=0
	TotalChunks int32 `json:"totalChunks,omitempty"`

	// ReservedChunks is the number of chunks currently bound to active
	// Reservations (those whose phase is not Released | Expired | Failed).
	// +optional
	// +kubebuilder:validation:Minimum=0
	ReservedChunks int32 `json:"reservedChunks,omitempty"`

	// AvailableChunks is TotalChunks minus ReservedChunks. Consumers see this
	// value as the maxSize of the corresponding node group.
	// +optional
	// +kubebuilder:validation:Minimum=0
	AvailableChunks int32 `json:"availableChunks,omitempty"`

	// Conditions reports the current state of the ClusterAdvertisement.
	// Standard condition types:
	//   - "Available"  — the provider is reporting within agent-heartbeat-timeout.
	//   - "Ready"      — chunks have been calculated and the advertisement is
	//                    usable by the decision engine.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cadv
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.clusterType`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.totalChunks`
// +kubebuilder:printcolumn:name="Reserved",type=integer,JSONPath=`.status.reservedChunks`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableChunks`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.available`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterAdvertisement is the Broker-side record of a provider cluster's
// donatable capacity. Upserted by each Provider Agent every 30 s.
type ClusterAdvertisement struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the desired state advertised by the provider.
	// +required
	Spec ClusterAdvertisementSpec `json:"spec"`

	// status is the Broker's observed state for this advertisement.
	// +optional
	Status ClusterAdvertisementStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterAdvertisementList contains a list of ClusterAdvertisement.
type ClusterAdvertisementList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterAdvertisement `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterAdvertisement{}, &ClusterAdvertisementList{})
}
