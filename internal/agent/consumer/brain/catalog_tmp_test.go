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
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func testOffer(provider, site string) Offer {
	return Offer{
		SchemaVersion: 1,
		ProviderID:    provider,
		LiqoClusterID: provider,
		SiteID:        site,
		Available: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("12"),
			corev1.ResourceMemory: resource.MustParse("24Gi"),
		},
	}
}

// fakeClock is a settable clock for exercising OfferTTL.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestCatalog() (*MemoryCatalog, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	m := NewMemoryCatalog()
	m.Now = clk.now
	return m, clk
}

func TestMemoryCatalogUpsertGetList(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestCatalog()
	for _, o := range []Offer{testOffer("fibercop", "asti"), testOffer("fibercop", "alba"), testOffer("p2", "default")} {
		if err := m.UpsertOffer(ctx, o); err != nil {
			t.Fatalf("UpsertOffer: %v", err)
		}
	}

	got, err := m.GetOffer(ctx, "fibercop", "alba")
	if err != nil || got.SiteID != "alba" {
		t.Fatalf("GetOffer = %+v, %v", got, err)
	}

	list, err := m.ListOffers(ctx)
	if err != nil {
		t.Fatalf("ListOffers: %v", err)
	}
	keys := make([]string, 0, len(list))
	for _, o := range list {
		keys = append(keys, o.ProviderID+"/"+o.SiteID)
	}
	sort.Strings(keys)
	if want := []string{"fibercop/alba", "fibercop/asti", "p2/default"}; len(keys) != 3 ||
		keys[0] != want[0] || keys[1] != want[1] || keys[2] != want[2] {
		t.Errorf("ListOffers keys = %v, want %v", keys, want)
	}

	// One offer per (provider, site): a re-upsert replaces, it never duplicates.
	again := testOffer("fibercop", "asti")
	again.SLA = "gold"
	_ = m.UpsertOffer(ctx, again)
	list, _ = m.ListOffers(ctx)
	if len(list) != 3 {
		t.Errorf("re-upsert must replace, got %d offers", len(list))
	}
	if got, _ := m.GetOffer(ctx, "fibercop", "asti"); got.SLA != "gold" {
		t.Errorf("re-upsert not applied: %+v", got)
	}
}

func TestMemoryCatalogExpiry(t *testing.T) {
	ctx := context.Background()
	m, clk := newTestCatalog()
	_ = m.UpsertOffer(ctx, testOffer("p1", "default"))

	clk.t = clk.t.Add(OfferTTL - time.Second)
	if _, err := m.GetOffer(ctx, "p1", "default"); err != nil {
		t.Fatalf("offer must still be live just before OfferTTL: %v", err)
	}

	// A refresh restarts the TTL.
	_ = m.UpsertOffer(ctx, testOffer("p1", "default"))
	clk.t = clk.t.Add(OfferTTL - time.Second)
	if _, err := m.GetOffer(ctx, "p1", "default"); err != nil {
		t.Fatalf("refresh must restart the TTL: %v", err)
	}

	clk.t = clk.t.Add(time.Second)
	if _, err := m.GetOffer(ctx, "p1", "default"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired offer: want ErrNotFound, got %v", err)
	}
	if list, _ := m.ListOffers(ctx); len(list) != 0 {
		t.Errorf("expired offer must not be listed, got %+v", list)
	}
}

func TestMemoryCatalogDeleteAndMissing(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestCatalog()
	if _, err := m.GetOffer(ctx, "nobody", "default"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing offer: want ErrNotFound, got %v", err)
	}
	_ = m.UpsertOffer(ctx, testOffer("p1", "default"))
	if err := m.DeleteOffer(ctx, "p1", "default"); err != nil {
		t.Fatalf("DeleteOffer: %v", err)
	}
	if _, err := m.GetOffer(ctx, "p1", "default"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted offer: want ErrNotFound, got %v", err)
	}
	if err := m.DeleteOffer(ctx, "p1", "default"); err != nil {
		t.Errorf("deleting a missing offer must not fail: %v", err)
	}
	if err := m.UpsertOffer(ctx, Offer{ProviderID: "p1"}); err == nil {
		t.Error("an offer without siteId must be rejected")
	}
}

// TestOfferJSONContract pins the wire names of the contract and checks that
// readers ignore fields they don't know (forward compatibility).
func TestOfferJSONContract(t *testing.T) {
	raw := `{
	  "schemaVersion": 1,
	  "providerId": "fibercop",
	  "liqoClusterId": "fibercop",
	  "siteId": "asti",
	  "location": { "region": "Piedmont", "city": "Asti", "latitude": 44.90, "longitude": 8.21 },
	  "available": { "cpu": "12", "memory": "24Gi", "nvidia.com/gpu": "2" },
	  "unitPrices": { "cpu": "0.03", "memory": "0.004" },
	  "expectedLatencyMs": 2,
	  "probeEndpoint": "asti.fibercop.example:7777",
	  "extra": { "note": "powered by renewable energy at night" },
	  "updatedAt": "2026-11-03T10:00:00Z",
	  "aFieldFromTheFuture": { "anything": true }
	}`
	var o Offer
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		t.Fatalf("decode contract example: %v", err)
	}
	gpu := o.Available[corev1.ResourceName("nvidia.com/gpu")]
	switch {
	case o.ProviderID != "fibercop" || o.SiteID != "asti" || o.SchemaVersion != 1:
		t.Errorf("identity fields: %+v", o)
	case o.Location.City != "Asti" || o.Location.Latitude == nil || *o.Location.Latitude != 44.90:
		t.Errorf("location: %+v", o.Location)
	case gpu.Value() != 2 || o.Available.Memory().Value() != 24*1024*1024*1024:
		t.Errorf("available: %v", o.Available)
	case o.ExpectedLatencyMs == nil || *o.ExpectedLatencyMs != 2:
		t.Errorf("expectedLatencyMs: %v", o.ExpectedLatencyMs)
	case o.Extra["note"] != "powered by renewable energy at night":
		t.Errorf("extra: %v", o.Extra)
	}
}
