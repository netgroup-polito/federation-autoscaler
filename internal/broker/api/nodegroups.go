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
	carbons := make([]float64, len(avail))
	hasCarbon := make([]bool, len(avail))
	for i, cadv := range avail {
		cost, ok := s.perChunkCost(cadv)
		costs[i], priced[i] = cost, ok
		carbons[i], hasCarbon[i] = weightedCarbon(cadv)
		views[i] = s.nodeGroupViewFromAdvertisement(cadv, cost, ok)
	}

	// Per-consumer placement preference: narrow to the single best provider with
	// capacity within each chunk type. "best" is the composite Standard default
	// (most free capacity, renewable bonus) when no policy is set, or cheapest
	// (Price) / greenest (Eco) / closest (Latency) when one is.
	consumerID := ClusterIDFromContext(ctx)
	entry, _ := s.consumers.Lookup(consumerID) // zero ConsumerEntry ⇒ Standard default

	// In-flight reservations gate the "spill to the next-best" step so we don't
	// prematurely expose a worse provider while the chosen one's chunk is still
	// peering (CA would grab it and then have to unpeer it). The gate is
	// policy-agnostic, so every strategy shares it.
	inflight, err := s.inFlightByProvider(ctx, consumerID)
	if err != nil {
		s.log.Error(err, "list Reservations for placement gate failed", "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code: ErrCodeInternalError, Message: "reservation list failed", RequestID: requestID,
		})
		return
	}
	switch entry.Placement.Type {
	case autoscalingv1alpha1.PlacementStrategyPrice:
		applyPricePreference(views, costs, priced, inflight)
	case autoscalingv1alpha1.PlacementStrategyEco:
		applyEcoPreference(views, carbons, hasCarbon, inflight)
	case autoscalingv1alpha1.PlacementStrategyLatency:
		// Latency is a consumer↔provider PAIR metric: distance depends on the
		// calling consumer's own location (from its heartbeat). If the consumer
		// has not advertised a location, distances carry no usable value and the
		// masking is a no-op (all providers stay exposed).
		distances, hasDist := consumerProviderDistances(entry, avail)
		applyLatencyPreference(views, distances, hasDist, inflight)
	default:
		// Empty (no ConsumerPolicy) or "Standard" → the composite default.
		applyStandardPreference(views, avail, inflight)
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
	applyMetricPreference(views, costs, priced, inflight)
}

// ecoWeights weights the next 6 hours of a carbon forecast for the eco ranking —
// the near future counts most (mirrors the origin's eco scoring). Lower weighted
// score = greener.
var ecoWeights = [6]float64{0.40, 0.25, 0.15, 0.10, 0.06, 0.04}

// weightedCarbon returns the carbon value the eco ranking uses for a provider,
// and whether it has one: the 6-hour weighted forecast average when a forecast is
// advertised, else the single current CarbonIntensity, else (0, false).
func weightedCarbon(cadv *brokerv1alpha1.ClusterAdvertisement) (float64, bool) {
	if v, ok := ecoWeightedScore(cadv.Spec.CarbonForecast); ok {
		return v, true
	}
	if cadv.Spec.CarbonIntensity != nil {
		return *cadv.Spec.CarbonIntensity, true
	}
	return 0, false
}

// ecoWeightedScore returns the weighted average of the first min(6, len) forecast
// hours, normalized by the weights actually used (so a short forecast still
// scores sensibly). ok=false for an empty forecast.
func ecoWeightedScore(forecast []float64) (float64, bool) {
	n := min(len(forecast), len(ecoWeights))
	if n == 0 {
		return 0, false
	}
	var sum, wsum float64
	for i := range n {
		sum += ecoWeights[i] * forecast[i]
		wsum += ecoWeights[i]
	}
	return sum / wsum, true
}

// applyEcoPreference masks the node-group view for a carbon-preferring consumer:
// greenest-first greedy, exactly mirroring price. carbons[i]/hasCarbon[i] describe
// the same advertisement as views[i]; a provider that advertises no carbon
// intensity (hasCarbon[i] == false) is reached only once every carbon-bearing
// provider of that chunk type is exhausted (mirrors the unpriced tail).
func applyEcoPreference(views []NodeGroupView, carbons []float64, hasCarbon []bool, inflight map[string]bool) {
	applyMetricPreference(views, carbons, hasCarbon, inflight)
}

// applyLatencyPreference masks the node-group view for a latency-preferring
// consumer: closest-first greedy. distances[i]/hasDist[i] describe the same
// advertisement as views[i]; hasDist[i] is false when either endpoint lacks a
// location, so a consumer with no location (every hasDist false) yields no
// masking at all (all providers stay exposed) — see consumerProviderDistances.
func applyLatencyPreference(views []NodeGroupView, distances []float64, hasDist []bool, inflight map[string]bool) {
	applyMetricPreference(views, distances, hasDist, inflight)
}

// standardCapacityWeight / standardRenewableWeight are the composite Standard
// policy's blend: mostly remaining free capacity (spread load across providers),
// plus a bonus for self-declared renewable-energy providers. Highest score wins.
const (
	standardCapacityWeight  = 0.8
	standardRenewableWeight = 0.2
)

// applyStandardPreference masks the node-group view for a consumer with no
// explicit policy (the Standard composite default). It scores every provider by
// standardCapacityWeight·freeFraction + standardRenewableWeight·renewable
// (higher = better, so it spreads load and prefers greener providers) and feeds
// (1 − score) — lower wins — through the shared greedy masking, so the Cluster
// Autoscaler grows the single best provider (spilling to the next-best as each
// fills). Unlike the single-metric policies EVERY provider has a score, so this
// always narrows to one grower rather than exposing all.
//
// avail[i] is the advertisement behind views[i]; free capacity uses the same
// TotalChunks/ReservedChunks the view carries so a just-reserved chunk lowers the
// score and the next call naturally balances onto the now-more-free provider.
func applyStandardPreference(views []NodeGroupView, avail []*brokerv1alpha1.ClusterAdvertisement, inflight map[string]bool) {
	metric := make([]float64, len(views))
	has := make([]bool, len(views))
	for i, cadv := range avail {
		free := 0.0
		if total := cadv.Status.TotalChunks; total > 0 {
			available := max(total-cadv.Status.ReservedChunks, 0)
			free = float64(available) / float64(total)
		}
		renewable := 0.0
		if cadv.Spec.Renewable {
			renewable = 1.0
		}
		score := standardCapacityWeight*free + standardRenewableWeight*renewable
		metric[i] = 1.0 - score // invert: applyMetricPreference is lower-wins
		has[i] = true           // every provider has a composite score
	}
	applyMetricPreference(views, metric, has, inflight)
}

// applyMetricPreference is the strictly-additive greedy masking shared by all
// placement strategies (Price/Eco/Latency). Within each chunk type it walks the
// providers that HAVE a metric, best (lowest) first, and exposes exactly one —
// the best that still has head-room — masking every other provider
// (MaxSize = CurrentReserved) so the Cluster Autoscaler can only grow that one.
//
// The "spill to the next-best" only happens once the better provider is
// genuinely exhausted: full AND fully settled. While the best provider is full
// but a reservation of ours is still mid-peering (in flight), the masking holds
// CA by exposing NOTHING growable in that type — CA waits for the chunk's
// virtual node to materialise instead of prematurely peering a worse provider it
// would then have to unpeer. The in-flight gate is policy-agnostic.
//
// The rules are strictly additive — the function only ever *removes* growable
// options, never adds a choice the Broker didn't already expose:
//   - A chunk type with no metric-bearing provider that has capacity or is
//     settling is left untouched (today's behaviour: the "policy set but no
//     metric" case and the metric-less tail of "metric partially set").
//   - Metric-less providers are never the winner, so they are reachable only
//     once every metric-bearing provider of that type is exhausted.
//
// "Lower wins" fits all current strategies: cheapest cost, lowest carbon, and
// shortest distance.
//
// views[i]/metric[i]/has[i] describe the same advertisement; inflight[provider]
// is true when this consumer has a not-yet-Peered reservation on that provider.
func applyMetricPreference(views []NodeGroupView, metric []float64, has []bool, inflight map[string]bool) {
	byType := map[brokerv1alpha1.ChunkType][]int{}
	for i := range views {
		byType[views[i].Type] = append(byType[views[i].Type], i)
	}
	for _, idxs := range byType {
		// Metric-bearing providers in this type, best (lowest) first (name break ties).
		order := make([]int, 0, len(idxs))
		for _, i := range idxs {
			if has[i] {
				order = append(order, i)
			}
		}
		sort.SliceStable(order, func(a, b int) bool {
			ia, ib := order[a], order[b]
			if metric[ia] != metric[ib] {
				return metric[ia] < metric[ib]
			}
			return views[ia].ProviderClusterID < views[ib].ProviderClusterID
		})

		chosen, blockAll := -1, false
		for _, i := range order {
			if views[i].MaxSize-views[i].CurrentReserved > 0 {
				chosen = i // best provider with head-room → grow this one
				break
			}
			if inflight[views[i].ProviderClusterID] {
				blockAll = true // it's full but still peering → hold CA, don't spill yet
				break
			}
			// exhausted (full AND settled) → try the next-best provider
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
			// No metric-bearing provider has capacity or is settling → leave the
			// type as-is (metric-less providers remain reachable as a last resort).
		}
	}
}

// consumerProviderDistances returns, per advertisement in avail, the great-circle
// distance (km) from the consumer to that provider and whether it is usable.
// hasDist[i] is false when the consumer has no location, or when the provider
// advertised no coordinates (Topology nil or lat/lon both zero — the Provider
// Agent leaves them zero when its geo lookup fails, and no real region sits at
// 0,0). When the consumer has no location, every entry is unusable, so the
// caller's masking becomes a no-op and all providers stay exposed.
func consumerProviderDistances(entry ConsumerEntry, avail []*brokerv1alpha1.ClusterAdvertisement) ([]float64, []bool) {
	distances := make([]float64, len(avail))
	hasDist := make([]bool, len(avail))
	if !entry.HasLocation {
		return distances, hasDist
	}
	for i, cadv := range avail {
		t := cadv.Spec.Topology
		if t == nil || (t.Latitude == 0 && t.Longitude == 0) {
			continue
		}
		distances[i] = haversineKm(entry.Latitude, entry.Longitude, t.Latitude, t.Longitude)
		hasDist[i] = true
	}
	return distances, hasDist
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
