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

// Package sites picks the edge site of a region for a booking (M3).
//
// A region orchestrator runs in the region's own cluster, so it sees the
// region's nodes. Each edge node carries a label naming its site. A booking
// becomes one virtual node, and all its pods are pinned to one site, so the
// whole booking must fit in one site:
//
//	free(site) = allocatable of the site's ready, schedulable nodes
//	             - resources of the active Reservations already on that site
//
// The site with the most free CPU that still fits the request wins.
package sites

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// counted are the resources a site is checked on.
var counted = []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, "nvidia.com/gpu"}

// Picker chooses an edge site for a booking.
type Picker struct {
	// Reader reads Nodes and Reservations. Use an uncached reader (the
	// manager's API reader) so a booking sees the one made just before it.
	Reader client.Reader
	// Label is the node label whose value names a node's edge site.
	Label string
	// Namespace is where the Broker's Reservations live.
	Namespace string
}

// Pick returns the site with the most free CPU that can hold want, or "" when
// no site can.
func (p *Picker) Pick(ctx context.Context, want corev1.ResourceList) (string, error) {
	free, err := p.Free(ctx)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(free))
	for name := range free {
		names = append(names, name)
	}
	sort.Strings(names) // stable tie-break
	best := ""
	var bestCPU resource.Quantity
	for _, name := range names {
		if !fits(free[name], want) {
			continue
		}
		cpu := free[name][corev1.ResourceCPU]
		if best == "" || cpu.Cmp(bestCPU) > 0 {
			best, bestCPU = name, cpu
		}
	}
	return best, nil
}

// Free returns, per site, what is left for new bookings.
func (p *Picker) Free(ctx context.Context) (map[string]corev1.ResourceList, error) {
	var nodes corev1.NodeList
	if err := p.Reader.List(ctx, &nodes, client.HasLabels{p.Label}); err != nil {
		return nil, fmt.Errorf("list nodes with label %q: %w", p.Label, err)
	}
	free := map[string]corev1.ResourceList{}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		site := n.Labels[p.Label]
		if site == "" || !usable(n) {
			continue
		}
		if free[site] == nil {
			free[site] = corev1.ResourceList{}
		}
		for _, name := range counted {
			if q, ok := n.Status.Allocatable[name]; ok {
				add(free[site], name, q, 1)
			}
		}
	}

	var resvs brokerv1alpha1.ReservationList
	if err := p.Reader.List(ctx, &resvs, client.InNamespace(p.Namespace)); err != nil {
		return nil, fmt.Errorf("list reservations: %w", err)
	}
	for i := range resvs.Items {
		r := &resvs.Items[i]
		if free[r.Spec.Site] == nil || !active(r.Status.Phase) {
			continue
		}
		n := int64(max(r.Spec.ChunkCount, 1))
		for _, name := range counted {
			if q, ok := r.Spec.Resources[name]; ok {
				add(free[r.Spec.Site], name, q, -n)
			}
		}
	}
	return free, nil
}

// usable reports whether a node is Ready and schedulable.
func usable(n *corev1.Node) bool {
	if n.Spec.Unschedulable {
		return false
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// active reports whether a Reservation in this phase still holds its room.
func active(p brokerv1alpha1.ReservationPhase) bool {
	switch p {
	case brokerv1alpha1.ReservationPhaseReleased,
		brokerv1alpha1.ReservationPhaseExpired,
		brokerv1alpha1.ReservationPhaseFailed:
		return false
	default:
		return true
	}
}

// add adds n times q to list[name].
func add(list corev1.ResourceList, name corev1.ResourceName, q resource.Quantity, n int64) {
	delta := q.DeepCopy()
	if n < 0 {
		delta.Neg()
		n = -n
	}
	for range n {
		cur := list[name]
		cur.Add(delta)
		list[name] = cur
	}
}

// fits reports whether every resource of want is within free.
func fits(free, want corev1.ResourceList) bool {
	for name, w := range want {
		if w.Sign() <= 0 {
			continue
		}
		have, ok := free[name]
		if !ok || have.Cmp(w) < 0 {
			return false
		}
	}
	return true
}
