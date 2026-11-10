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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/catalog"
)

// Piedmont coordinates for the latency cases. Distances from Turin (the
// consumer): Asti ~48 km, Cuneo ~76 km, Alba ~52 km, Rome ~525 km.
const (
	turinLat, turinLon = 45.0703, 7.6869
	astiLat, astiLon   = 44.9008, 8.2064
	albaLat, albaLon   = 44.7008, 8.0356
	cuneoLat, cuneoLon = 44.3845, 7.5427
	romeLat, romeLon   = 41.9028, 12.4964
)

func policy(t autoscalingv1alpha1.PlacementStrategy) brokerapi.ConsumerEntry {
	return brokerapi.ConsumerEntry{Placement: autoscalingv1alpha1.PlacementPolicy{Type: t}}
}

// headroom maps provider → growable head-room (MaxSize - CurrentReserved).
func headroom(resp brokerapi.NodeGroupListResponse) map[string]int32 {
	out := map[string]int32{}
	for _, v := range resp.NodeGroups {
		out[v.ProviderClusterID] = v.MaxSize - v.CurrentReserved
	}
	return out
}

// growable lists the providers with head-room left, in the response's order.
func growable(resp brokerapi.NodeGroupListResponse) []string {
	var out []string
	for _, v := range resp.NodeGroups {
		if v.MaxSize-v.CurrentReserved > 0 {
			out = append(out, v.ProviderClusterID)
		}
	}
	return out
}

func located(o catalog.Offer, lat, lon float64) catalog.Offer {
	o.Location.Latitude, o.Location.Longitude = &lat, &lon
	return o
}

func withCarbon(o catalog.Offer, gco2 float64) catalog.Offer {
	o.CarbonIntensity = &gco2
	return o
}

func withPrice(o catalog.Offer, cpu string) catalog.Offer {
	o.UnitPrices = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse("0.001"),
	}
	return o
}

func TestPolicyStandard(t *testing.T) {
	t.Run("equal free room: the renewable provider wins", func(t *testing.T) {
		green := siteOffer("p-green", "default", 6, 12)
		green.Renewable = true
		resp := NodeGroups(Input{Offers: []catalog.Offer{siteOffer("p-a", "default", 6, 12), green}})
		if got := growable(resp); len(got) != 1 || got[0] != "p-green" {
			t.Errorf("growable = %v, want [p-green]", got)
		}
		if resp.AppliedPlacement != "" {
			t.Errorf("Standard is the empty policy, got %q", resp.AppliedPlacement)
		}
	})
	t.Run("more free room wins", func(t *testing.T) {
		// Both have 3 free chunks; this consumer already holds 3 on p-busy, so
		// p-busy is only half free while p-idle is fully free.
		resp := NodeGroups(Input{
			Offers:      []catalog.Offer{siteOffer("p-busy", "default", 6, 12), siteOffer("p-idle", "default", 6, 12)},
			OwnReserved: map[string]int32{"p-busy": 3},
		})
		if got := growable(resp); len(got) != 1 || got[0] != "p-idle" {
			t.Errorf("growable = %v, want [p-idle]", got)
		}
	})
}

func TestPolicyEco(t *testing.T) {
	resp := NodeGroups(Input{
		Offers: []catalog.Offer{
			withCarbon(siteOffer("p-coal", "default", 6, 12), 600),
			withCarbon(siteOffer("p-hydro", "default", 6, 12), 30),
			withCarbon(siteOffer("p-gas", "default", 6, 12), 350),
		},
		Consumer: policy(autoscalingv1alpha1.PlacementStrategyEco),
	})
	if got := growable(resp); len(got) != 1 || got[0] != "p-hydro" {
		t.Errorf("growable = %v, want [p-hydro]", got)
	}
	for _, v := range resp.NodeGroups {
		if !v.HasMetric || v.PlacementMetric != *v.CarbonIntensity {
			t.Errorf("%s: metric = %v/%v, want its carbon %v", v.ProviderClusterID, v.HasMetric, v.PlacementMetric, *v.CarbonIntensity)
		}
	}
}

func TestPolicyLatency(t *testing.T) {
	entry := policy(autoscalingv1alpha1.PlacementStrategyLatency)
	entry.Latitude, entry.Longitude, entry.HasLocation = turinLat, turinLon, true
	resp := NodeGroups(Input{
		Offers: []catalog.Offer{
			located(siteOffer("p-rome", "rome", 6, 12), romeLat, romeLon),
			located(siteOffer("p-asti", "asti", 6, 12), astiLat, astiLon),
			located(siteOffer("p-cuneo", "cuneo", 6, 12), cuneoLat, cuneoLon),
			located(siteOffer("p-alba", "alba", 6, 12), albaLat, albaLon),
		},
		Consumer: entry,
	})
	if !resp.LatencyShortlist {
		t.Error("a located consumer must get a latency shortlist")
	}
	hr := headroom(resp)
	for _, near := range []string{"p-asti", "p-alba", "p-cuneo"} {
		if hr[near] != 3 {
			t.Errorf("%s is among the 3 nearest and must stay growable, head-room = %d", near, hr[near])
		}
	}
	if hr["p-rome"] != 0 {
		t.Errorf("p-rome is the farthest and must be masked, head-room = %d", hr["p-rome"])
	}
}

func TestPolicyRandom(t *testing.T) {
	offers := []catalog.Offer{
		siteOffer("p1", "default", 6, 12), siteOffer("p2", "default", 6, 12), siteOffer("p3", "default", 6, 12),
	}
	for range 20 {
		resp := NodeGroups(Input{Offers: offers, Consumer: policy(autoscalingv1alpha1.PlacementStrategyRandom)})
		if got := growable(resp); len(got) != 1 {
			t.Fatalf("Random must leave exactly one provider growable, got %v", got)
		}
	}
}

func TestPolicyConsumerChoice(t *testing.T) {
	resp := NodeGroups(Input{
		Offers: []catalog.Offer{
			withPrice(siteOffer("p1", "default", 6, 12), "0.01"),
			withPrice(siteOffer("p2", "default", 6, 12), "0.09"),
		},
		Consumer: policy(autoscalingv1alpha1.PlacementStrategyConsumerChoice),
	})
	if got := growable(resp); len(got) != 2 {
		t.Errorf("ConsumerChoice must mask nothing, growable = %v", got)
	}
	if resp.AppliedPlacement != autoscalingv1alpha1.PlacementStrategyConsumerChoice {
		t.Errorf("applied placement = %q", resp.AppliedPlacement)
	}
}

// TestBrainMatchesBroker checks that, for a single consumer, the brain's answer
// built from catalog offers equals the Broker's answer built from the
// equivalent ClusterAdvertisements, for every deterministic policy.
func TestBrainMatchesBroker(t *testing.T) {
	type provider struct {
		name, city    string
		lat, lon      float64
		cpu, memGi    int64 // free capacity
		own           int32 // this consumer's chunks there
		price         string
		carbon        float64
		renewable     bool
		free, totalCh int32 // chunk counts the Broker would hold
	}
	providers := []provider{
		{"p-asti", "asti", astiLat, astiLon, 12, 24, 1, "0.03", 120, false, 6, 7},
		{"p-alba", "alba", albaLat, albaLon, 8, 16, 0, "0.02", 300, true, 4, 4},
		{"p-rome", "rome", romeLat, romeLon, 4, 8, 2, "0.01", 50, false, 2, 4},
	}

	offers := make([]catalog.Offer, 0, len(providers))
	advs := make([]*brokerv1alpha1.ClusterAdvertisement, 0, len(providers))
	own := map[string]int32{}
	for _, p := range providers {
		o := withCarbon(withPrice(located(siteOffer(p.name, p.city, p.cpu, p.memGi), p.lat, p.lon), p.price), p.carbon)
		o.Renewable = p.renewable
		offers = append(offers, o)
		own[p.name] = p.own

		carbon := p.carbon
		advs = append(advs, &brokerv1alpha1.ClusterAdvertisement{
			ObjectMeta: metav1.ObjectMeta{Name: p.name},
			Spec: brokerv1alpha1.ClusterAdvertisementSpec{
				ClusterID:       p.name,
				LiqoClusterID:   "liqo-" + p.name,
				ClusterType:     brokerv1alpha1.ChunkTypeStandard,
				Resources:       brokerv1alpha1.AdvertisedResources{Allocatable: o.Available},
				Topology:        &brokerv1alpha1.Topology{City: p.city, Latitude: p.lat, Longitude: p.lon},
				UnitPrices:      o.UnitPrices,
				CarbonIntensity: &carbon,
				Renewable:       p.renewable,
			},
			Status: brokerv1alpha1.ClusterAdvertisementStatus{
				Available: true, TotalChunks: p.totalCh, ReservedChunks: p.own, AvailableChunks: p.free,
			},
		})
	}

	inflight := map[string]bool{"p-asti": true}
	for _, strategy := range []autoscalingv1alpha1.PlacementStrategy{
		"", // Standard
		autoscalingv1alpha1.PlacementStrategyStandard,
		autoscalingv1alpha1.PlacementStrategyPrice,
		autoscalingv1alpha1.PlacementStrategyEco,
		autoscalingv1alpha1.PlacementStrategyLatency,
		autoscalingv1alpha1.PlacementStrategyConsumerChoice,
	} {
		entry := policy(strategy)
		entry.Latitude, entry.Longitude, entry.HasLocation = turinLat, turinLon, true

		fromBrain := NodeGroups(Input{Offers: offers, OwnReserved: own, InFlight: inflight, Consumer: entry})
		fromBroker := brokerapi.BuildNodeGroups(brokerapi.PlacementInput{
			Advertisements: advs, Consumer: entry, InFlight: inflight,
		})
		fromBrain.ServedAt, fromBroker.ServedAt = metav1.Time{}, metav1.Time{}
		if !reflect.DeepEqual(fromBrain, fromBroker) {
			t.Errorf("policy %q: brain and broker differ\nbrain:  %+v\nbroker: %+v", strategy, fromBrain, fromBroker)
		}
	}
}
