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

package testlib

import (
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// GrowableNodeGroups returns the node groups that have capacity to grow
// (MaxSize > CurrentReserved). These are the candidates the Cluster
// Autoscaler would consider for scale-up.
func GrowableNodeGroups(ngs []brokerapi.NodeGroupView) []brokerapi.NodeGroupView {
	var out []brokerapi.NodeGroupView
	for _, ng := range ngs {
		if ng.MaxSize > ng.CurrentReserved {
			out = append(out, ng)
		}
	}
	return out
}

// FindWinner returns the single growable node group from a non-latency response
// (Random, Eco, Price). The Broker single-winner masks losers by setting
// MaxSize = CurrentReserved, so exactly one should be growable. Returns nil
// if no growable candidate exists (all providers at capacity).
func FindWinner(ngs []brokerapi.NodeGroupView) *brokerapi.NodeGroupView {
	growable := GrowableNodeGroups(ngs)
	if len(growable) != 1 {
		return nil
	}
	return &growable[0]
}

// FindShortlist returns the growable node groups from a latency-shortlist
// response. The Broker leaves the top-3 nearest providers growable for
// consumer-side UDP probing.
func FindShortlist(ngs []brokerapi.NodeGroupView) []brokerapi.NodeGroupView {
	return GrowableNodeGroups(ngs)
}

// UniqueProviderIDs returns the distinct provider cluster IDs from a set
// of node groups.
func UniqueProviderIDs(ngs []brokerapi.NodeGroupView) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, ng := range ngs {
		if _, ok := seen[ng.ProviderClusterID]; !ok {
			seen[ng.ProviderClusterID] = struct{}{}
			out = append(out, ng.ProviderClusterID)
		}
	}
	return out
}

// NodeGroupsByProvider groups node groups by provider cluster ID.
func NodeGroupsByProvider(ngs []brokerapi.NodeGroupView) map[string][]brokerapi.NodeGroupView {
	out := make(map[string][]brokerapi.NodeGroupView)
	for _, ng := range ngs {
		out[ng.ProviderClusterID] = append(out[ng.ProviderClusterID], ng)
	}
	return out
}

// FindNodeGroupByProvider returns the first standard-type node group
// for the given provider, or nil if not found.
func FindNodeGroupByProvider(ngs []brokerapi.NodeGroupView, providerID string) *brokerapi.NodeGroupView {
	for _, ng := range ngs {
		if ng.ProviderClusterID == providerID && ng.Type == "standard" {
			return &ng
		}
	}
	return nil
}
