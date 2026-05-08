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

// This file collects the typed wrappers for every Broker REST endpoint an
// agent calls (docs/design.md §7.3). Each method is a thin shell over
// (*Client).do — the request layer (request.go) already handles header
// injection, retries, and error categorisation; this file only chooses
// the path, method, request type, and response type for each endpoint.

package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// PostAdvertisement publishes the provider's resource snapshot to the
// Broker (POST /api/v1/advertisements, §7.3.1). Called every 30 s by the
// Provider Agent; the response piggy-backs any pending ProviderInstruction
// the broker queued for this provider since the last call. Treated as
// idempotent: retrying after a transient failure converges on the same
// upserted ClusterAdvertisement.
func (c *Client) PostAdvertisement(
	ctx context.Context, req *brokerapi.AdvertisementRequest,
) (*brokerapi.AdvertisementResponse, error) {
	if req == nil {
		return nil, errors.New("client: PostAdvertisement: req is nil")
	}
	out := &brokerapi.AdvertisementResponse{}
	if err := c.do(ctx, http.MethodPost, "/api/v1/advertisements", req, out, requestOptions{
		idempotent: true,
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// PostHeartbeat tells the Broker the consumer is alive and refreshes the
// in-memory ConsumerRegistry entry the broker uses when minting reservation
// CRs (POST /api/v1/heartbeat, §7.3.3). Called every 15 s by the Consumer
// Agent. Idempotent; retries are safe because the registry is keyed by
// cluster ID and the broker overwrites prior entries.
func (c *Client) PostHeartbeat(
	ctx context.Context, req *brokerapi.HeartbeatRequest,
) (*brokerapi.HeartbeatResponse, error) {
	if req == nil {
		return nil, errors.New("client: PostHeartbeat: req is nil")
	}
	out := &brokerapi.HeartbeatResponse{}
	if err := c.do(ctx, http.MethodPost, "/api/v1/heartbeat", req, out, requestOptions{
		idempotent: true,
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// GetNodeGroups returns the broker's snapshot of available node groups
// (GET /api/v1/nodegroups, §7.3.4) — one entry per (provider × chunk
// type). Consumed by the Consumer Agent's loopback REST API which proxies
// it to the local gRPC server so Cluster Autoscaler can populate the
// node-group list it scales over.
func (c *Client) GetNodeGroups(
	ctx context.Context,
) (*brokerapi.NodeGroupListResponse, error) {
	out := &brokerapi.NodeGroupListResponse{}
	if err := c.do(ctx, http.MethodGet, "/api/v1/nodegroups", nil, out, requestOptions{
		idempotent: true,
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// PostReservation requests a new reservation against the named provider
// (POST /api/v1/reservations, §7.3.5). reservationID is required and is
// sent as the X-Reservation-Id header — that is what gives the broker its
// idempotency: the broker derives the Reservation CR's name from this
// header, so a retry of the same call returns the existing CR's state
// rather than creating a duplicate.
//
// HTTP 201 on first creation, HTTP 200 on idempotent hit. Both are
// surfaced as a non-error return; callers branch on
// resp.Status to pick up the current Reservation phase.
func (c *Client) PostReservation(
	ctx context.Context, reservationID string, req *brokerapi.ReservationRequest,
) (*brokerapi.ReservationResponse, error) {
	if reservationID == "" {
		return nil, errors.New("client: PostReservation: reservationID is required (idempotency key)")
	}
	if req == nil {
		return nil, errors.New("client: PostReservation: req is nil")
	}
	out := &brokerapi.ReservationResponse{}
	if err := c.do(ctx, http.MethodPost, "/api/v1/reservations", req, out, requestOptions{
		reservationID: reservationID,
		idempotent:    true,
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteReservation requests release of an existing reservation
// (DELETE /api/v1/reservations/{id}, §7.3.9). The current implementation
// always releases all chunks (`v1 limitation` per CLAUDE.md / step 6); the
// `?chunks=N` query parameter is reserved for the v2 partial-release
// feature and is intentionally not exposed on this signature.
func (c *Client) DeleteReservation(
	ctx context.Context, reservationID string,
) (*brokerapi.ReleaseResponse, error) {
	if reservationID == "" {
		return nil, errors.New("client: DeleteReservation: reservationID is required")
	}
	path := "/api/v1/reservations/" + url.PathEscape(reservationID)
	out := &brokerapi.ReleaseResponse{}
	if err := c.do(ctx, http.MethodDelete, path, nil, out, requestOptions{
		idempotent: true,
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// GetInstructions polls the broker for outstanding instructions targeted
// at this caller (GET /api/v1/instructions, §7.3.6). Called every
// --poll-interval (default 5 s) by both the consumer- and provider-role
// agents; the broker returns ProviderInstructions when the caller is a
// provider and ReservationInstructions when the caller is a consumer.
//
// The response is the canonical source of truth: any instruction with
// Status.Enforced=true on the broker side is filtered out, so a steady
// stream of empty Instructions slices is the normal idle state.
func (c *Client) GetInstructions(
	ctx context.Context,
) (*brokerapi.InstructionsResponse, error) {
	out := &brokerapi.InstructionsResponse{}
	if err := c.do(ctx, http.MethodGet, "/api/v1/instructions", nil, out, requestOptions{
		idempotent: true,
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// PostInstructionResult reports the outcome of a single instruction
// (POST /api/v1/instructions/{id}/result, §7.3.7). The broker is
// idempotent at the CR level: it short-circuits when Status.Enforced is
// already true, so retrying after a transient failure is always safe.
func (c *Client) PostInstructionResult(
	ctx context.Context, instructionID string, req *brokerapi.InstructionResultRequest,
) error {
	if instructionID == "" {
		return errors.New("client: PostInstructionResult: instructionID is required")
	}
	if req == nil {
		return errors.New("client: PostInstructionResult: req is nil")
	}
	path := fmt.Sprintf("/api/v1/instructions/%s/result", url.PathEscape(instructionID))
	out := &brokerapi.InstructionResultResponse{}
	return c.do(ctx, http.MethodPost, path, req, out, requestOptions{
		idempotent: true,
	})
}
