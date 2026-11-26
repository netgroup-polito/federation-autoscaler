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

import "github.com/netgroup-polito/federation-autoscaler/internal/broker/geo"

// HaversineKm returns the great-circle distance, in kilometres, between two
// points given in decimal degrees (see internal/broker/geo). For the latency
// strategy the Broker ranks providers by this distance to build the top-N
// nearest SHORTLIST (computed from advertised coordinates alone, no probing,
// no Broker dial-out); the Consumer Agent then UDP-probes the shortlist.
//
// Exported for the ConsumerChoice end-to-end suite (federation-tests/consumerchoice),
// which judges proximity requests with the same figure the Latency policy ranks on.
func HaversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	return geo.HaversineKm(lat1, lon1, lat2, lon2)
}
