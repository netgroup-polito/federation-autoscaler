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

// Package mockeco is a tiny stand-in for a grid carbon-intensity API
// (ElectricityMaps-style) used by the eco placement strategy demo. It is
// REGION-KEYED: GET /carbon?region=QC returns that region's CURRENT-hour carbon
// intensity (gCO2eq/kWh). It runs on the dedicated mock cluster; provider agents
// fetch their own region's value and advertise it to the Broker (the Broker
// never calls this service).
package mockeco

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// regionData holds a 24-hour carbon-intensity profile (gCO2eq/kWh) per region,
// indexed by UTC hour. Values are realistic estimates by energy mix: hydro and
// nuclear regions are low (Quebec, Ile-de-France ~20-80), coal-heavy regions are
// high (New South Wales ~600-700). Region codes match the geo service.
var regionData = map[string][24]int{
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
}

// carbonResponse is the JSON shape consumed by the agent's eco client.
type carbonResponse struct {
	Region          string `json:"region"`
	CarbonIntensity int    `json:"carbonIntensity"`
}

// forecastResponse is the JSON shape of GET /carbon/forecast: the next 24 hours
// of carbon intensity, current hour first.
type forecastResponse struct {
	Region   string `json:"region"`
	Forecast []int  `json:"forecast"`
}

// lookupProfile resolves the region's 24-hour profile, writing an error response
// and returning ok=false when the region is missing/unknown.
func lookupProfile(w http.ResponseWriter, region string) ([24]int, bool) {
	if region == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "missing 'region' query parameter"})
		return [24]int{}, false
	}
	profile, ok := regionData[region]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("unknown region: %s", region)})
		return [24]int{}, false
	}
	return profile, true
}

// Handler serves GET /carbon?region=XX with the region's current-hour carbon
// intensity. Lowest wins under the eco placement strategy.
func Handler(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	w.Header().Set("Content-Type", "application/json")
	profile, ok := lookupProfile(w, region)
	if !ok {
		return
	}
	current := profile[time.Now().UTC().Hour()%24]
	if err := json.NewEncoder(w).Encode(carbonResponse{Region: region, CarbonIntensity: current}); err != nil {
		log.Printf("mock-eco: encode error: %v", err)
	}
}

// ForecastHandler serves GET /carbon/forecast?region=XX with the next 24 hours of
// carbon intensity, current hour first (wrapping the region's daily profile). The
// Broker's eco ranking weights the first few hours; the agent advertises the
// series alongside the current value.
func ForecastHandler(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	w.Header().Set("Content-Type", "application/json")
	profile, ok := lookupProfile(w, region)
	if !ok {
		return
	}
	start := time.Now().UTC().Hour() % 24
	forecast := make([]int, 24)
	for i := range forecast {
		forecast[i] = profile[(start+i)%24]
	}
	if err := json.NewEncoder(w).Encode(forecastResponse{Region: region, Forecast: forecast}); err != nil {
		log.Printf("mock-eco: encode error: %v", err)
	}
}

// StartServer starts the mock-eco HTTP server on the given port and blocks.
func StartServer(port int) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /carbon", Handler)
	mux.HandleFunc("GET /carbon/forecast", ForecastHandler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	addr := fmt.Sprintf(":%d", port)
	log.Printf("mock-eco listening on %s", addr)
	return http.ListenAndServe(addr, mux)
}
