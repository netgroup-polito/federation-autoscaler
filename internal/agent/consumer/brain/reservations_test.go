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
	"testing"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/catalog"
)

func resv(id, provider string, phase brokerv1alpha1.ReservationPhase) brokerapi.ReservationResponse {
	return brokerapi.ReservationResponse{
		ReservationID: id, ProviderClusterID: provider, Status: phase, ChunkCount: 1,
	}
}

func TestFromReservationsPhases(t *testing.T) {
	cases := []struct {
		phase            brokerv1alpha1.ReservationPhase
		holds, settingUp bool
	}{
		{"", true, true},
		{brokerv1alpha1.ReservationPhasePending, true, true},
		{brokerv1alpha1.ReservationPhaseGeneratingKubeconfig, true, true},
		{brokerv1alpha1.ReservationPhaseKubeconfigReady, true, true},
		{brokerv1alpha1.ReservationPhasePeering, true, true},
		{brokerv1alpha1.ReservationPhasePeered, true, false},
		{brokerv1alpha1.ReservationPhaseUnpeering, false, false},
		{brokerv1alpha1.ReservationPhaseReleased, false, false},
		{brokerv1alpha1.ReservationPhaseFailed, false, false},
		{brokerv1alpha1.ReservationPhaseExpired, false, false},
	}
	for _, tc := range cases {
		own, inFlight := FromReservations([]brokerapi.ReservationResponse{resv("r", "p1", tc.phase)})
		if got := own["p1"] == 1; got != tc.holds {
			t.Errorf("phase %q: holds a chunk = %v, want %v", tc.phase, got, tc.holds)
		}
		if inFlight["p1"] != tc.settingUp {
			t.Errorf("phase %q: in flight = %v, want %v", tc.phase, inFlight["p1"], tc.settingUp)
		}
	}
}

func TestFromReservationsCountsPerProvider(t *testing.T) {
	own, inFlight := FromReservations([]brokerapi.ReservationResponse{
		resv("a", "p1", brokerv1alpha1.ReservationPhasePeered),
		resv("b", "p1", brokerv1alpha1.ReservationPhasePeering),
		resv("c", "p2", brokerv1alpha1.ReservationPhasePeered),
		resv("d", "p2", brokerv1alpha1.ReservationPhaseReleased),
		{ReservationID: "e", ProviderClusterID: "p3", Status: brokerv1alpha1.ReservationPhasePending}, // ChunkCount unset
	})
	if own["p1"] != 2 || own["p2"] != 1 || own["p3"] != 1 {
		t.Errorf("own = %v, want p1:2 p2:1 p3:1", own)
	}
	if !inFlight["p1"] || inFlight["p2"] || !inFlight["p3"] {
		t.Errorf("inFlight = %v, want p1 and p3 only", inFlight)
	}
}

// TestTwoConsumersSeeOnlyTheirOwnNodes: two consumers share provider p1. Each
// must see only its own chunks there (the Cluster Autoscaler compares this
// number with the nodes it actually has), while the free capacity in the
// catalog is shared.
func TestTwoConsumersSeeOnlyTheirOwnNodes(t *testing.T) {
	// p1 has 4 free chunks left after both consumers reserved.
	offers := []catalog.Offer{siteOffer("p1", "default", 8, 16)}

	ownA, inFlightA := FromReservations([]brokerapi.ReservationResponse{
		resv("a1", "p1", brokerv1alpha1.ReservationPhasePeered),
		resv("a2", "p1", brokerv1alpha1.ReservationPhasePeered),
	})
	ownB, inFlightB := FromReservations([]brokerapi.ReservationResponse{
		resv("b1", "p1", brokerv1alpha1.ReservationPhasePeering),
	})

	a := NodeGroups(Input{Offers: offers, OwnReserved: ownA, InFlight: inFlightA}).NodeGroups[0]
	b := NodeGroups(Input{Offers: offers, OwnReserved: ownB, InFlight: inFlightB}).NodeGroups[0]

	if a.CurrentReserved != 2 || a.MaxSize != 6 {
		t.Errorf("consumer A: CurrentReserved/MaxSize = %d/%d, want 2/6", a.CurrentReserved, a.MaxSize)
	}
	if b.CurrentReserved != 1 || b.MaxSize != 5 {
		t.Errorf("consumer B: CurrentReserved/MaxSize = %d/%d, want 1/5", b.CurrentReserved, b.MaxSize)
	}
}

// TestInFlightGatePerConsumer: the cheapest provider is full. The consumer
// whose chunk there is still being set up must wait (nothing growable); the
// other consumer moves on to the next-cheapest at once.
func TestInFlightGatePerConsumer(t *testing.T) {
	offers := []catalog.Offer{
		withPrice(siteOffer("p-cheap", "default", 0, 0), "0.01"), // no free chunk left
		withPrice(siteOffer("p-dear", "default", 6, 12), "0.09"),
	}
	price := policy(autoscalingv1alpha1.PlacementStrategyPrice)

	ownWaiting, inFlightWaiting := FromReservations([]brokerapi.ReservationResponse{
		resv("w1", "p-cheap", brokerv1alpha1.ReservationPhasePeering),
	})
	waiting := NodeGroups(Input{Offers: offers, Consumer: price, OwnReserved: ownWaiting, InFlight: inFlightWaiting})
	if got := growable(waiting); len(got) != 0 {
		t.Errorf("consumer still peering on p-cheap must wait, growable = %v", got)
	}

	ownSettled, inFlightSettled := FromReservations([]brokerapi.ReservationResponse{
		resv("s1", "p-cheap", brokerv1alpha1.ReservationPhasePeered),
	})
	settled := NodeGroups(Input{Offers: offers, Consumer: price, OwnReserved: ownSettled, InFlight: inFlightSettled})
	if got := growable(settled); len(got) != 1 || got[0] != "p-dear" {
		t.Errorf("settled consumer must move to p-dear, growable = %v", got)
	}
}
