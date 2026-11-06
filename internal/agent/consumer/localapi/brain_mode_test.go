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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/brain"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

func pricedOffer(provider, cpuPrice string) brain.Offer {
	return brain.Offer{
		SchemaVersion: 1,
		ProviderID:    provider,
		LiqoClusterID: "liqo-" + provider,
		SiteID:        "default",
		Available: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("6"),
			corev1.ResourceMemory: resource.MustParse("12Gi"),
		},
		UnitPrices: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpuPrice),
			corev1.ResourceMemory: resource.MustParse("0.001"),
		},
	}
}

// TestLocalBrainMode: with a Brain configured, GET /local/nodegroups is built
// locally from the catalog (the Broker's /api/v1/nodegroups is never called),
// and a "no free chunk" refusal on POST /local/reservations makes the next
// decision move on to the next-best provider.
func TestLocalBrainMode(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	fb := newFakeBroker(t)
	fb.setHandler(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/reservations":
			_ = json.NewEncoder(w).Encode(brokerapi.ReservationListResponse{Reservations: []brokerapi.ReservationResponse{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/reservations":
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(brokerapi.ErrorResponse{
				Code: brokerapi.ErrCodeInsufficientCapacity, Message: "no free chunk",
			})
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	})

	catalog := brain.NewMemoryCatalog()
	for _, o := range []brain.Offer{pricedOffer("p-cheap", "0.01"), pricedOffer("p-dear", "0.09")} {
		if err := catalog.UpsertOffer(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	client := fb.buildClient(t)
	kube := newFakeKubeClient(&autoscalingv1alpha1.ConsumerPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "federation-autoscaler-system"},
		Spec: autoscalingv1alpha1.ConsumerPolicySpec{
			Placement: autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyPrice},
		},
	})
	b, err := brain.New(brain.Options{
		Catalog: catalog, Reservations: client, LocalClient: kube, Namespace: "federation-autoscaler-system",
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		BindAddress: "127.0.0.1:0",
		Client:      client,
		LocalClient: kube,
		Namespace:   "federation-autoscaler-system",
		Brain:       b,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	if hr := headroom(getNodeGroups(t, ts)); hr["p-cheap"] != 3 || hr["p-dear"] != 0 {
		t.Fatalf("local Price decision: headroom = %v, want p-cheap growable only", hr)
	}
	if got := b.LastChoice(); got != "p-cheap" {
		t.Errorf("recorded choice = %q, want p-cheap (reported on the heartbeat)", got)
	}

	body, _ := json.Marshal(brokerapi.ReservationRequest{
		ProviderClusterID: "p-cheap", NodeGroupID: "ng-p-cheap-standard", ChunkCount: 1, ChunkType: "standard",
	})
	resp, err := ts.Client().Post(ts.URL+"/local/reservations", brokerapi.ContentTypeJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST /local/reservations status = %d, want 409 passed through", resp.StatusCode)
	}

	if hr := headroom(getNodeGroups(t, ts)); hr["p-cheap"] != 0 || hr["p-dear"] != 3 {
		t.Errorf("after the refusal: headroom = %v, want p-dear growable only", hr)
	}
	if got := b.LastChoice(); got != "p-dear" {
		t.Errorf("recorded choice after the refusal = %q, want p-dear", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if n := calls["GET /api/v1/nodegroups"]; n != 0 {
		t.Errorf("local mode must not ask the Broker for node groups, got %d calls", n)
	}
	if calls["GET /api/v1/reservations"] == 0 {
		t.Error("local mode must read this consumer's own reservations")
	}
}
