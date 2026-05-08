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

// Package snapshot computes the provider's advertisable capacity by
// walking the local cluster's Nodes and summing Allocatable across the
// ones the provider is willing to donate. The result feeds directly
// into the AdvertisementRequest the publisher (substep 8c) sends to
// the Broker every 30 s (docs/design.md §7.3.1).
//
// Counting policy (v1):
//   - skip cordoned nodes (Spec.Unschedulable=true) — explicit operator
//     opt-out
//   - skip nodes whose Ready condition is not True — capacity isn't
//     actually deliverable
//   - include everything else, including control-plane nodes
//
// Subtler policies (control-plane exclusion, GPU-only counting, etc.)
// belong to the chunk-config ConfigMap on the Broker side, not here:
// the Broker is the single decision point, so the agent's job is to
// publish the truth and let the Broker filter.
package snapshot

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// Snapshot is the result of one walk of the local cluster's Nodes.
// All fields are populated unconditionally; callers are free to ignore
// what they don't need.
type Snapshot struct {
	// Allocatable is the sum of Status.Allocatable across counted
	// Nodes. Keys are propagated verbatim from the Node objects so
	// custom resources (nvidia.com/gpu, vendor.com/foo, …) survive the
	// summation without filtering.
	Allocatable corev1.ResourceList

	// CountedNodes is the number of Nodes that contributed to
	// Allocatable. Logged by the publisher and surfaced as a metric.
	CountedNodes int

	// SkippedNodes maps each skipped Node's name to the reason it was
	// skipped. Useful for debugging when a provider's advertisement
	// suddenly drops capacity.
	SkippedNodes map[string]string
}

// Take walks the local cluster's Nodes and returns a Snapshot. It does
// no I/O beyond a single List call; failures bubble up wrapped so the
// caller can surface them through the publisher's health hook.
func Take(ctx context.Context, c ctrlclient.Client) (*Snapshot, error) {
	if c == nil {
		return nil, fmt.Errorf("snapshot: client is nil")
	}
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("snapshot: list nodes: %w", err)
	}

	out := &Snapshot{
		Allocatable:  corev1.ResourceList{},
		SkippedNodes: map[string]string{},
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if reason := skipReason(n); reason != "" {
			out.SkippedNodes[n.Name] = reason
			continue
		}
		addResources(out.Allocatable, n.Status.Allocatable)
		out.CountedNodes++
	}
	return out, nil
}

// skipReason returns a non-empty human-readable reason when n must NOT
// contribute to the snapshot, or "" when it should be counted.
func skipReason(n *corev1.Node) string {
	if n.Spec.Unschedulable {
		return "cordoned"
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue {
			return fmt.Sprintf("not ready (status=%s)", c.Status)
		}
	}
	return ""
}

// addResources adds every (key, quantity) pair in src into dst.
// corev1.ResourceList stores Quantity by value, so the read-modify-write
// dance below is necessary for the Add to actually persist in the map.
func addResources(dst, src corev1.ResourceList) {
	for k, v := range src {
		existing, ok := dst[k]
		if !ok {
			dst[k] = v.DeepCopy()
			continue
		}
		existing.Add(v)
		dst[k] = existing
	}
}
