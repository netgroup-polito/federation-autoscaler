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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PlacementStrategy selects how the Broker places a consumer's chunk requests
// across the available provider clusters. An empty value (or a missing
// ConsumerPolicy) means the Broker keeps its default behaviour: it exposes all
// available providers and lets the Cluster Autoscaler pick — i.e. no
// price-based preference. The Broker is the decision-maker only when a strategy
// is set; the Cluster Autoscaler never sees price.
// +kubebuilder:validation:Enum=Price
type PlacementStrategy string

const (
	// PlacementStrategyPrice makes the Broker prefer, for this consumer, the
	// cheapest *priced* provider(s) that still have capacity (cheapest-first
	// greedy). Providers without a price are reached only as a last resort.
	PlacementStrategyPrice PlacementStrategy = "Price"
)

// PlacementPolicy is the (currently price-only, deliberately extensible)
// placement policy a consumer declares for itself. It is carried on the
// Consumer Agent's heartbeat to the Broker.
type PlacementPolicy struct {
	// Type selects the placement strategy. Empty means the Broker default
	// (no price preference). The only supported value today is "Price".
	// +optional
	Type PlacementStrategy `json:"type,omitempty"`
}

// ConsumerPolicySpec is the desired placement policy for the consumer cluster
// it lives on. It is operator-stamped (manually, or via the fa_consumer Ansible
// role) and may be edited at any time; the Consumer Agent re-reads it on every
// heartbeat (~15 s) so changes take effect without a restart.
type ConsumerPolicySpec struct {
	// Placement is the resource-placement policy the Broker applies to this
	// consumer's requests.
	// +optional
	Placement PlacementPolicy `json:"placement,omitempty,omitzero"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=cpolicy
// +kubebuilder:printcolumn:name="Placement",type=string,JSONPath=`.spec.placement.type`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ConsumerPolicy is a consumer-cluster-local declaration of how the Broker
// should place this consumer's borrowed capacity (e.g. prefer the cheapest
// providers). It lives only on the consumer cluster; the Broker never reads it
// directly — the Consumer Agent pushes its spec on the heartbeat, preserving
// the agent-initiated (no Broker→cluster dial-in) communication model.
type ConsumerPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the desired placement policy.
	// +required
	Spec ConsumerPolicySpec `json:"spec"`
}

// +kubebuilder:object:root=true

// ConsumerPolicyList contains a list of ConsumerPolicy.
type ConsumerPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ConsumerPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ConsumerPolicy{}, &ConsumerPolicyList{})
}
