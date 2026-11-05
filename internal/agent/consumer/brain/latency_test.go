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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// Bologna is about 250 km from Asti (~2.5 ms estimated round trip).
const bolognaLat, bolognaLon = 44.4949, 11.3426

// boundFromAsti is "max ms from Asti".
func boundFromAsti(ms int32) *autoscalingv1alpha1.LatencyBound {
	return &autoscalingv1alpha1.LatencyBound{
		MaxLatencyMs: ms,
		From:         &autoscalingv1alpha1.GeoPoint{Name: "Asti", Latitude: astiLat, Longitude: astiLon},
	}
}

// boundFromConsumer is "max ms from the consumer's own location".
func boundFromConsumer(ms int32) *autoscalingv1alpha1.LatencyBound {
	return &autoscalingv1alpha1.LatencyBound{MaxLatencyMs: ms}
}

func inTurin() brokerapi.ConsumerEntry {
	return brokerapi.ConsumerEntry{Latitude: turinLat, Longitude: turinLon, HasLocation: true}
}

func TestEstimateLatencyMs(t *testing.T) {
	if got := EstimateLatencyMs(300); got != 3 {
		t.Errorf("300 km = %v ms, want 3", got)
	}
	// Sanity check on the fixtures the cases below rely on.
	if d := brokerapi.HaversineKm(astiLat, astiLon, bolognaLat, bolognaLon); d < 200 || d > 300 {
		t.Fatalf("Asti-Bologna = %.0f km, fixtures assume 200..300", d)
	}
	if d := brokerapi.HaversineKm(astiLat, astiLon, romeLat, romeLon); d < 300 {
		t.Fatalf("Asti-Rome = %.0f km, fixtures assume > 300", d)
	}
}

func TestLatencyBoundFiberCopStory(t *testing.T) {
	// "3 VMs, max 3 ms from Asti": the Piedmont sites qualify, a Rome site does not.
	offers := []Offer{
		located(siteOffer("fibercop", "asti", 12, 24), astiLat, astiLon),
		located(siteOffer("fibercop", "cuneo", 8, 16), cuneoLat, cuneoLon),
		located(siteOffer("fibercop", "alba", 14, 28), albaLat, albaLon),
		located(siteOffer("fibercop", "rome", 10, 20), romeLat, romeLon),
	}
	v := NodeGroups(Input{Offers: offers, LatencyBound: boundFromAsti(3)}).NodeGroups[0]
	if v.MaxSize != 17 {
		t.Errorf("MaxSize = %d, want 17 (Asti 6 + Cuneo 4 + Alba 7, Rome excluded)", v.MaxSize)
	}
	// Without a bound, Rome's 5 chunks count too.
	if all := NodeGroups(Input{Offers: offers}).NodeGroups[0]; all.MaxSize != 22 {
		t.Errorf("unbounded MaxSize = %d, want 22", all.MaxSize)
	}
}

func TestLatencyBoundSizeMatters(t *testing.T) {
	offers := []Offer{
		located(siteOffer("p-alba", "alba", 6, 12), albaLat, albaLon),
		located(siteOffer("p-bologna", "bologna", 6, 12), bolognaLat, bolognaLon),
	}
	tight := headroom(NodeGroups(Input{Offers: offers, LatencyBound: boundFromAsti(2)}))
	if _, listed := tight["p-bologna"]; listed || tight["p-alba"] != 3 {
		t.Errorf("2 ms from Asti: want only p-alba, got %v", tight)
	}
	loose := headroom(NodeGroups(Input{Offers: offers, LatencyBound: boundFromAsti(3)}))
	if _, listed := loose["p-bologna"]; !listed {
		t.Errorf("3 ms from Asti: p-bologna must qualify, got %v", loose)
	}
}

func TestLatencyBoundFromConsumer(t *testing.T) {
	offers := []Offer{
		located(siteOffer("p-asti", "asti", 6, 12), astiLat, astiLon),
		located(siteOffer("p-rome", "rome", 6, 12), romeLat, romeLon),
	}
	hr := headroom(NodeGroups(Input{Offers: offers, Consumer: inTurin(), LatencyBound: boundFromConsumer(3)}))
	if _, listed := hr["p-rome"]; listed || hr["p-asti"] != 3 {
		t.Errorf("3 ms from Turin: want only p-asti, got %v", hr)
	}
}

func TestLatencyBoundMeasuredRTTWins(t *testing.T) {
	// From the consumer's own location, a measured RTT replaces the estimate:
	// a congested near provider is excluded, a well-connected far one qualifies.
	offers := []Offer{
		located(siteOffer("p-asti", "asti", 6, 12), astiLat, astiLon),
		located(siteOffer("p-rome", "rome", 6, 12), romeLat, romeLon),
	}
	hr := headroom(NodeGroups(Input{
		Offers:            offers,
		Consumer:          inTurin(),
		LatencyBound:      boundFromConsumer(3),
		MeasuredLatencyMs: map[string]float64{"p-asti": 9, "p-rome": 2},
	}))
	if _, listed := hr["p-asti"]; listed || hr["p-rome"] != 3 {
		t.Errorf("measured RTTs must win, got %v", hr)
	}
}

func TestLatencyBoundStrictWhenUnknown(t *testing.T) {
	asti := located(siteOffer("p-asti", "asti", 6, 12), astiLat, astiLon)
	unlocated := siteOffer("p-nowhere", "default", 6, 12)

	// No reference location at all: nothing can be shown to be within the bound.
	resp := NodeGroups(Input{Offers: []Offer{asti}, LatencyBound: boundFromConsumer(3)})
	if len(resp.NodeGroups) != 0 {
		t.Errorf("no reference point: want no node groups, got %+v", resp.NodeGroups)
	}

	// A site without coordinates never qualifies under a bound...
	hr := headroom(NodeGroups(Input{
		Offers: []Offer{asti, unlocated}, LatencyBound: boundFromAsti(3),
	}))
	if _, listed := hr["p-nowhere"]; listed {
		t.Errorf("a site without coordinates must not qualify, got %v", hr)
	}
	// ...but is fine with no bound.
	if hr := headroom(NodeGroups(Input{Offers: []Offer{unlocated}})); hr["p-nowhere"] != 3 {
		t.Errorf("without a bound every offer counts, got %v", hr)
	}
}

func TestLatencyBoundKeepsHeldProviderListed(t *testing.T) {
	// This consumer already holds 2 GPU chunks in Rome, now outside the bound.
	rome := located(siteOffer("p-rome", "rome", 8, 16), romeLat, romeLon)
	rome.Available[corev1.ResourceName("nvidia.com/gpu")] = resource.MustParse("2")
	resp := NodeGroups(Input{
		Offers:       []Offer{rome, located(siteOffer("p-asti", "asti", 6, 12), astiLat, astiLon)},
		OwnReserved:  map[string]int32{"p-rome": 2},
		LatencyBound: boundFromAsti(3),
	})
	v, listed := viewsByProvider(resp)["p-rome"]
	if !listed {
		t.Fatal("a provider holding our chunks must stay listed")
	}
	if v.MaxSize != 2 || v.CurrentReserved != 2 {
		t.Errorf("held provider: MaxSize/CurrentReserved = %d/%d, want 2/2 (not growable)", v.MaxSize, v.CurrentReserved)
	}
	if v.Type != brokerv1alpha1.ChunkTypeGPU {
		t.Errorf("held provider must keep its chunk type (and node group ID), got %s", v.Type)
	}
}

func TestLatencyBoundThenPolicyRanks(t *testing.T) {
	// The cheapest provider is outside the bound, so Price picks the cheapest
	// of those inside it.
	offers := []Offer{
		withPrice(located(siteOffer("p-rome", "rome", 6, 12), romeLat, romeLon), "0.01"),
		withPrice(located(siteOffer("p-asti", "asti", 6, 12), astiLat, astiLon), "0.05"),
		withPrice(located(siteOffer("p-alba", "alba", 6, 12), albaLat, albaLon), "0.03"),
	}
	resp := NodeGroups(Input{
		Offers:       offers,
		Consumer:     policy(autoscalingv1alpha1.PlacementStrategyPrice),
		LatencyBound: boundFromAsti(3),
	})
	if got := growable(resp); len(got) != 1 || got[0] != "p-alba" {
		t.Errorf("growable = %v, want [p-alba]", got)
	}
}

func TestEffectiveBound(t *testing.T) {
	policyBound := boundFromConsumer(5)
	requestBound := boundFromAsti(3)
	if EffectiveBound(policyBound, requestBound) != requestBound {
		t.Error("a request's bound must override the policy's")
	}
	if EffectiveBound(policyBound, nil) != policyBound {
		t.Error("without a request bound, the policy's applies")
	}
	if EffectiveBound(nil, nil) != nil {
		t.Error("no bound anywhere means no bound")
	}
}
