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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/broker/chunk"
)

// Values shared by the catalog tests of this package.
const (
	catProvider                     = "provider-1"
	catRegion                       = "Piedmont"
	catCity                         = "Asti"
	catGPU      corev1.ResourceName = "nvidia.com/gpu"
)

// catReq builds an advertisement of 8 cpu / 16Gi, i.e. 4 standard chunks.
func catReq() *AdvertisementRequest {
	return &AdvertisementRequest{
		ClusterID:     catProvider,
		LiqoClusterID: "liqo-provider-1",
		Resources: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("8"),
			corev1.ResourceMemory: resource.MustParse("16Gi"),
		},
	}
}

func catSized(t *testing.T, req *AdvertisementRequest) chunk.Result {
	t.Helper()
	results := chunk.NewDefaultSizer().Size(req.Resources)
	if len(results) != 1 {
		t.Fatalf("sizer returned %d results, want 1", len(results))
	}
	return results[0]
}

func catWantQty(t *testing.T, got corev1.ResourceList, name corev1.ResourceName, want string) {
	t.Helper()
	q, ok := got[name]
	if !ok {
		t.Fatalf("resource %s missing from %v", name, got)
	}
	w := resource.MustParse(want)
	if q.Cmp(w) != 0 {
		t.Errorf("%s = %s, want %s", name, q.String(), want)
	}
}

func TestOfferFromAdvertisement_SubtractsReservedChunks(t *testing.T) {
	req := catReq()
	o := offerFromAdvertisement(req, 2, catSized(t, req), time.Now())

	// 8 cpu / 16Gi minus 2 chunks of 2 cpu / 4Gi = 4 cpu / 8Gi.
	catWantQty(t, o.Available, corev1.ResourceCPU, "4")
	catWantQty(t, o.Available, corev1.ResourceMemory, "8Gi")
}

func TestOfferFromAdvertisement_NoReservationKeepsFullCapacity(t *testing.T) {
	req := catReq()
	o := offerFromAdvertisement(req, 0, catSized(t, req), time.Now())

	catWantQty(t, o.Available, corev1.ResourceCPU, "8")
	catWantQty(t, o.Available, corev1.ResourceMemory, "16Gi")
}

func TestOfferFromAdvertisement_AvailableNeverNegative(t *testing.T) {
	req := catReq()
	// More reserved chunks than the advertisement holds.
	o := offerFromAdvertisement(req, 10, catSized(t, req), time.Now())

	catWantQty(t, o.Available, corev1.ResourceCPU, "0")
	catWantQty(t, o.Available, corev1.ResourceMemory, "0")
}

func TestOfferFromAdvertisement_DoesNotMutateRequest(t *testing.T) {
	req := catReq()
	_ = offerFromAdvertisement(req, 2, catSized(t, req), time.Now())

	catWantQty(t, req.Resources, corev1.ResourceCPU, "8")
	catWantQty(t, req.Resources, corev1.ResourceMemory, "16Gi")
}

func TestFreeCapacity_ResourceNotInChunkIsUnchanged(t *testing.T) {
	adv := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("8"),
		catGPU:                resource.MustParse("2"),
		corev1.ResourceMemory: resource.MustParse("16Gi"),
	}
	perChunk := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("2"),
		corev1.ResourceMemory: resource.MustParse("4Gi"),
	}
	got := freeCapacity(adv, 2, perChunk)

	catWantQty(t, got, corev1.ResourceCPU, "4")
	catWantQty(t, got, catGPU, "2")
}

func TestOfferFromAdvertisement_Location(t *testing.T) {
	tests := []struct {
		name     string
		topology *brokerv1alpha1.Topology
		wantCoor bool
		wantLat  float64
		wantLon  float64
		region   string
		city     string
	}{
		{name: "no topology"},
		{
			name:     "region and city only, coordinates 0,0",
			topology: &brokerv1alpha1.Topology{Region: catRegion, City: catCity},
			region:   catRegion,
			city:     catCity,
		},
		{
			name: "full coordinates",
			topology: &brokerv1alpha1.Topology{
				Region: catRegion, City: catCity, Latitude: 44.9, Longitude: 8.2,
			},
			wantCoor: true, wantLat: 44.9, wantLon: 8.2,
			region: catRegion, city: catCity,
		},
		{
			name:     "only one coordinate is zero",
			topology: &brokerv1alpha1.Topology{Latitude: 0, Longitude: 8.2},
			wantCoor: true, wantLat: 0, wantLon: 8.2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := catReq()
			req.Topology = tc.topology
			o := offerFromAdvertisement(req, 0, catSized(t, req), time.Now())

			if o.Location.Region != tc.region || o.Location.City != tc.city {
				t.Errorf("region/city = %q/%q, want %q/%q",
					o.Location.Region, o.Location.City, tc.region, tc.city)
			}
			if !tc.wantCoor {
				if o.Location.Latitude != nil || o.Location.Longitude != nil {
					t.Errorf("coordinates must be nil, got %v/%v",
						o.Location.Latitude, o.Location.Longitude)
				}
				return
			}
			if o.Location.Latitude == nil || o.Location.Longitude == nil {
				t.Fatalf("coordinates must be set")
			}
			if *o.Location.Latitude != tc.wantLat || *o.Location.Longitude != tc.wantLon {
				t.Errorf("coordinates = %v/%v, want %v/%v",
					*o.Location.Latitude, *o.Location.Longitude, tc.wantLat, tc.wantLon)
			}
		})
	}
}

func TestOfferFromAdvertisement_CopiesOtherFields(t *testing.T) {
	req := catReq()
	carbon := 120.5
	req.CarbonIntensity = &carbon
	req.CarbonForecast = []float64{120.5, 110, 100}
	req.Renewable = true
	req.ProbeEndpoint = "10.0.0.5:7000"
	req.UnitPrices = corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("0.01"),
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.FixedZone("CET", 3600))

	o := offerFromAdvertisement(req, 1, catSized(t, req), now)

	if o.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", o.SchemaVersion)
	}
	if o.ProviderID != catProvider || o.LiqoClusterID != "liqo-provider-1" {
		t.Errorf("ids = %q/%q", o.ProviderID, o.LiqoClusterID)
	}
	if o.SiteID != "default" {
		t.Errorf("SiteID = %q, want default", o.SiteID)
	}
	if o.CarbonIntensity == nil || *o.CarbonIntensity != 120.5 {
		t.Errorf("CarbonIntensity = %v, want 120.5", o.CarbonIntensity)
	}
	if len(o.CarbonForecast) != 3 || o.CarbonForecast[2] != 100 {
		t.Errorf("CarbonForecast = %v", o.CarbonForecast)
	}
	if !o.Renewable {
		t.Errorf("Renewable = false, want true")
	}
	if o.ProbeEndpoint != "10.0.0.5:7000" {
		t.Errorf("ProbeEndpoint = %q", o.ProbeEndpoint)
	}
	catWantQty(t, o.UnitPrices, corev1.ResourceCPU, "0.01")
	if !o.UpdatedAt.Equal(now) || o.UpdatedAt.Location() != time.UTC {
		t.Errorf("UpdatedAt = %v, want %v in UTC", o.UpdatedAt, now)
	}
	// Fields left empty on purpose in Phase 1.
	if o.SLA != "" || o.ExpectedLatencyMs != nil || o.NodeLabels != nil ||
		o.NodeTaints != nil || o.Extra != nil {
		t.Errorf("SLA, ExpectedLatencyMs, NodeLabels, NodeTaints and Extra must stay empty")
	}
}
