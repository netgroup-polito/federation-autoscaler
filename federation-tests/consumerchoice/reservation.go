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

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/federation-tests/testlib"
	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// PhaseTransition is one observed reservation phase and when it was first seen.
// Phases are sampled by polling, so a phase shorter than the poll interval can
// be missed and every timestamp is "first seen", not "entered".
type PhaseTransition struct {
	Phase   string    `json:"phase"`
	SeenAt  time.Time `json:"seenAt"`
	Message string    `json:"message,omitempty"`
}

// ReservationResult is the full record of the single reservation a repetition makes.
type ReservationResult struct {
	ReservationID       string                         `json:"reservationId"`
	RequestedProviderID string                         `json:"requestedProviderId"`
	Request             brokerapi.ReservationRequest   `json:"request"`
	SubmittedAt         time.Time                      `json:"submittedAt"`
	BrokerCreatedAt     *time.Time                     `json:"brokerCreatedAt,omitempty"`
	Transitions         []PhaseTransition              `json:"transitions"`
	FinalPhase          string                         `json:"finalPhase"`
	Peered              bool                           `json:"peered"`
	PeeredAt            *time.Time                     `json:"peeredAt,omitempty"`
	PeeringDurationMs   float64                        `json:"peeringDurationMs,omitempty"`
	LastResponse        *brokerapi.ReservationResponse `json:"lastResponse,omitempty"`
	Error               string                         `json:"error,omitempty"`
	HTTPStatus          int                            `json:"httpStatus,omitempty"`
	FailureCategory     string                         `json:"failureCategory,omitempty"`
	Released            bool                           `json:"released"`
	ReleasedAt          *time.Time                     `json:"releasedAt,omitempty"`
	ReleaseError        string                         `json:"releaseError,omitempty"`
}

// Retry budget for rejections the Broker documents as transient.
const (
	maxTransientRetries = 10
	transientBackoff    = 750 * time.Millisecond
)

// reserveAndTrack creates one reservation and polls it to Peered, recording
// every distinct phase. Polling re-POSTs the same X-Reservation-Id, which the
// Broker treats as idempotent and answers with the current state.
func reserveAndTrack(ctx context.Context, broker *testlib.BrokerClient, id string, ng *brokerapi.NodeGroupView,
	poll, timeout time.Duration) *ReservationResult {
	res := &ReservationResult{
		ReservationID:       id,
		RequestedProviderID: ng.ProviderClusterID,
		Request: brokerapi.ReservationRequest{
			ProviderClusterID: ng.ProviderClusterID,
			NodeGroupID:       ng.ID,
			ChunkCount:        1,
			ChunkType:         ng.Type,
		},
		SubmittedAt: time.Now(),
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	retries := 0
	for {
		resp, err := broker.CreateReservation(ctx, id, &res.Request)
		if err != nil {
			var apiErr *agentclient.Error
			if errors.As(err, &apiErr) {
				res.HTTPStatus = apiErr.Status
			}
			retryable := agentclient.IsTooManyRequests(err) || agentclient.IsTransient(err)
			if retryable && retries < maxTransientRetries && ctx.Err() == nil {
				retries++
				if testlib.SleepCtx(ctx, transientBackoff) == nil {
					continue
				}
			}
			res.Error = err.Error()
			res.FailureCategory = failReservationRejected
			if ctx.Err() != nil {
				res.FailureCategory = failReservationTimeout
			}
			return res
		}

		res.LastResponse = resp
		if res.BrokerCreatedAt == nil && !resp.CreatedAt.IsZero() {
			t := resp.CreatedAt.Time
			res.BrokerCreatedAt = &t
		}
		phase := string(resp.Status)
		if n := len(res.Transitions); n == 0 || res.Transitions[n-1].Phase != phase {
			res.Transitions = append(res.Transitions, PhaseTransition{Phase: phase, SeenAt: time.Now(), Message: resp.Message})
			log.Printf("[reservation] %s -> %s %s", id, phase, resp.Message)
		}
		res.FinalPhase = phase

		switch resp.Status {
		case brokerv1alpha1.ReservationPhasePeered:
			now := time.Now()
			res.Peered, res.PeeredAt = true, &now
			res.PeeringDurationMs = float64(now.Sub(res.SubmittedAt).Microseconds()) / 1000
			return res
		case brokerv1alpha1.ReservationPhaseFailed, brokerv1alpha1.ReservationPhaseReleased,
			brokerv1alpha1.ReservationPhaseExpired:
			res.Error = fmt.Sprintf("reservation ended in phase %s: %s", phase, resp.Message)
			res.FailureCategory = failReservationBadPhase
			return res
		}

		if err := testlib.SleepCtx(ctx, poll); err != nil {
			res.Error = fmt.Sprintf("did not reach Peered within %s (last phase %s)", timeout, phase)
			res.FailureCategory = failReservationTimeout
			return res
		}
	}
}

// release frees the reservation and waits until the Broker reports it Released.
// It runs on its own context so a cancelled run still gives capacity back.
func release(broker *testlib.BrokerClient, res *ReservationResult, poll time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := broker.ReleaseAndWait(ctx, res.ReservationID, &res.Request, poll); err != nil {
		res.ReleaseError = err.Error()
		log.Printf("[reservation] release %s: %v", res.ReservationID, err)
		return
	}
	now := time.Now()
	res.Released, res.ReleasedAt = true, &now
	log.Printf("[reservation] %s released", res.ReservationID)
}
