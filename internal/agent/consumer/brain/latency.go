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
	"math"

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

// withinBound keeps the offers (sites) whose round-trip latency to this
// consumer is at most MaxLatencyMs. With a nil bound it returns the offers
// unchanged.
//
// A site's latency is the round-trip time measured to its probe endpoint
// (measuredMs, keyed by endpoint) when there is a finite measurement. Otherwise
// it falls back to the distance estimate between the consumer's location and
// the site's, and the site is reported in estimated ("provider/site") so the
// caller can log that the decision rests on an estimate.
//
// The filter is strict: a site with neither a measurement nor coordinates (or
// a consumer with no known location) does not qualify.
func withinBound(
	offers []Offer, bound *autoscalingv1alpha1.LatencyBound,
	consumer brokerapi.ConsumerEntry, measuredMs map[string]float64,
) (kept []Offer, estimated []string) {
	if bound == nil {
		return offers, nil
	}
	limit := float64(bound.MaxLatencyMs)

	kept = make([]Offer, 0, len(offers))
	for _, o := range offers {
		if rtt, ok := measuredMs[o.ProbeEndpoint]; ok && o.ProbeEndpoint != "" && isFinite(rtt) {
			if rtt <= limit {
				kept = append(kept, o)
			}
			continue
		}
		lat, lon := o.Location.Latitude, o.Location.Longitude
		if !consumer.HasLocation || lat == nil || lon == nil {
			continue // cannot evaluate → not within the bound
		}
		estimated = append(estimated, o.ProviderID+"/"+o.SiteID)
		km := brokerapi.HaversineKm(consumer.Latitude, consumer.Longitude, *lat, *lon)
		if EstimateLatencyMs(km) <= limit {
			kept = append(kept, o)
		}
	}
	return kept, estimated
}

// probeEndpoints returns the distinct, non-empty probe endpoints of offers.
func probeEndpoints(offers []Offer) []string {
	seen := map[string]bool{}
	var out []string
	for _, o := range offers {
		if o.ProbeEndpoint != "" && !seen[o.ProbeEndpoint] {
			seen[o.ProbeEndpoint] = true
			out = append(out, o.ProbeEndpoint)
		}
	}
	return out
}

func isFinite(v float64) bool { return !math.IsInf(v, 0) && !math.IsNaN(v) }

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
