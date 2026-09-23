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

// Package mockgeotest is a controllable variant of the mock-geo service.
// It wraps the production mock-geo's static CIDR table and adds an admin API
// (POST /admin/geo) that lets a test harness register per-IP location overrides
// at runtime. This enables Kind-based tests where provider IPs are on the Docker
// bridge (172.18.0.x) — not in any known CIDR block — so the default table
// returns no location for them. The harness registers each provider's Docker
// bridge IP with the desired region after cluster creation.
//
// Wire-compatible: GET /json/{ip} returns the same JSON shape the agent's geo
// client expects (ip-api.com format), so agents can use this as a drop-in.
package mockgeotest

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/netgroup-polito/federation-autoscaler/internal/mockgeo"
)

// Server is the controllable mock-geo HTTP server.
type Server struct {
	mu        sync.RWMutex
	overrides map[string]mockgeo.GeoResponse
}

// New returns a Server with no overrides (falls back to the static table).
func New() *Server {
	return &Server{
		overrides: make(map[string]mockgeo.GeoResponse),
	}
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /json/{ip}", s.handleLookup)
	mux.HandleFunc("POST /admin/geo", s.handleAdminSet)
	mux.HandleFunc("DELETE /admin/geo", s.handleAdminReset)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (s *Server) handleLookup(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	w.Header().Set("Content-Type", "application/json")

	s.mu.RLock()
	override, ok := s.overrides[ip]
	s.mu.RUnlock()

	if ok {
		override.Query = ip
		override.Status = "success"
		_ = json.NewEncoder(w).Encode(override)
		return
	}

	// Fall back to the production mock-geo handler.
	mockgeo.Handler(w, r)
}

// handleAdminSet registers a per-IP location override.
// Body: {"ip":"172.18.0.5","region":"QC","city":"Montreal","lat":45.6,"lon":-73.5}
func (s *Server) handleAdminSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IP     string  `json:"ip"`
		Region string  `json:"region"`
		City   string  `json:"city"`
		Lat    float64 `json:"lat"`
		Lon    float64 `json:"lon"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "decode body: " + err.Error()})
		return
	}
	if body.IP == "" || body.Region == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "ip and region are required"})
		return
	}

	s.mu.Lock()
	s.overrides[body.IP] = mockgeo.GeoResponse{
		Region:     body.Region,
		RegionName: body.Region,
		City:       body.City,
		Lat:        body.Lat,
		Lon:        body.Lon,
	}
	s.mu.Unlock()

	log.Printf("mock-geo: override %s → region=%s city=%s lat=%.4f lon=%.4f",
		body.IP, body.Region, body.City, body.Lat, body.Lon)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *Server) handleAdminReset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.overrides = make(map[string]mockgeo.GeoResponse)
	s.mu.Unlock()

	log.Printf("mock-geo: all overrides cleared")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// StartServer starts the controllable mock-geo server on the given port.
func StartServer(port int) error {
	s := New()
	addr := fmt.Sprintf(":%d", port)
	log.Printf("controllable mock-geo listening on %s", addr)
	return http.ListenAndServe(addr, s.Handler())
}
