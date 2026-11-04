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

// Package brain is the consumer-side placement decision (local mode): it reads
// the provider offers from the catalog and builds the node-group view the
// Cluster Autoscaler grows, with the same policies the Broker applies.
package brain

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/broker/chunk"
)

// Input is everything one local placement decision needs.
type Input struct {
	// Offers are the live catalog offers (every provider, every site).
	Offers []Offer

	// OwnReserved[provider] is how many chunks THIS consumer currently holds on
	// that provider. It becomes the node group's CurrentReserved, so the
	// Cluster Autoscaler sees only its own nodes, never other consumers'.
	OwnReserved map[string]int32

	// InFlight[provider] is true while this consumer has a reservation on that
	// provider still being provisioned (the no-premature-spill gate).
	InFlight map[string]bool

	// Consumer carries this consumer's placement policy and location.
	Consumer brokerapi.ConsumerEntry

	// Sizer turns capacity into chunks. Nil means the default sizer, the same
	// one the Broker uses.
	Sizer chunk.Sizer
}

// NodeGroups builds the node-group view from the catalog offers. It converts
// the offers into one in-memory advertisement per provider (see
// Advertisements) and runs the shared placement core, so every policy behaves
// exactly as it does on the Broker.
func NodeGroups(in Input) brokerapi.NodeGroupListResponse {
	sizer := in.Sizer
	if sizer == nil {
		sizer = chunk.NewDefaultSizer()
	}
	return brokerapi.BuildNodeGroups(brokerapi.PlacementInput{
		Advertisements: Advertisements(in.Offers, in.OwnReserved, sizer),
		Consumer:       in.Consumer,
		InFlight:       in.InFlight,
		Sizer:          sizer,
	})
}

// Advertisements converts catalog offers into one in-memory
// ClusterAdvertisement per provider, the shape the placement core expects.
//
// In this phase booking goes through the Broker per provider cluster, so a
// provider's sites are pooled into a single node group:
//   - free chunks are counted per site and then summed, so leftovers at
//     different sites never add up to a chunk no single site could host;
//   - only sites whose chunk type matches the provider's pooled type count;
//   - location, prices, carbon and the probe endpoint come from the
//     provider's "lead" site: the one with the most free chunks (lowest site
//     ID on a tie), which is where a new chunk most likely lands.
//
// Status follows local-mode semantics: ReservedChunks is this consumer's own
// count and TotalChunks is own + free, so MaxSize = own + free and
// CurrentReserved = own in the resulting view. Providers without a live offer
// are absent, like unavailable providers on the Broker.
func Advertisements(
	offers []Offer, ownReserved map[string]int32, sizer chunk.Sizer,
) []*brokerv1alpha1.ClusterAdvertisement {
	if sizer == nil {
		sizer = chunk.NewDefaultSizer()
	}

	byProvider := map[string][]Offer{}
	for _, o := range offers {
		if o.ProviderID == "" {
			continue
		}
		byProvider[o.ProviderID] = append(byProvider[o.ProviderID], o)
	}

	providers := make([]string, 0, len(byProvider))
	for p := range byProvider {
		providers = append(providers, p)
	}
	sort.Strings(providers)

	out := make([]*brokerv1alpha1.ClusterAdvertisement, 0, len(providers))
	for _, p := range providers {
		out = append(out, providerAdvertisement(p, byProvider[p], ownReserved[p], sizer))
	}
	return out
}

// providerAdvertisement pools one provider's sites into a ClusterAdvertisement.
func providerAdvertisement(
	provider string, sites []Offer, own int32, sizer chunk.Sizer,
) *brokerv1alpha1.ClusterAdvertisement {
	sort.Slice(sites, func(i, j int) bool { return sites[i].SiteID < sites[j].SiteID })

	pooled := corev1.ResourceList{}
	for _, s := range sites {
		addResources(pooled, s.Available)
	}
	chunkType, _ := sizedChunks(sizer, pooled)

	var free int32
	lead, leadChunks := 0, int32(-1)
	for i, s := range sites {
		t, n := sizedChunks(sizer, s.Available)
		if t != chunkType {
			n = 0
		}
		free += n
		if n > leadChunks {
			lead, leadChunks = i, n
		}
	}
	ref := sites[lead]

	return &brokerv1alpha1.ClusterAdvertisement{
		ObjectMeta: metav1.ObjectMeta{Name: provider},
		Spec: brokerv1alpha1.ClusterAdvertisementSpec{
			ClusterID:       provider,
			LiqoClusterID:   ref.LiqoClusterID,
			ClusterType:     chunkType,
			Resources:       brokerv1alpha1.AdvertisedResources{Allocatable: pooled},
			Topology:        topologyFrom(ref.Location),
			UnitPrices:      ref.UnitPrices,
			CarbonIntensity: ref.CarbonIntensity,
			CarbonForecast:  ref.CarbonForecast,
			Renewable:       ref.Renewable,
			ProbeEndpoint:   ref.ProbeEndpoint,
		},
		Status: brokerv1alpha1.ClusterAdvertisementStatus{
			Available:       true,
			TotalChunks:     own + free,
			ReservedChunks:  own,
			AvailableChunks: free,
		},
	}
}

// sizedChunks returns the chunk type and count the sizer derives from capacity.
// The default sizer always returns one group, so a site with no free capacity
// still yields its type with a count of 0.
func sizedChunks(sizer chunk.Sizer, capacity corev1.ResourceList) (brokerv1alpha1.ChunkType, int32) {
	results := sizer.Size(capacity)
	if len(results) == 0 {
		return brokerv1alpha1.ChunkTypeStandard, 0
	}
	return results[0].Type, results[0].Count
}

// addResources adds every quantity in add into sum, in place.
func addResources(sum, add corev1.ResourceList) {
	for name, qty := range add {
		cur, ok := sum[name]
		if !ok {
			cur = resource.Quantity{}
		}
		cur.Add(qty)
		sum[name] = cur
	}
}

// topologyFrom maps an offer's location onto the advertisement topology the
// latency policy reads. Missing coordinates stay 0, which the placement core
// already treats as "no location".
func topologyFrom(l Location) *brokerv1alpha1.Topology {
	t := &brokerv1alpha1.Topology{Region: l.Region, City: l.City}
	if l.Latitude != nil {
		t.Latitude = *l.Latitude
	}
	if l.Longitude != nil {
		t.Longitude = *l.Longitude
	}
	return t
}
