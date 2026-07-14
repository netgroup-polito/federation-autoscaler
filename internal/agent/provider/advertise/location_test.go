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

	"github.com/netgroup-polito/federation-autoscaler/internal/agent/eco"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/geo"
)

// geoStub serves one canned /json/{ip} location and records the path it was hit
// with, so a test can assert which IP was geolocated. Node-IP resolution itself
// is covered by the nodeip package; loadPlacementInputs takes an already-resolved
// ip, so these tests exercise the ip → geo → Topology mapping directly.
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

func TestLoadPlacementInputsGeolocatesIP(t *testing.T) {
	var path string
	srv := geoStub(t, `{"status":"success","region":"HE","city":"Frankfurt","lat":50.1109,"lon":8.6821,"countryCode":"DE"}`, &path)
	defer srv.Close()

	p := &Publisher{
		geoClient:  geo.NewClient(),
		ecoClient:  eco.NewClient(),
		mockGeoURL: srv.URL,
		// mockEcoURL left empty ⇒ no carbon lookup.
		log: logr.Discard(),
	}

	topo, carbon, forecast := p.loadPlacementInputs(context.Background(), "172.23.7.10")
	if topo == nil {
		t.Fatal("expected a topology from the resolved IP")
	}
	if path != "/json/172.23.7.10" {
		t.Errorf("geolocated %q, want /json/172.23.7.10", path)
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

func TestLoadPlacementInputsEmptyIP(t *testing.T) {
	// No resolved IP ⇒ no location advertised (mock-geo never hit).
	p := &Publisher{
		geoClient:  geo.NewClient(),
		ecoClient:  eco.NewClient(),
		mockGeoURL: "http://mock-geo.invalid",
		log:        logr.Discard(),
	}
	topo, carbon, forecast := p.loadPlacementInputs(context.Background(), "")
	if topo != nil || carbon != nil || forecast != nil {
		t.Errorf("expected no placement inputs, got %+v / %v / %v", topo, carbon, forecast)
	}
}

func TestLoadPlacementInputsDefaultRowNoLocation(t *testing.T) {
	// A default-row / no-location geo response ⇒ nil topology (the geo client
	// returns ok=false), so the provider advertises no location.
	srv := geoStub(t, `{"status":"success","query":"8.8.8.8"}`, nil)
	defer srv.Close()

	p := &Publisher{
		geoClient:  geo.NewClient(),
		ecoClient:  eco.NewClient(),
		mockGeoURL: srv.URL,
		log:        logr.Discard(),
	}
	topo, _, _ := p.loadPlacementInputs(context.Background(), "8.8.8.8")
	if topo != nil {
		t.Errorf("expected nil topology for a no-location response, got %+v", topo)
	}
}
