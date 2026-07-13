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
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// stdAdvChunks is the per-provider chunk count every test advertisement is
// sized for (DefaultSizer yields one 2 cpu / 4 GiB standard chunk per slot).
const stdAdvChunks int32 = 3

// stdAdv builds an Available standard-type ClusterAdvertisement sized for
// stdAdvChunks chunks, with `reserved` of them already taken and the given
// (optional) per-resource unit prices.
func stdAdv(name string, reserved int32, prices corev1.ResourceList) *brokerv1alpha1.ClusterAdvertisement {
	total := stdAdvChunks
	return &brokerv1alpha1.ClusterAdvertisement{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: dashboardTestNS},
		Spec: brokerv1alpha1.ClusterAdvertisementSpec{
			ClusterID:     name,
			LiqoClusterID: "liqo-" + name,
			ClusterType:   brokerv1alpha1.ChunkTypeStandard,
			Resources: brokerv1alpha1.AdvertisedResources{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    *resource.NewQuantity(int64(2*total), resource.DecimalSI),
					corev1.ResourceMemory: *resource.NewQuantity(int64(total)*4*1024*1024*1024, resource.BinarySI),
				},
			},
			UnitPrices: prices,
		},
		Status: brokerv1alpha1.ClusterAdvertisementStatus{
			Available: true, TotalChunks: total, ReservedChunks: reserved, AvailableChunks: total - reserved,
		},
	}
}

// cpuMemPrices builds a unit-price list with the given cpu price (per core-hour)
// and a fixed memory price (per GiB-hour) — enough to make a provider "priced".
func cpuMemPrices(cpu string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse("0.001"),
	}
}

func TestPerChunkCost(t *testing.T) {
	s := newDashboardTestServer(t)

	// standard chunk = 2 cpu + 4 GiB; memory price is fixed at 0.001/GiB-hr.
	t.Run("standard fully priced", func(t *testing.T) {
		cost, priced := s.perChunkCost(stdAdv("p", 0, cpuMemPrices("0.01")))
		if !priced {
			t.Fatalf("want priced=true")
		}
		if want := 2*0.01 + 4*0.001; math.Abs(cost-want) > 1e-9 {
			t.Errorf("cost = %v, want %v", cost, want)
		}
	})

	t.Run("no prices is unpriced", func(t *testing.T) {
		if _, priced := s.perChunkCost(stdAdv("p", 0, nil)); priced {
			t.Errorf("want priced=false for an advertisement with no UnitPrices")
		}
	})

	t.Run("missing a chunk resource price is unpriced", func(t *testing.T) {
		only := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0.01")}
		if _, priced := s.perChunkCost(stdAdv("p", 0, only)); priced {
			t.Errorf("partial pricing (memory missing) must yield priced=false")
		}
	})

	t.Run("negative price is unpriced", func(t *testing.T) {
		if _, priced := s.perChunkCost(stdAdv("p", 0, cpuMemPrices("-0.01"))); priced {
			t.Errorf("negative unit price must yield priced=false")
		}
	})

	t.Run("gpu chunk prices cpu, memory and gpu", func(t *testing.T) {
		gpu := &brokerv1alpha1.ClusterAdvertisement{
			ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: dashboardTestNS},
			Spec: brokerv1alpha1.ClusterAdvertisementSpec{
				ClusterID: "g", LiqoClusterID: "liqo-g", ClusterType: brokerv1alpha1.ChunkTypeGPU,
				Resources: brokerv1alpha1.AdvertisedResources{
					Allocatable: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(8*1024*1024*1024, resource.BinarySI),
						"nvidia.com/gpu":      *resource.NewQuantity(1, resource.DecimalSI),
					},
				},
				UnitPrices: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("0.01"),
					corev1.ResourceMemory: resource.MustParse("0.001"),
					"nvidia.com/gpu":      resource.MustParse("2.0"),
				},
			},
		}
		cost, priced := s.perChunkCost(gpu)
		if !priced {
			t.Fatalf("want priced=true")
		}
		if want := 4*0.01 + 8*0.001 + 1*2.0; math.Abs(cost-want) > 1e-9 {
			t.Errorf("gpu cost = %v, want %v", cost, want)
		}
	})
}

// callNodeGroups drives handleNodeGroupsList directly as consumerCluster, with
// that identity threaded onto the request context (mirroring ClusterIDMiddleware).
func callNodeGroups(t *testing.T, s *Server) NodeGroupListResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodegroups", nil)
	req = req.WithContext(NewContextWithClusterID(req.Context(), consumerCluster))
	rec := httptest.NewRecorder()
	s.handleNodeGroupsList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out NodeGroupListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// headroomByProvider maps providerClusterID → growable head-room (MaxSize -
// CurrentReserved), the knob CA uses to decide if a node group can grow.
func headroomByProvider(resp NodeGroupListResponse) map[string]int32 {
	out := map[string]int32{}
	for _, v := range resp.NodeGroups {
		out[v.ProviderClusterID] = v.MaxSize - v.CurrentReserved
	}
	return out
}

// withPricePolicy records a Price-preferring heartbeat for consumerCluster.
func withPricePolicy(s *Server) {
	s.consumers.Touch(consumerCluster, "liqo-c",
		autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyPrice},
		"", "", nil, nil)
}

// TestNodeGroupsPricePreference asserts the strictly-additive 4-case matrix
// (Context section of the plan): masking only ever happens in case 4.
func TestNodeGroupsPricePreference(t *testing.T) {
	cheapPrices := cpuMemPrices("0.01") // per-chunk cost 0.024
	dearPrices := cpuMemPrices("0.05")  // per-chunk cost 0.104

	t.Run("case 1: no policy → Standard composite grows the most-free provider", func(t *testing.T) {
		// No Touch → no ConsumerPolicy → the Standard composite default, which
		// narrows by remaining free capacity (p-free is more free than p-full).
		s := newDashboardTestServer(t, stdAdv("p-full", 2, nil), stdAdv("p-free", 0, nil))
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-free"] == 0 {
			t.Errorf("Standard default must grow the most-free provider; got %+v", hr)
		}
		if hr["p-full"] != 0 {
			t.Errorf("less-free provider must be masked by the composite; got %+v", hr)
		}
	})

	t.Run("case 2: no policy → prices are inert; composite (capacity) drives the choice", func(t *testing.T) {
		// p-cheap is cheaper but LESS free; without a Price policy the composite
		// ignores price and grows the more-free (dearer) provider.
		s := newDashboardTestServer(t, stdAdv("p-cheap", 2, cheapPrices), stdAdv("p-dear", 0, dearPrices))
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-dear"] == 0 {
			t.Errorf("without a Price policy price must be inert; composite grows the most-free provider; got %+v", hr)
		}
		if hr["p-cheap"] != 0 {
			t.Errorf("cheaper-but-less-free provider must be masked; got %+v", hr)
		}
	})

	t.Run("case 3: policy set, no prices → all exposed (no narrowing)", func(t *testing.T) {
		s := newDashboardTestServer(t, stdAdv("p-a", 0, nil), stdAdv("p-b", 0, nil))
		withPricePolicy(s)
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-a"] != 3 || hr["p-b"] != 3 {
			t.Errorf("no priced provider ⇒ no narrowing; got %+v", hr)
		}
	})

	t.Run("case 4: policy + prices → only cheapest priced grows; unpriced is last resort", func(t *testing.T) {
		s := newDashboardTestServer(t,
			stdAdv("p-cheap", 0, cheapPrices),
			stdAdv("p-dear", 0, dearPrices),
			stdAdv("p-unpriced", 0, nil))
		withPricePolicy(s)
		resp := callNodeGroups(t, s)
		hr := headroomByProvider(resp)
		if hr["p-cheap"] != 3 {
			t.Errorf("cheapest priced provider must keep head-room; got %+v", hr)
		}
		if hr["p-dear"] != 0 || hr["p-unpriced"] != 0 {
			t.Errorf("dearer and unpriced providers must be masked; got %+v", hr)
		}
		// Cost is surfaced for priced providers and nil for unpriced ones.
		for _, v := range resp.NodeGroups {
			switch v.ProviderClusterID {
			case "p-cheap":
				if v.Cost == nil || math.Abs(v.Cost.AsApproximateFloat64()-0.024) > 1e-6 {
					t.Errorf("p-cheap Cost = %v, want ≈0.024", v.Cost)
				}
			case "p-unpriced":
				if v.Cost != nil {
					t.Errorf("p-unpriced Cost should be nil; got %v", v.Cost)
				}
			}
		}
	})

	t.Run("case 4 greedy spill: cheapest full → next-cheapest grows", func(t *testing.T) {
		s := newDashboardTestServer(t,
			stdAdv("p-cheap", 3, cheapPrices), // fully reserved → no capacity
			stdAdv("p-dear", 0, dearPrices))
		withPricePolicy(s)
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-cheap"] != 0 {
			t.Errorf("exhausted cheapest must have no head-room; got %+v", hr)
		}
		if hr["p-dear"] != 3 {
			t.Errorf("next-cheapest must be promoted to growable; got %+v", hr)
		}
	})

	t.Run("case 4 in-flight: cheapest full but still peering → hold CA, do NOT spill", func(t *testing.T) {
		// The cheapest provider is full because its one chunk is reserved, but
		// that reservation is still peering (node not ready). The broker must
		// keep CA waiting — neither provider growable — instead of exposing the
		// dearer one (which CA would peer and then have to unpeer).
		peering := &brokerv1alpha1.Reservation{
			ObjectMeta: metav1.ObjectMeta{Name: "r-inflight", Namespace: dashboardTestNS},
			Spec: brokerv1alpha1.ReservationSpec{
				ConsumerClusterID: consumerCluster,
				ProviderClusterID: "p-cheap",
				ChunkCount:        1,
				ChunkType:         brokerv1alpha1.ChunkTypeStandard,
			},
			Status: brokerv1alpha1.ReservationStatus{Phase: brokerv1alpha1.ReservationPhasePeering},
		}
		s := newDashboardTestServer(t,
			stdAdv("p-cheap", 3, cheapPrices), // full (reserved == total)
			stdAdv("p-dear", 0, dearPrices),   // has capacity
			peering)
		withPricePolicy(s)
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-cheap"] != 0 {
			t.Errorf("full cheapest must have no head-room; got %+v", hr)
		}
		if hr["p-dear"] != 0 {
			t.Errorf("dearer provider must NOT be promoted while cheapest is mid-peering; got %+v", hr)
		}
	})
}

// TestDashboardOverview_PricingFields asserts the dashboard projection surfaces
// the two things the demo shows: each provider's per-chunk cost and each
// consumer's placement policy.
func TestDashboardOverview_PricingFields(t *testing.T) {
	s := newDashboardTestServer(t, stdAdv("p-cheap", 0, cpuMemPrices("0.01")))
	withPricePolicy(s) // records consumerCluster with type=Price

	ov, err := s.buildOverview(context.Background())
	if err != nil {
		t.Fatalf("buildOverview: %v", err)
	}

	if len(ov.Advertisements) != 1 || ov.Advertisements[0].CostPerChunk == nil {
		t.Fatalf("priced provider must surface costPerChunk; got %+v", ov.Advertisements)
	}
	if got, want := *ov.Advertisements[0].CostPerChunk, 2*0.01+4*0.001; math.Abs(got-want) > 1e-9 {
		t.Errorf("costPerChunk = %v, want %v", got, want)
	}
	if len(ov.Consumers) != 1 || ov.Consumers[0].Placement != string(autoscalingv1alpha1.PlacementStrategyPrice) {
		t.Errorf("consumer Placement must be Price on the dashboard; got %+v", ov.Consumers)
	}
}
