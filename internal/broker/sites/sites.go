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
//
// With a latency limit, a site also needs coordinates (annotations
// federation-autoscaler.io/latitude and /longitude on any of its nodes) and an
// estimated round-trip latency to the consumer within the limit.
package sites

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/broker/geo"
)

// Node annotations giving an edge site's coordinates, in decimal degrees.
const (
	LatitudeAnnotation  = "federation-autoscaler.io/latitude"
	LongitudeAnnotation = "federation-autoscaler.io/longitude"
)

// LatencyLimit is a booking's hard latency limit.
type LatencyLimit struct {
	// Latitude and Longitude are the consumer's coordinates.
	Latitude, Longitude float64
	// MaxMs is the highest estimated round-trip latency allowed.
	MaxMs float64
}

// Reason says why Pick found no site.
type Reason int

const (
	// Found: a site was picked.
	Found Reason = iota
	// NoRoom: no site has room for the booking.
	NoRoom
	// TooFar: some site has room, but none is within the latency limit.
	TooFar
)

// Site is an edge site's free room and, when known, its coordinates.
type Site struct {
	Free           corev1.ResourceList
	Latitude       float64
	Longitude      float64
	HasCoordinates bool
}

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

// Pick returns the site with the most free CPU that can hold want and, with a
// limit, is within it. With no such site it returns "" and the reason.
func (p *Picker) Pick(ctx context.Context, want corev1.ResourceList, limit *LatencyLimit) (string, Reason, error) {
	all, err := p.Sites(ctx)
	if err != nil {
		return "", NoRoom, err
	}
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names) // stable tie-break
	best, roomy := "", false
	var bestCPU resource.Quantity
	for _, name := range names {
		site := all[name]
		if !fits(site.Free, want) {
			continue
		}
		roomy = true
		if limit != nil && !site.within(limit) {
			continue
		}
		cpu := site.Free[corev1.ResourceCPU]
		if best == "" || cpu.Cmp(bestCPU) > 0 {
			best, bestCPU = name, cpu
		}
	}
	switch {
	case best != "":
		return best, Found, nil
	case roomy:
		return "", TooFar, nil
	default:
		return "", NoRoom, nil
	}
}

// within reports whether the site is known to be within the latency limit.
func (s *Site) within(limit *LatencyLimit) bool {
	if !s.HasCoordinates {
		return false // cannot be checked, so not accepted
	}
	km := geo.HaversineKm(limit.Latitude, limit.Longitude, s.Latitude, s.Longitude)
	return geo.EstimateLatencyMs(km) <= limit.MaxMs
}

// Free returns, per site, what is left for new bookings.
func (p *Picker) Free(ctx context.Context) (map[string]corev1.ResourceList, error) {
	all, err := p.Sites(ctx)
	if err != nil {
		return nil, err
	}
	free := make(map[string]corev1.ResourceList, len(all))
	for name, site := range all {
		free[name] = site.Free
	}
	return free, nil
}

// Sites returns every edge site with its free room and coordinates.
func (p *Picker) Sites(ctx context.Context) (map[string]*Site, error) {
	var nodes corev1.NodeList
	if err := p.Reader.List(ctx, &nodes, client.HasLabels{p.Label}); err != nil {
		return nil, fmt.Errorf("list nodes with label %q: %w", p.Label, err)
	}
	all := map[string]*Site{}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		name := n.Labels[p.Label]
		if name == "" || !usable(n) {
			continue
		}
		site := all[name]
		if site == nil {
			site = &Site{Free: corev1.ResourceList{}}
			all[name] = site
		}
		for _, res := range counted {
			if q, ok := n.Status.Allocatable[res]; ok {
				add(site.Free, res, q, 1)
			}
		}
		if !site.HasCoordinates {
			site.Latitude, site.Longitude, site.HasCoordinates = coordinates(n)
		}
	}

	var resvs brokerv1alpha1.ReservationList
	if err := p.Reader.List(ctx, &resvs, client.InNamespace(p.Namespace)); err != nil {
		return nil, fmt.Errorf("list reservations: %w", err)
	}
	for i := range resvs.Items {
		r := &resvs.Items[i]
		site := all[r.Spec.Site]
		if site == nil || !active(r.Status.Phase) {
			continue
		}
		n := int64(max(r.Spec.ChunkCount, 1))
		for _, res := range counted {
			if q, ok := r.Spec.Resources[res]; ok {
				add(site.Free, res, q, -n)
			}
		}
	}
	return all, nil
}

// coordinates reads a node's site coordinates from its annotations.
func coordinates(n *corev1.Node) (lat, lon float64, ok bool) {
	lat, err1 := strconv.ParseFloat(n.Annotations[LatitudeAnnotation], 64)
	lon, err2 := strconv.ParseFloat(n.Annotations[LongitudeAnnotation], 64)
	if err1 != nil || err2 != nil || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, false
	}
	return lat, lon, true
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
