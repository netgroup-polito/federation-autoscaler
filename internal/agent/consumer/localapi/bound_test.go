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

package localapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/brain"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/catalog"
)

func TestLatencyBoundQueryRoundTrip(t *testing.T) {
	if EncodeLatencyBound(nil) != nil {
		t.Error("a nil bound encodes to no query")
	}
	if b, err := DecodeLatencyBound(url.Values{}); b != nil || err != nil {
		t.Errorf("no query decodes to no bound, got %+v, %v", b, err)
	}

	got, err := DecodeLatencyBound(EncodeLatencyBound(&autoscalingv1alpha1.LatencyBound{MaxLatencyMs: 5}))
	if err != nil || got == nil || got.MaxLatencyMs != 5 {
		t.Errorf("bound: got %+v, %v", got, err)
	}
}

func TestLatencyBoundQueryRejectsBadInput(t *testing.T) {
	for name, raw := range map[string]string{
		"zero ms":           "maxLatencyMs=0",
		"negative":          "maxLatencyMs=-2",
		"not a number":      "maxLatencyMs=abc",
		"too big for int32": "maxLatencyMs=99999999999",
	} {
		q, _ := url.ParseQuery(raw)
		if _, err := DecodeLatencyBound(q); err == nil {
			t.Errorf("%s (%s): want an error", name, raw)
		}
	}
}

// TestLocalBrainAppliesRequestBound: a bound in the query (a ResourceRequest's
// latencyBound) overrides the ConsumerPolicy for that one call, and a
// malformed bound is rejected with 400.
func TestLocalBrainAppliesRequestBound(t *testing.T) {
	fb := newFakeBroker(t)
	fb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.ReservationListResponse{Reservations: []brokerapi.ReservationResponse{}})
	})
	astiLat, astiLon, romeLat, romeLon := 44.9008, 8.2064, 41.9028, 12.4964
	asti, rome := pricedOffer("p-asti", "0.05"), pricedOffer("p-rome", "0.01")
	asti.Location = catalog.Location{Latitude: &astiLat, Longitude: &astiLon}
	rome.Location = catalog.Location{Latitude: &romeLat, Longitude: &romeLon}
	cat := catalog.NewMemoryCatalog()
	for _, o := range []catalog.Offer{asti, rome} {
		if err := cat.UpsertOffer(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	client := fb.buildClient(t)
	kube := newFakeKubeClient() // no ConsumerPolicy: Standard, no bound
	b, err := brain.New(brain.Options{
		Catalog: cat, Reservations: client, LocalClient: kube,
		Location: func() (float64, float64, bool) { return astiLat, astiLon, true }, // consumer in Asti
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{BindAddress: "127.0.0.1:0", Client: client, LocalClient: kube, Brain: b})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	if hr := headroom(getNodeGroups(t, ts)); len(hr) != 2 {
		t.Fatalf("without a bound both providers are listed, got %v", hr)
	}

	q := EncodeLatencyBound(&autoscalingv1alpha1.LatencyBound{MaxLatencyMs: 3})
	resp, err := ts.Client().Get(ts.URL + "/local/nodegroups?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	var bounded brokerapi.NodeGroupListResponse
	_ = json.NewDecoder(resp.Body).Decode(&bounded)
	_ = resp.Body.Close()
	if hr := headroom(bounded); len(hr) != 1 || hr["p-asti"] != 3 {
		t.Errorf("3 ms to a consumer in Asti: want only p-asti, got %v", hr)
	}

	bad, err := ts.Client().Get(ts.URL + "/local/nodegroups?maxLatencyMs=0")
	if err != nil {
		t.Fatal(err)
	}
	_ = bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("a malformed bound must be rejected with 400, got %d", bad.StatusCode)
	}
}
