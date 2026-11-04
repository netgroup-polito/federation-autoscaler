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
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/broker/chunk"
)

// PlacementInput is everything the placement decision needs, with no I/O
// behind it. The Broker fills it from its CRs and consumer registry; a
// consumer-side brain can fill it from its own sources and get the same
// answer, because BuildNodeGroups is the single implementation of the
// placement policies.
type PlacementInput struct {
	// Advertisements are the candidate providers. Entries whose
	// Status.Available is false are skipped, so callers may pass the full list.
	Advertisements []*brokerv1alpha1.ClusterAdvertisement

	// Consumer carries the calling consumer's placement policy and location.
	// The zero value means the Standard default with no location.
	Consumer ConsumerEntry

	// InFlight[provider] is true while this consumer has a reservation on that
	// provider that is still being provisioned (see reservationInFlight).
	InFlight map[string]bool

	// Sizer recovers the per-chunk shape of each advertisement. Nil means the
	// default sizer.
	Sizer chunk.Sizer
}

// BuildNodeGroups projects the available advertisements onto node-group views
// and applies the consumer's placement policy: narrow to the single best
// provider with capacity within each chunk type. "best" is the composite
// Standard default (most free capacity, renewable bonus) when no policy is set,
// or cheapest (Price) / greenest (Eco) / closest (Latency) when one is.
// ConsumerChoice is the exception: it leaves the whole list unmasked for the
// consumer to choose.
//
// Sort order of the result is stable: (clusterId ASC, chunkType ASC).
func BuildNodeGroups(in PlacementInput) NodeGroupListResponse {
	sizer := in.Sizer
	if sizer == nil {
		sizer = chunk.NewDefaultSizer()
	}
	entry := in.Consumer

	// Project every Available advertisement, computing its per-chunk cost once
	// so both the view's Cost field and the price-preference masking reuse it.
	avail := make([]*brokerv1alpha1.ClusterAdvertisement, 0, len(in.Advertisements))
	for _, cadv := range in.Advertisements {
		if cadv != nil && cadv.Status.Available {
			avail = append(avail, cadv)
		}
	}
	views := make([]NodeGroupView, len(avail))
	costs := make([]float64, len(avail))
	priced := make([]bool, len(avail))
	carbons := make([]float64, len(avail))
	hasCarbon := make([]bool, len(avail))
	for i, cadv := range avail {
		cost, ok := perChunkCostWith(sizer, cadv)
		costs[i], priced[i] = cost, ok
		carbons[i], hasCarbon[i] = weightedCarbon(cadv)
		views[i] = nodeGroupView(sizer, cadv, cost, ok)
	}

	// In-flight reservations gate the "spill to the next-best" step so we don't
	// prematurely expose a worse provider while the chosen one's chunk is still
	// peering (CA would grab it and then have to unpeer it). The gate is
	// policy-agnostic, so every strategy shares it.
	inflight := in.InFlight
	if inflight == nil {
		inflight = map[string]bool{}
	}

	latencyShortlist := false
	switch entry.Placement.Type {
	case autoscalingv1alpha1.PlacementStrategyPrice:
		applyPricePreference(views, costs, priced, inflight)
		setPlacementMetric(views, costs, priced)
	case autoscalingv1alpha1.PlacementStrategyEco:
		applyEcoPreference(views, carbons, hasCarbon, inflight)
		setPlacementMetric(views, carbons, hasCarbon)
	case autoscalingv1alpha1.PlacementStrategyRandom:
		applyRandomPreference(views, inflight)
	case autoscalingv1alpha1.PlacementStrategyLatency:
		// Latency is a consumer↔provider PAIR metric: distance depends on the
		// calling consumer's own location (from its heartbeat). Unlike the other
		// policies this does NOT narrow to one here — it exposes the top-N
		// nearest-with-capacity as a shortlist and lets the Consumer Agent UDP-probe
		// them and grow the lowest-RTT one. If the consumer has no location, this is
		// a no-op (all providers stay exposed) and no shortlist is signalled.
		distances, hasDist := consumerProviderDistances(entry, avail)
		latencyShortlist = applyLatencyTopN(views, distances, hasDist, inflight, latencyShortlistSize)
		setPlacementMetric(views, distances, hasDist)
	case autoscalingv1alpha1.PlacementStrategyConsumerChoice:
		// The choice belongs to the consumer: its agent hands every candidate
		// plus the operator's natural-language request to a local LLM
		// (internal/agent/ollama), so nothing is pre-picked here (docs/design.md,
		// placement table). No masking at all: every available provider keeps
		// MaxSize = TotalChunks, which already leaves a full provider
		// non-growable (MaxSize == CurrentReserved) without any help, and
		// unavailable ones were dropped above. The in-flight gate is not applied
		// either -- it only exists to stop a single-winner policy from spilling
		// to the runner-up, and there is no runner-up to spill to.
		// No PlacementMetric: there is no single metric the choice ranks on.
	default:
		// Empty (no ConsumerPolicy) or "Standard" → the composite default. No stable
		// per-provider metric is exposed (Standard's most-free score wanders as
		// capacity fills), so the re-eval loop never migrates a Standard reservation.
		applyStandardPreference(views, avail, inflight)
	}

	sort.SliceStable(views, func(i, j int) bool {
		if views[i].ProviderClusterID != views[j].ProviderClusterID {
			return views[i].ProviderClusterID < views[j].ProviderClusterID
		}
		return string(views[i].Type) < string(views[j].Type)
	})

	return NodeGroupListResponse{
		NodeGroups:       views,
		LatencyShortlist: latencyShortlist,
		AppliedPlacement: entry.Placement.Type,
		Generation:       0, // step 5b will surface a real revision once the reconciler watches CRs.
		ServedAt:         metav1.Now(),
		CacheAgeSeconds:  0,
	}
}

// nodeGroupView projects one ClusterAdvertisement onto the wire shape consumed
// by the Consumer Agent (and ultimately by the Cluster Autoscaler via the gRPC
// server).
//
// MinSize is hard-coded to 0 — chunks are always optional. MaxSize is the
// provider's total chunk count (Option B in docs/design.md): the consumer
// can reserve up to that, no further. CurrentReserved tracks how many of
// those chunks are already bound to active reservations.
//
// ChunkResources is recovered by re-running the sizer over the advertisement
// so callers see the same per-chunk shape that downstream node templates will
// report. Cost is the computed per-chunk cost (nil when the provider is
// unpriced), kept for the dashboard and the gRPC server's PricingNodePrice
// back-compat.
func nodeGroupView(
	sizer chunk.Sizer, cadv *brokerv1alpha1.ClusterAdvertisement, cost float64, priced bool,
) NodeGroupView {
	return NodeGroupView{
		ID:                    nodeGroupID(cadv.Spec.ClusterID, cadv.Spec.ClusterType),
		ProviderClusterID:     cadv.Spec.ClusterID,
		ProviderLiqoClusterID: cadv.Spec.LiqoClusterID,
		Type:                  cadv.Spec.ClusterType,
		MinSize:               0,
		MaxSize:               cadv.Status.TotalChunks,
		CurrentReserved:       cadv.Status.ReservedChunks,
		ChunkResources:        perChunkResourcesWith(sizer, cadv),
		Cost:                  costQuantity(cost, priced),
		Topology:              cadv.Spec.Topology,
		ProbeEndpoint:         cadv.Spec.ProbeEndpoint,
		CarbonIntensity:       cadv.Spec.CarbonIntensity,
		UnitPrices:            cadv.Spec.UnitPrices,
		// LiqoLabels / LiqoTaints aren't on ClusterAdvertisementSpec yet
		// (designed but pending). When they land, populate Labels /
		// Taints straight from the spec; for now, leave them nil.
	}
}

// perChunkResourcesWith returns the per-chunk capacity matching cadv's
// ClusterType as computed by sizer, falling back to the first group the sizer
// produced, or an empty list when it produced none.
func perChunkResourcesWith(sizer chunk.Sizer, cadv *brokerv1alpha1.ClusterAdvertisement) corev1.ResourceList {
	results := sizer.Size(cadv.Spec.Resources.Allocatable)
	for _, r := range results {
		if r.Type == cadv.Spec.ClusterType {
			return r.PerChunk
		}
	}
	if len(results) > 0 {
		return results[0].PerChunk
	}
	return corev1.ResourceList{}
}
