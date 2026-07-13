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

package advertise

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/netgroup-polito/federation-autoscaler/internal/agent/eco"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/geo"
)

// geoStub serves one canned /json/{ip} location and records the path it was hit
// with, so a test can assert which IP was geolocated.
func geoStub(t *testing.T, body string, gotPath *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotPath != nil {
			*gotPath = r.URL.Path
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func nodeClient(t *testing.T, node *corev1.Node) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	b := fake.NewClientBuilder().WithScheme(scheme)
	if node != nil {
		b = b.WithRuntimeObjects(node)
	}
	return b
}

func TestLoadPlacementInputsDiscoversFromNodeIP(t *testing.T) {
	var path string
	srv := geoStub(t, `{"status":"success","region":"HE","city":"Frankfurt","lat":50.1109,"lon":8.6821,"countryCode":"DE"}`, &path)
	defer srv.Close()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "172.23.7.10"},
		}},
	}
	p := &Publisher{
		localClient: nodeClient(t, node).Build(),
		geoClient:   geo.NewClient(),
		ecoClient:   eco.NewClient(),
		nodeName:    "n1",
		mockGeoURL:  srv.URL,
		// mockEcoURL left empty ⇒ no carbon lookup.
		log: logr.Discard(),
	}

	topo, carbon, forecast := p.loadPlacementInputs(context.Background())
	if topo == nil {
		t.Fatal("expected a topology from discovery")
	}
	if path != "/json/172.23.7.10" {
		t.Errorf("geolocated %q, want the node's InternalIP", path)
	}
	if topo.Region != "HE" || topo.City != "Frankfurt" {
		t.Errorf("topology = %+v, want HE/Frankfurt", topo)
	}
	if topo.Latitude != 50.1109 || topo.Longitude != 8.6821 {
		t.Errorf("coords = %v,%v, want 50.1109,8.6821", topo.Latitude, topo.Longitude)
	}
	if carbon != nil || forecast != nil {
		t.Errorf("expected no carbon (mock-eco unset), got %v / %v", carbon, forecast)
	}
}

func TestLoadPlacementInputsAdvertisedIPOverride(t *testing.T) {
	var path string
	srv := geoStub(t, `{"status":"success","region":"NSW","city":"Sydney","lat":-33.8688,"lon":151.2093}`, &path)
	defer srv.Close()

	// With an override IP the node is never read (nil client is fine).
	p := &Publisher{
		localClient:  nil,
		geoClient:    geo.NewClient(),
		ecoClient:    eco.NewClient(),
		advertisedIP: "172.23.4.130",
		mockGeoURL:   srv.URL,
		log:          logr.Discard(),
	}

	topo, _, _ := p.loadPlacementInputs(context.Background())
	if topo == nil || topo.City != "Sydney" {
		t.Fatalf("topology = %+v, want Sydney", topo)
	}
	if path != "/json/172.23.4.130" {
		t.Errorf("geolocated %q, want the override IP", path)
	}
}

func TestLoadPlacementInputsNoDiscovery(t *testing.T) {
	// No node name, no override ⇒ no location advertised (mock-geo never hit).
	p := &Publisher{
		localClient: nodeClient(t, nil).Build(),
		geoClient:   geo.NewClient(),
		ecoClient:   eco.NewClient(),
		mockGeoURL:  "http://mock-geo.invalid",
		log:         logr.Discard(),
	}
	topo, carbon, forecast := p.loadPlacementInputs(context.Background())
	if topo != nil || carbon != nil || forecast != nil {
		t.Errorf("expected no placement inputs, got %+v / %v / %v", topo, carbon, forecast)
	}
}
