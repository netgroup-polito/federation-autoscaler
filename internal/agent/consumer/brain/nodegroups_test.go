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
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/catalog"
)

// siteOffer builds an offer with cpu cores and memory GiB free at one site.
func siteOffer(provider, site string, cpu, memGi int64) catalog.Offer {
	return catalog.Offer{
		SchemaVersion: 1,
		ProviderID:    provider,
		LiqoClusterID: "liqo-" + provider,
		SiteID:        site,
		Location:      catalog.Location{City: site},
		Available: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(cpu, resource.DecimalSI),
			corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dGi", memGi)),
		},
	}
}

func viewsByProvider(resp brokerapi.NodeGroupListResponse) map[string]brokerapi.NodeGroupView {
	out := map[string]brokerapi.NodeGroupView{}
	for _, v := range resp.NodeGroups {
		out[v.ProviderClusterID] = v
	}
	return out
}

func TestNodeGroupsSingleSite(t *testing.T) {
	// 12 cpu / 24 Gi = six standard 2 cpu / 4 Gi chunks.
	resp := NodeGroups(Input{Offers: []catalog.Offer{siteOffer("p1", "default", 12, 24)}})
	if len(resp.NodeGroups) != 1 {
		t.Fatalf("want 1 node group, got %+v", resp.NodeGroups)
	}
	v := resp.NodeGroups[0]
	if v.ProviderClusterID != "p1" || v.ProviderLiqoClusterID != "liqo-p1" || v.Type != brokerv1alpha1.ChunkTypeStandard {
		t.Errorf("identity: %+v", v)
	}
	if v.MaxSize != 6 || v.CurrentReserved != 0 {
		t.Errorf("MaxSize/CurrentReserved = %d/%d, want 6/0", v.MaxSize, v.CurrentReserved)
	}
	if cpu := v.ChunkResources[corev1.ResourceCPU]; cpu.Value() != 2 {
		t.Errorf("chunk cpu = %s, want 2", cpu.String())
	}
}

func TestNodeGroupsPoolsSitesPerProvider(t *testing.T) {
	// An edge provider with three sites: Asti 6, Cuneo 4, Alba 7 chunks free.
	offers := []catalog.Offer{
		siteOffer("edge-telco", "asti", 12, 24),
		siteOffer("edge-telco", "cuneo", 8, 16),
		siteOffer("edge-telco", "alba", 14, 28),
	}
	resp := NodeGroups(Input{Offers: offers})
	if len(resp.NodeGroups) != 1 {
		t.Fatalf("sites must pool into one node group per provider, got %d", len(resp.NodeGroups))
	}
	if v := resp.NodeGroups[0]; v.MaxSize != 17 {
		t.Errorf("pooled MaxSize = %d, want 17 (6+4+7)", v.MaxSize)
	}
	// Location comes from the lead site: the one with the most free chunks.
	if v := resp.NodeGroups[0]; v.Topology == nil || v.Topology.City != "alba" {
		t.Errorf("lead site should be alba, topology = %+v", v.Topology)
	}
}

func TestNodeGroupsLeftoversDoNotAddUp(t *testing.T) {
	// Each site has half a chunk free. Pooled it would look like one chunk, but
	// no single site can host it, so the provider must not be growable.
	offers := []catalog.Offer{siteOffer("p1", "a", 1, 2), siteOffer("p1", "b", 1, 2)}
	v := NodeGroups(Input{Offers: offers}).NodeGroups[0]
	if v.MaxSize != 0 {
		t.Errorf("MaxSize = %d, want 0 (leftovers across sites are not a chunk)", v.MaxSize)
	}
}

func TestNodeGroupsOwnReservations(t *testing.T) {
	// 3 free chunks in the catalog, and this consumer already holds 2 there.
	resp := NodeGroups(Input{
		Offers:      []catalog.Offer{siteOffer("p1", "default", 6, 12)},
		OwnReserved: map[string]int32{"p1": 2, "elsewhere": 5},
	})
	v := resp.NodeGroups[0]
	if v.CurrentReserved != 2 || v.MaxSize != 5 {
		t.Errorf("CurrentReserved/MaxSize = %d/%d, want 2/5 (own, own + free)", v.CurrentReserved, v.MaxSize)
	}
	if len(resp.NodeGroups) != 1 {
		t.Errorf("a provider without a live offer must not appear, got %d groups", len(resp.NodeGroups))
	}
}

func TestNodeGroupsCostPerChunk(t *testing.T) {
	o := siteOffer("p1", "default", 6, 12)
	o.UnitPrices = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("0.03"),  // per core-hour
		corev1.ResourceMemory: resource.MustParse("0.004"), // per GiB-hour
	}
	v := NodeGroups(Input{Offers: []catalog.Offer{o}}).NodeGroups[0]
	// One chunk = 2 cpu + 4 GiB → 2*0.03 + 4*0.004 = 0.076 per hour.
	if v.Cost == nil || v.Cost.MilliValue() != 76 {
		t.Errorf("cost per chunk = %v, want 0.076", v.Cost)
	}
}

func TestNodeGroupsGPUSite(t *testing.T) {
	o := siteOffer("p1", "default", 8, 16)
	o.Available[corev1.ResourceName("nvidia.com/gpu")] = resource.MustParse("2")
	v := NodeGroups(Input{Offers: []catalog.Offer{o}}).NodeGroups[0]
	if v.Type != brokerv1alpha1.ChunkTypeGPU || v.MaxSize != 2 {
		t.Errorf("type/MaxSize = %s/%d, want gpu/2", v.Type, v.MaxSize)
	}
}

func TestNodeGroupsPricePolicyEndToEnd(t *testing.T) {
	cheap := siteOffer("p-cheap", "default", 6, 12)
	cheap.UnitPrices = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("0.01"), corev1.ResourceMemory: resource.MustParse("0.001"),
	}
	dear := siteOffer("p-dear", "default", 6, 12)
	dear.UnitPrices = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("0.09"), corev1.ResourceMemory: resource.MustParse("0.009"),
	}
	resp := NodeGroups(Input{
		Offers:   []catalog.Offer{dear, cheap},
		Consumer: brokerapi.ConsumerEntry{Placement: autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyPrice}},
	})
	if resp.AppliedPlacement != autoscalingv1alpha1.PlacementStrategyPrice {
		t.Errorf("applied placement = %q, want Price", resp.AppliedPlacement)
	}
	views := viewsByProvider(resp)
	if hr := views["p-cheap"].MaxSize - views["p-cheap"].CurrentReserved; hr != 3 {
		t.Errorf("cheapest must be growable, head-room = %d", hr)
	}
	if hr := views["p-dear"].MaxSize - views["p-dear"].CurrentReserved; hr != 0 {
		t.Errorf("dearer must be masked, head-room = %d", hr)
	}
	if resp.NodeGroups[0].ProviderClusterID != "p-cheap" {
		t.Errorf("result must be sorted by provider, got %s first", resp.NodeGroups[0].ProviderClusterID)
	}
}

func TestNodeGroupsNoOffers(t *testing.T) {
	resp := NodeGroups(Input{})
	if len(resp.NodeGroups) != 0 {
		t.Errorf("no offers must give no node groups, got %+v", resp.NodeGroups)
	}
}
