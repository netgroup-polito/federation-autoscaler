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

package brain

import (
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// FromReservations derives the brain's view of this consumer's own bookings
// from its reservation list (GET /api/v1/reservations):
//   - own[provider] is how many chunks it holds there (Input.OwnReserved),
//     using the Broker's own rule (ReservationHoldsChunk);
//   - inFlight[provider] is true while one of them is still being set up
//     (Input.InFlight), using the Broker's own rule (ReservationInFlight).
//
// Because own counts only this consumer's reservations, the node groups it
// produces report this consumer's nodes, never another consumer's chunks on
// the same provider.
func FromReservations(list []brokerapi.ReservationResponse) (map[string]int32, map[string]bool) {
	own := map[string]int32{}
	inFlight := map[string]bool{}
	for _, r := range list {
		if r.ProviderClusterID == "" {
			continue
		}
		if brokerapi.ReservationHoldsChunk(r.Status) {
			n := r.ChunkCount
			if n <= 0 {
				n = 1 // one reservation is one chunk
			}
			own[r.ProviderClusterID] += n
		}
		if brokerapi.ReservationInFlight(r.Status) {
			inFlight[r.ProviderClusterID] = true
		}
	}
	return own, inFlight
}
