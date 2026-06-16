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
	if entry, ok := s.consumers.Lookup(ClusterIDFromContext(ctx)); ok &&
		entry.Placement.Type == autoscalingv1alpha1.PlacementStrategyPrice {
		applyPricePreference(views, costs, priced)
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
// consumer (cheapest-first greedy). Within each chunk type it keeps head-room
// only on the cheapest *priced* provider that still has capacity, and removes
// head-room from every other provider in that type (MaxSize = CurrentReserved)
// so the Cluster Autoscaler can only grow the cheapest one. When the cheapest
// fills up it loses its capacity and the next /nodegroups call promotes the
// next-cheapest.
//
// The rules are strictly additive — the function only ever *removes* growable
// options, never adds a choice the Broker didn't already expose:
//   - A chunk type with no priced provider that has capacity is left untouched
//     (today's behaviour). This covers the "policy set but no prices" case and
//     the unpriced tail of "prices partially set".
//   - Unpriced (or partially-priced) providers are never the winner, so they
//     are reachable only once every priced provider of that type is exhausted —
//     i.e. last resort.
//
// views[i]/costs[i]/priced[i] describe the same advertisement.
func applyPricePreference(views []NodeGroupView, costs []float64, priced []bool) {
	byType := map[brokerv1alpha1.ChunkType][]int{}
	for i := range views {
		byType[views[i].Type] = append(byType[views[i].Type], i)
	}
	for _, idxs := range byType {
		winner := -1
		for _, i := range idxs {
			if !priced[i] || views[i].MaxSize-views[i].CurrentReserved <= 0 {
				continue // unpriced, or no capacity to grow
			}
			if winner == -1 || costs[i] < costs[winner] ||
				(costs[i] == costs[winner] && views[i].ProviderClusterID < views[winner].ProviderClusterID) {
				winner = i
			}
		}
		if winner == -1 {
			continue // no priced provider with capacity → leave the type as-is
		}
		for _, i := range idxs {
			if i != winner {
				views[i].MaxSize = views[i].CurrentReserved // remove head-room
			}
		}
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
