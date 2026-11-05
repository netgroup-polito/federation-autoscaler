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

package brain

import (
	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// fibreKmPerMs is how many kilometres of straight-line distance add one
// millisecond of round-trip time over fibre (light in glass covers about
// 200 km per ms, there and back). Real paths are longer than a straight line,
// so the estimate is optimistic: a site it rejects is surely out of the bound.
const fibreKmPerMs = 100.0

// EstimateLatencyMs is the estimated round-trip latency, in milliseconds,
// between two points distanceKm apart.
func EstimateLatencyMs(distanceKm float64) float64 {
	return distanceKm / fibreKmPerMs
}

// EffectiveBound returns the latency bound that applies to one decision: the
// request's own bound when it has one, otherwise the policy's (possibly nil).
func EffectiveBound(policy, request *autoscalingv1alpha1.LatencyBound) *autoscalingv1alpha1.LatencyBound {
	if request != nil {
		return request
	}
	return policy
}

// withinBound keeps the offers (sites) whose latency from the bound's
// reference point is at most MaxLatencyMs, and reports which providers lost at
// least one site. With a nil bound it returns the offers unchanged.
//
// The reference point is bound.From, or the consumer's own location when From
// is empty. In that second case a measured round-trip time for the provider
// (from the agent's UDP probe) replaces the distance estimate, because it is
// the real path from this consumer.
//
// The filter is strict: a site whose latency cannot be evaluated (no
// coordinates, or no reference location at all) does not qualify.
func withinBound(
	offers []Offer, bound *autoscalingv1alpha1.LatencyBound,
	consumer brokerapi.ConsumerEntry, measuredMs map[string]float64,
) []Offer {
	if bound == nil {
		return offers
	}
	limit := float64(bound.MaxLatencyMs)

	fromConsumer := bound.From == nil
	var refLat, refLon float64
	hasRef := true
	if fromConsumer {
		refLat, refLon, hasRef = consumer.Latitude, consumer.Longitude, consumer.HasLocation
	} else {
		refLat, refLon = bound.From.Latitude, bound.From.Longitude
	}

	kept := make([]Offer, 0, len(offers))
	for _, o := range offers {
		if fromConsumer {
			if rtt, ok := measuredMs[o.ProviderID]; ok {
				if rtt <= limit {
					kept = append(kept, o)
				}
				continue
			}
		}
		lat, lon := o.Location.Latitude, o.Location.Longitude
		if !hasRef || lat == nil || lon == nil {
			continue // cannot evaluate → not within the bound
		}
		if EstimateLatencyMs(brokerapi.HaversineKm(refLat, refLon, *lat, *lon)) <= limit {
			kept = append(kept, o)
		}
	}
	return kept
}

// heldOutsideBound returns the providers this consumer holds chunks on but
// which have no site left after the bound filter. NodeGroups keeps them listed
// with no free chunks, so the Cluster Autoscaler still maps the nodes it
// already has there while it cannot grow them.
func heldOutsideBound(all, kept []Offer, ownReserved map[string]int32) map[string]bool {
	stillListed := map[string]bool{}
	for _, o := range kept {
		stillListed[o.ProviderID] = true
	}
	held := map[string]bool{}
	for _, o := range all {
		if !stillListed[o.ProviderID] && ownReserved[o.ProviderID] > 0 {
			held[o.ProviderID] = true
		}
	}
	return held
}
