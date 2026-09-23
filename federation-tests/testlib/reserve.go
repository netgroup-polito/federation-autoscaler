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

package testlib

import (
	"context"
	"fmt"
	"time"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// ConsumerReservation tracks the active reservation for one consumer.
type ConsumerReservation struct {
	ReservationID     string
	ProviderClusterID string
	NodeGroupID       string
	Request           *brokerapi.ReservationRequest
}

// MakeReservationID generates a deterministic reservation ID for the test harness.
func MakeReservationID(runID, phase, consumerID string, seq int) string {
	return fmt.Sprintf("test-%s-%s-%s-%d", runID, phase, consumerID, seq)
}

// ReserveAndWait creates a reservation and polls until it reaches Peered.
func (bc *BrokerClient) ReserveAndWait(ctx context.Context, reservationID string, req *brokerapi.ReservationRequest, pollInterval time.Duration) (*brokerapi.ReservationResponse, error) {
	return bc.WaitForPhase(ctx, reservationID, req, string(brokerv1alpha1.ReservationPhasePeered), pollInterval)
}

// ReleaseAndWait deletes a reservation and polls until it reaches Released.
func (bc *BrokerClient) ReleaseAndWait(ctx context.Context, reservationID string, req *brokerapi.ReservationRequest, pollInterval time.Duration) error {
	resp, err := bc.DeleteReservation(ctx, reservationID)
	if err != nil {
		return fmt.Errorf("delete reservation %s: %w", reservationID, err)
	}
	if string(resp.Status) == string(brokerv1alpha1.ReservationPhaseReleased) {
		return nil
	}
	_, err = bc.WaitForPhase(ctx, reservationID, req, string(brokerv1alpha1.ReservationPhaseReleased), pollInterval)
	return err
}

// ShouldSwitch decides whether to switch from the current reservation to a new
// winner. For metric-aware policies (Eco, Latency, Price) it only switches when
// the winner is strictly better (lower PlacementMetric), preventing
// self-occupancy thrashing. For Random (no metric) it always switches.
func ShouldSwitch(current *ConsumerReservation, winnerProviderID string, allNodeGroups []brokerapi.NodeGroupView) bool {
	if current == nil {
		return true
	}
	if current.ProviderClusterID == winnerProviderID {
		return false
	}
	var curMetric, winMetric float64
	var curHas, winHas bool
	for _, ng := range allNodeGroups {
		if ng.ProviderClusterID == current.ProviderClusterID && ng.HasMetric {
			curMetric = ng.PlacementMetric
			curHas = true
		}
		if ng.ProviderClusterID == winnerProviderID && ng.HasMetric {
			winMetric = ng.PlacementMetric
			winHas = true
		}
	}
	if curHas && winHas {
		return winMetric < curMetric
	}
	return true
}
