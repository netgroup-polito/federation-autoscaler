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

// Package geo holds the distance and latency estimates shared by the Broker,
// the region orchestrator and the consumer agent. No imports from the rest of
// the module, so any package can use it.
package geo

import "math"

// earthRadiusKm is the mean Earth radius used by the Haversine formula.
const earthRadiusKm = 6371.0

// fibreKmPerMs is how many kilometres of straight-line distance add one
// millisecond of round-trip time over fibre (light in glass covers about
// 200 km per ms, there and back). Real paths are longer than a straight line,
// so the estimate is optimistic: a site it rejects is surely out of the bound.
const fibreKmPerMs = 100.0

// HaversineKm returns the great-circle distance, in kilometres, between two
// points given in decimal degrees.
func HaversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	toRad := func(deg float64) float64 { return deg * math.Pi / 180.0 }

	phi1, phi2 := toRad(lat1), toRad(lat2)
	dPhi := toRad(lat2 - lat1)
	dLambda := toRad(lon2 - lon1)

	a := math.Sin(dPhi/2)*math.Sin(dPhi/2) +
		math.Cos(phi1)*math.Cos(phi2)*math.Sin(dLambda/2)*math.Sin(dLambda/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusKm * c
}

// EstimateLatencyMs is the estimated round-trip latency, in milliseconds,
// between two points distanceKm apart.
func EstimateLatencyMs(distanceKm float64) float64 {
	return distanceKm / fibreKmPerMs
}
