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
	"math"
	"testing"
)

// TestHaversineKm checks the great-circle helper against known city distances
// (the metric the latency strategy ranks by) plus the zero and symmetry cases.
func TestHaversineKm(t *testing.T) {
	const (
		montrealLat, montrealLon = 45.6085, -73.5493
		milanLat, milanLon       = 45.4642, 9.1900
		sydneyLat, sydneyLon     = -33.8688, 151.2093
	)

	tests := []struct {
		name                   string
		lat1, lon1, lat2, lon2 float64
		wantKm, tolKm          float64
	}{
		{"same point is zero", montrealLat, montrealLon, montrealLat, montrealLon, 0, 1},
		{"Montreal->Milan ~6130km", montrealLat, montrealLon, milanLat, milanLon, 6130, 150},
		{"Montreal->Sydney ~16000km", montrealLat, montrealLon, sydneyLat, sydneyLon, 16000, 600},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HaversineKm(tt.lat1, tt.lon1, tt.lat2, tt.lon2)
			if math.Abs(got-tt.wantKm) > tt.tolKm {
				t.Errorf("HaversineKm = %.1f km, want %.0f ± %.0f", got, tt.wantKm, tt.tolKm)
			}
		})
	}

	// Distance is symmetric.
	ab := HaversineKm(montrealLat, montrealLon, sydneyLat, sydneyLon)
	ba := HaversineKm(sydneyLat, sydneyLon, montrealLat, montrealLon)
	if math.Abs(ab-ba) > 1e-6 {
		t.Errorf("haversine not symmetric: %.6f vs %.6f", ab, ba)
	}
}
