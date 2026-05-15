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

// Package bootstrap installs federation-autoscaler's per-cluster
// prerequisites (cert-manager, Liqo, the federation-autoscaler CRDs) and
// stamps the agent-config ConfigMap with each cluster's identity. The
// step-13 e2e suite invokes these helpers once per Kind cluster after
// `kind.Bootstrap` brings them up.
package bootstrap

import (
	"fmt"

	"github.com/netgroup-polito/federation-autoscaler/test/e2e/kind"
)

// CentralBrokerNodePort is the NodePort the e2e overlay pins the broker
// Service to so the consumer / provider agents on other Kind clusters
// can reach the central cluster's broker over the shared docker
// network. Pinned (rather than auto-assigned) so the URL the agents
// configure is deterministic.
const CentralBrokerNodePort = 30443

// BrokerNamespace is where the broker Deployment lives — matches the
// namespace baked into the config/broker/ overlay.
const BrokerNamespace = "federation-autoscaler-system"

// Identity describes a cluster's federation-autoscaler-facing identity.
// Same fields the agent-config ConfigMap holds, see config/agent/base/
// configmap.yaml. clusterID and liqoClusterID are intentionally equal
// in v1 (docs/design.md §7.0 "Cluster identity": "Every agent's clusterId
// equals its Liqo cluster ID, so one identity is used throughout.").
type Identity struct {
	// ClusterID is the broker-facing identifier for this cluster. The
	// agent's mTLS client cert MUST have this as its CN.
	ClusterID string

	// LiqoClusterID is the Liqo cluster identifier for this cluster.
	// Identical to ClusterID by design (see docs/design.md §7.0).
	LiqoClusterID string

	// BrokerURL is the HTTPS endpoint of the broker the agent on this
	// cluster polls. Always the central cluster's broker, reachable via
	// the docker network the four Kind clusters share.
	BrokerURL string
}

// IdentityForRole derives the Identity for a Kind role. central gets
// its own identity even though it doesn't run an agent — Liqo's install
// requires a cluster-id and we reuse the same naming.
func IdentityForRole(role kind.Role) Identity {
	clusterID := kind.ClusterName(role)
	return Identity{
		ClusterID:     clusterID,
		LiqoClusterID: clusterID,
		BrokerURL:     centralBrokerURL(),
	}
}

// centralBrokerURL returns the URL the agents use to reach the central
// cluster's broker. Resolved via Kind's docker network DNS — the
// control-plane container is named "<cluster-name>-control-plane".
func centralBrokerURL() string {
	return fmt.Sprintf("https://%s-control-plane:%d",
		kind.ClusterName(kind.RoleCentral), CentralBrokerNodePort)
}
