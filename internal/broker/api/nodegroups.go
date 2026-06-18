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

package api

import (
	"context"
	"net/http"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// -----------------------------------------------------------------------------
// 7.3.4 — GET /api/v1/nodegroups   (Consumer Agent → Broker)
// -----------------------------------------------------------------------------
//
// The endpoint lists every Available ClusterAdvertisement and returns one
// NodeGroupView per advertisement (each CR carries exactly one ChunkType so the
// "(provider × chunkType)" pair is implicit). By default it stays dumb — no
// scoring or selection — and lets the Cluster Autoscaler choose where to
// schedule.
//
// The ONE exception is price preference: when the calling consumer has a
// ConsumerPolicy with placement type "Price" (pushed on its heartbeat), the
// Broker masks the view so the consumer can only grow the cheapest priced
// provider with capacity (per chunk type). The CA itself never sees price — it
// just grows the one pool the Broker left head-room on (see applyPricePreference
// for the exact, strictly-additive rules). Views are per-consumer because each
// consumer's Agent authenticates with its own mTLS identity.
//
// Sort order is stable: (clusterId ASC, chunkType ASC).

func (s *Server) handleNodeGroupsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := r.Header.Get(HeaderRequestID)

	var list brokerv1alpha1.ClusterAdvertisementList
	if err := s.client.List(ctx, &list, client.InNamespace(s.namespace)); err != nil {
		s.log.Error(err, "list ClusterAdvertisements failed", "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code:      ErrCodeInternalError,
			Message:   "advertisement list failed",
			RequestID: requestID,
		})
		return
	}

	// Project every Available advertisement, computing its per-chunk cost once
	// so both the view's Cost field and the price-preference masking reuse it.
	avail := make([]*brokerv1alpha1.ClusterAdvertisement, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Status.Available {
			avail = append(avail, &list.Items[i])
		}
	}
	views := make([]NodeGroupView, len(avail))
	costs := make([]float64, len(avail))
	priced := make([]bool, len(avail))
	for i, cadv := range avail {
		cost, ok := s.perChunkCost(cadv)
		costs[i], priced[i] = cost, ok
		views[i] = s.nodeGroupViewFromAdvertisement(cadv, cost, ok)
	}

	// Per-consumer price preference: narrow to the cheapest priced provider
	// with capacity within each chunk type. No policy (or no priced provider
	// with capacity) ⇒ no masking ⇒ today's behaviour.
	consumerID := ClusterIDFromContext(ctx)
	if entry, ok := s.consumers.Lookup(consumerID); ok &&
		entry.Placement.Type == autoscalingv1alpha1.PlacementStrategyPrice {
		// In-flight reservations gate the "spill to the next-cheapest" step so we
		// don't prematurely expose a dearer provider while the chosen one's chunk
		// is still peering (CA would grab it and then have to unpeer it).
		inflight, err := s.inFlightByProvider(ctx, consumerID)
		if err != nil {
			s.log.Error(err, "list Reservations for placement gate failed", "requestId", requestID)
			writeError(w, http.StatusInternalServerError, ErrorResponse{
				Code: ErrCodeInternalError, Message: "reservation list failed", RequestID: requestID,
			})
			return
		}
		applyPricePreference(views, costs, priced, inflight)
	}

	sort.SliceStable(views, func(i, j int) bool {
		if views[i].ProviderClusterID != views[j].ProviderClusterID {
			return views[i].ProviderClusterID < views[j].ProviderClusterID
		}
		return string(views[i].Type) < string(views[j].Type)
	})

	writeJSON(w, http.StatusOK, NodeGroupListResponse{
		NodeGroups:      views,
		Generation:      0, // step 5b will surface a real revision once the reconciler watches CRs.
		ServedAt:        metav1.Now(),
		CacheAgeSeconds: 0,
	})
}

// applyPricePreference masks the node-group view for a price-preferring
// consumer (cheapest-first greedy). Within each chunk type it walks the priced
// providers cheapest-first and exposes exactly one — the cheapest that still has
// head-room — masking every other provider (MaxSize = CurrentReserved) so the
// Cluster Autoscaler can only grow that one.
//
// The "spill to the next-cheapest" only happens once the cheaper provider is
// genuinely exhausted: full AND fully settled. While the cheapest provider is
// full but a reservation of ours is still mid-peering (in flight), the masking
// holds CA by exposing NOTHING growable in that type — CA waits for the chunk's
// virtual node to materialise instead of prematurely peering a dearer provider
// it would then have to unpeer once the cheaper node absorbs the pods. The
// in-flight gate is policy-agnostic: any placement policy that narrows a
// consumer to a preferred provider inherits the same no-premature-spill rule.
//
// The rules are strictly additive — the function only ever *removes* growable
// options, never adds a choice the Broker didn't already expose:
//   - A chunk type with no priced provider that has capacity or is settling is
//     left untouched (today's behaviour: the "policy set but no prices" case and
//     the unpriced tail of "prices partially set").
//   - Unpriced (or partially-priced) providers are never the winner, so they are
//     reachable only once every priced provider of that type is exhausted.
//
// views[i]/costs[i]/priced[i] describe the same advertisement; inflight[provider]
// is true when this consumer has a not-yet-Peered reservation on that provider.
func applyPricePreference(views []NodeGroupView, costs []float64, priced []bool, inflight map[string]bool) {
	byType := map[brokerv1alpha1.ChunkType][]int{}
	for i := range views {
		byType[views[i].Type] = append(byType[views[i].Type], i)
	}
	for _, idxs := range byType {
		// Priced providers in this type, cheapest first (name break ties).
		order := make([]int, 0, len(idxs))
		for _, i := range idxs {
			if priced[i] {
				order = append(order, i)
			}
		}
		sort.SliceStable(order, func(a, b int) bool {
			ia, ib := order[a], order[b]
			if costs[ia] != costs[ib] {
				return costs[ia] < costs[ib]
			}
			return views[ia].ProviderClusterID < views[ib].ProviderClusterID
		})

		chosen, blockAll := -1, false
		for _, i := range order {
			if views[i].MaxSize-views[i].CurrentReserved > 0 {
				chosen = i // cheapest priced provider with head-room → grow this one
				break
			}
			if inflight[views[i].ProviderClusterID] {
				blockAll = true // it's full but still peering → hold CA, don't spill yet
				break
			}
			// exhausted (full AND settled) → try the next-cheapest priced provider
		}

		switch {
		case blockAll:
			for _, i := range idxs {
				views[i].MaxSize = views[i].CurrentReserved
			}
		case chosen != -1:
			for _, i := range idxs {
				if i != chosen {
					views[i].MaxSize = views[i].CurrentReserved
				}
			}
		default:
			// No priced provider has capacity or is settling → leave the type
			// as-is (unpriced providers remain reachable as a last resort).
		}
	}
}

// inFlightByProvider reports, per provider, whether THIS consumer has a
// reservation still being provisioned — created/peering but whose Liqo virtual
// node has not materialised yet (not Peered, not terminal). The placement
// masking uses it to keep the consumer on its chosen provider while that
// provider's chunk finishes peering, rather than spilling onto a dearer one.
func (s *Server) inFlightByProvider(ctx context.Context, consumerID string) (map[string]bool, error) {
	var list brokerv1alpha1.ReservationList
	if err := s.client.List(ctx, &list, client.InNamespace(s.namespace)); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for i := range list.Items {
		r := &list.Items[i]
		if r.Spec.ConsumerClusterID == consumerID && reservationInFlight(r.Status.Phase) {
			out[r.Spec.ProviderClusterID] = true
		}
	}
	return out, nil
}

// reservationInFlight is true while a Reservation's chunk is being provisioned —
// before its virtual node is ready (Peered) and before any terminal phase.
// Peered / Unpeering / Released / Failed / Expired are NOT in flight.
func reservationInFlight(p brokerv1alpha1.ReservationPhase) bool {
	switch p {
	case "",
		brokerv1alpha1.ReservationPhasePending,
		brokerv1alpha1.ReservationPhaseGeneratingKubeconfig,
		brokerv1alpha1.ReservationPhaseKubeconfigReady,
		brokerv1alpha1.ReservationPhasePeering:
		return true
	default:
		return false
	}
}

// nodeGroupViewFromAdvertisement projects one ClusterAdvertisement onto the
// wire shape consumed by the Consumer Agent (and ultimately by the Cluster
// Autoscaler via the gRPC server).
//
// MinSize is hard-coded to 0 — chunks are always optional. MaxSize is the
// provider's total chunk count (Option B in docs/design.md): the consumer
// can reserve up to that, no further. CurrentReserved tracks how many of
// those chunks are already bound to active reservations.
//
// ChunkResources is recovered by re-running the configured sizer over the
// advertisement so callers see the same per-chunk shape that downstream
// node templates will report. Cost is the Broker-computed per-chunk cost
// (nil when the provider is unpriced), kept for the dashboard and the gRPC
// server's PricingNodePrice back-compat.
func (s *Server) nodeGroupViewFromAdvertisement(
	cadv *brokerv1alpha1.ClusterAdvertisement, cost float64, priced bool,
) NodeGroupView {
	return NodeGroupView{
		ID:                    nodeGroupID(cadv.Spec.ClusterID, cadv.Spec.ClusterType),
		ProviderClusterID:     cadv.Spec.ClusterID,
		ProviderLiqoClusterID: cadv.Spec.LiqoClusterID,
		Type:                  cadv.Spec.ClusterType,
		MinSize:               0,
		MaxSize:               cadv.Status.TotalChunks,
		CurrentReserved:       cadv.Status.ReservedChunks,
		ChunkResources:        s.perChunkResources(cadv),
		Cost:                  costQuantity(cost, priced),
		Topology:              cadv.Spec.Topology,
		// LiqoLabels / LiqoTaints aren't on ClusterAdvertisementSpec yet
		// (designed but pending). When they land, populate Labels /
		// Taints straight from the spec; for now, leave them nil.
	}
}
