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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// memBytesPerGiB is the divisor that turns a memory Quantity (bytes) into the
// GiB unit the provider prices memory in.
const memBytesPerGiB = 1024 * 1024 * 1024

// perChunkCost computes the per-chunk hourly cost of cadv from its per-resource
// unit prices (Spec.UnitPrices) and the Broker-owned chunk size. Because chunk
// size belongs to the Broker (not the provider), providers price their own
// resources and the Broker converts: cost = Σ unitPrice[r] × chunkUnits[r].
//
// It returns (cost, true) only when the advertisement is FULLY priced — every
// resource key present in the chunk has a non-negative unit price. A provider
// with no UnitPrices, a missing key, or a negative price returns (0, false):
// computing a cost from partial prices would understate it and let a
// misconfigured provider masquerade as the cheapest, so callers treat such
// providers as a last resort rather than ranking them.
func (s *Server) perChunkCost(cadv *brokerv1alpha1.ClusterAdvertisement) (float64, bool) {
	prices := cadv.Spec.UnitPrices
	if len(prices) == 0 {
		return 0, false
	}
	chunk := s.perChunkResources(cadv)
	if len(chunk) == 0 {
		return 0, false
	}

	var total float64
	for name, qty := range chunk {
		price, ok := prices[name]
		if !ok || price.Sign() < 0 {
			return 0, false // missing or negative price → not (fully) priced
		}
		total += price.AsApproximateFloat64() * resourceUnits(name, qty)
	}
	return total, true
}

// resourceUnits converts a chunk's quantity of `name` into the unit the
// provider prices it in: cpu → cores, memory → GiB, and every other resource
// (e.g. nvidia.com/gpu) → its whole-unit count. The conventional
// AsApproximateFloat64 already yields cores and whole counts; only memory needs
// the bytes→GiB scaling.
func resourceUnits(name corev1.ResourceName, qty resource.Quantity) float64 {
	if name == corev1.ResourceMemory {
		return qty.AsApproximateFloat64() / memBytesPerGiB
	}
	return qty.AsApproximateFloat64()
}

// costQuantity renders a per-chunk cost as a *resource.Quantity for the wire
// NodeGroupView.Cost field (kept for the dashboard and the gRPC server's
// PricingNodePrice back-compat). Returns nil on the unpriced path.
func costQuantity(cost float64, priced bool) *resource.Quantity {
	if !priced {
		return nil
	}
	q := resource.NewMilliQuantity(int64(cost*1000), resource.DecimalSI)
	return q
}
