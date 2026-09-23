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

// Package mockecotest is a controllable variant of the mock-eco carbon service.
// Unlike the production mock-eco (internal/mockeco), it exposes an admin API
// (POST /admin/carbon) that lets a test harness set carbon intensity per region
// at runtime. This enables deterministic eco-policy experiments where the
// harness controls which provider the Broker ranks lowest (greenest).
//
// Wire-compatible: GET /carbon and GET /carbon/forecast return the same JSON
// shape the Provider Agent's eco client expects, so Provider Agents can be
// pointed at this service as a drop-in replacement via --mock-eco-url.
package mockecotest

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
)

// Server is the controllable mock-eco HTTP server.
type Server struct {
	mu       sync.RWMutex
	regions  map[string]RegionConfig
	defaults map[string][24]int
}

// RegionConfig holds the harness-controlled carbon data for one region.
type RegionConfig struct {
	CarbonIntensity int   `json:"carbonIntensity"`
	Forecast        []int `json:"forecast,omitempty"`
}

// New returns a Server pre-seeded with the same static profiles as the
// production mock-eco, so it behaves identically until the harness overrides.
func New() *Server {
	return &Server{
		regions:  make(map[string]RegionConfig),
		defaults: defaultProfiles(),
	}
}

type carbonResponse struct {
	Region          string `json:"region"`
	CarbonIntensity int    `json:"carbonIntensity"`
}

type forecastResponse struct {
	Region   string `json:"region"`
	Forecast []int  `json:"forecast"`
}

// Handler returns the HTTP handler for the controllable mock-eco.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /carbon", s.handleCarbon)
	mux.HandleFunc("GET /carbon/forecast", s.handleForecast)
	mux.HandleFunc("POST /admin/carbon", s.handleAdminCarbon)
	mux.HandleFunc("DELETE /admin/carbon", s.handleAdminReset)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (s *Server) handleCarbon(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	w.Header().Set("Content-Type", "application/json")
	if region == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "missing 'region' query parameter"})
		return
	}

	s.mu.RLock()
	rc, overridden := s.regions[region]
	s.mu.RUnlock()

	if overridden {
		_ = json.NewEncoder(w).Encode(carbonResponse{Region: region, CarbonIntensity: rc.CarbonIntensity})
		return
	}

	if _, ok := s.defaults[region]; !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("unknown region: %s", region)})
		return
	}
	// Fallback: return the first hour from defaults (static behaviour).
	_ = json.NewEncoder(w).Encode(carbonResponse{Region: region, CarbonIntensity: s.defaults[region][0]})
}

func (s *Server) handleForecast(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	w.Header().Set("Content-Type", "application/json")
	if region == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "missing 'region' query parameter"})
		return
	}

	s.mu.RLock()
	rc, overridden := s.regions[region]
	s.mu.RUnlock()

	if overridden && len(rc.Forecast) > 0 {
		_ = json.NewEncoder(w).Encode(forecastResponse{Region: region, Forecast: rc.Forecast})
		return
	}
	if overridden {
		// No explicit forecast: generate a flat forecast from the override value.
		flat := make([]int, 24)
		for i := range flat {
			flat[i] = rc.CarbonIntensity
		}
		_ = json.NewEncoder(w).Encode(forecastResponse{Region: region, Forecast: flat})
		return
	}

	profile, ok := s.defaults[region]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("unknown region: %s", region)})
		return
	}
	forecast := make([]int, 24)
	copy(forecast, profile[:])
	_ = json.NewEncoder(w).Encode(forecastResponse{Region: region, Forecast: forecast})
}

// handleAdminCarbon sets the carbon intensity (and optional forecast) for a
// region. Body: {"region":"QC","carbonIntensity":50,"forecast":[50,50,...]}
func (s *Server) handleAdminCarbon(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Region          string `json:"region"`
		CarbonIntensity int    `json:"carbonIntensity"`
		Forecast        []int  `json:"forecast,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "decode body: " + err.Error()})
		return
	}
	if body.Region == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "region is required"})
		return
	}

	s.mu.Lock()
	s.regions[body.Region] = RegionConfig{
		CarbonIntensity: body.CarbonIntensity,
		Forecast:        body.Forecast,
	}
	s.mu.Unlock()

	log.Printf("mock-eco: set region %s → carbon=%d forecast=%v", body.Region, body.CarbonIntensity, body.Forecast)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handleAdminReset clears all overrides, reverting to static defaults.
func (s *Server) handleAdminReset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.regions = make(map[string]RegionConfig)
	s.mu.Unlock()

	log.Printf("mock-eco: all overrides cleared")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// StartServer starts the controllable mock-eco server on the given port.
func StartServer(port int) error {
	s := New()
	addr := fmt.Sprintf(":%d", port)
	log.Printf("controllable mock-eco listening on %s", addr)
	return http.ListenAndServe(addr, s.Handler())
}

func defaultProfiles() map[string][24]int {
	return map[string][24]int{
		"QC":  {25, 22, 20, 19, 18, 18, 20, 24, 30, 35, 38, 40, 42, 43, 41, 38, 35, 33, 30, 28, 27, 26, 25, 24},
		"LOM": {320, 310, 300, 290, 285, 280, 290, 310, 340, 350, 330, 300, 270, 250, 240, 245, 260, 290, 320, 340, 350, 345, 335, 325},
		"CA":  {280, 270, 260, 255, 250, 245, 230, 200, 160, 120, 90, 80, 75, 80, 90, 130, 180, 240, 290, 310, 320, 310, 300, 290},
		"HE":  {380, 370, 360, 350, 345, 340, 350, 370, 400, 410, 390, 360, 330, 310, 300, 310, 330, 360, 390, 410, 420, 410, 400, 390},
		"13":  {450, 440, 430, 420, 415, 410, 420, 440, 470, 490, 480, 460, 440, 430, 425, 430, 440, 460, 480, 490, 495, 485, 470, 460},
		"NSW": {650, 640, 630, 620, 610, 600, 590, 610, 650, 680, 700, 690, 660, 630, 610, 600, 610, 640, 670, 690, 700, 695, 680, 660},
		"IDF": {60, 55, 50, 48, 45, 44, 46, 52, 65, 75, 80, 78, 72, 68, 64, 62, 64, 70, 78, 82, 80, 75, 68, 63},
		"SP":  {90, 85, 80, 78, 75, 74, 78, 85, 100, 110, 115, 112, 105, 98, 92, 88, 90, 95, 105, 112, 115, 110, 100, 95},
		"SG":  {420, 415, 410, 405, 400, 398, 400, 410, 430, 450, 460, 455, 445, 435, 428, 425, 430, 440, 450, 458, 460, 455, 445, 430},
		"ENG": {250, 240, 230, 225, 220, 215, 220, 240, 270, 290, 285, 260, 240, 225, 215, 220, 235, 260, 280, 295, 300, 290, 275, 260},
		"AB":  {20, 18, 16, 15, 14, 14, 16, 20, 26, 30, 32, 33, 34, 35, 33, 30, 28, 26, 24, 22, 21, 20, 19, 18},
		"MH":  {680, 670, 660, 650, 645, 640, 650, 670, 700, 720, 740, 750, 745, 735, 720, 710, 715, 730, 750, 770, 780, 770, 750, 720},
	}
}
