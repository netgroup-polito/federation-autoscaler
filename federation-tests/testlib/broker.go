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
	"sort"
	"strings"
	"time"

	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// BrokerClient wraps the agent mTLS client with helpers specific to the
// comparative test harness.
type BrokerClient struct {
	Raw       *agentclient.Client
	ClusterID string
}

// NewBrokerClientFromIdentity constructs a BrokerClient from an Identity.
func NewBrokerClientFromIdentity(brokerURL, serverName string, id Identity, caFile string) (*BrokerClient, error) {
	c, err := NewBrokerClient(brokerURL, serverName, id, caFile)
	if err != nil {
		return nil, err
	}
	return &BrokerClient{Raw: c, ClusterID: id.ClusterID}, nil
}

// CheckReachable verifies the Broker is reachable by fetching node groups.
func (bc *BrokerClient) CheckReachable(ctx context.Context) error {
	_, err := bc.Raw.GetNodeGroups(ctx)
	return err
}

// GetNodeGroups returns the Broker's current view of node groups.
func (bc *BrokerClient) GetNodeGroups(ctx context.Context) (*brokerapi.NodeGroupListResponse, error) {
	return bc.Raw.GetNodeGroups(ctx)
}

// FederationCapacity is how much of the federation is occupied, as the Broker
// itself sees it. The comparative harnesses use it to assert that Phase B
// starts on the same federation Phase A started on.
//
// The need is not hypothetical. When ReleaseAndWait fails mid-phase the harness
// logs the error and drops the reservation from its own bookkeeping, but the
// Broker keeps it — its ExpiresAt is 24h out — so the chunk stays occupied for
// the rest of the run. Phase A runs the Random policy and therefore switches
// provider on nearly every iteration, giving it an order of magnitude more
// releases than Phase B and an order of magnitude more chances to leak one.
// Left unchecked, Phase A gets the whole federation and Phase B gets whatever
// survived, which quietly stops the two phases from being a paired comparison.
type FederationCapacity struct {
	// TotalReserved sums CurrentReserved over every node group the Broker
	// reports. Masked losers stay in the list, so this covers the federation
	// and not just the caller's current winner.
	TotalReserved int32
	// ReservedBy maps node-group ID to its reserved chunks, for the groups
	// holding at least one. Empty on a free federation; it exists so a failure
	// can name the culprits instead of only reporting a total.
	ReservedBy map[string]int32
	// NodeGroupIDs is every node-group ID seen, sorted. Compared alongside the
	// total because a provider that has gone stale (Status.Available false,
	// 90s after its last advertisement) drops out of the list entirely — its
	// reserved chunks would vanish from the sum and read as "all clear".
	NodeGroupIDs []string
}

// FederationSettleTimeout is how long a phase transition waits for released
// chunks to be credited back before calling the shortfall a leak. Generous
// against a reconcile loop on purpose: the cost of waiting too long is two
// minutes, the cost of waiting too little is aborting a good run.
const FederationSettleTimeout = 2 * time.Minute

// ReadFederationCapacity snapshots the Broker's current view.
func ReadFederationCapacity(ctx context.Context, bc *BrokerClient) (FederationCapacity, error) {
	resp, err := bc.GetNodeGroups(ctx)
	if err != nil {
		return FederationCapacity{}, fmt.Errorf("read federation capacity: %w", err)
	}
	snap := FederationCapacity{ReservedBy: make(map[string]int32)}
	for _, ng := range resp.NodeGroups {
		snap.TotalReserved += ng.CurrentReserved
		if ng.CurrentReserved > 0 {
			snap.ReservedBy[ng.ID] = ng.CurrentReserved
		}
		snap.NodeGroupIDs = append(snap.NodeGroupIDs, ng.ID)
	}
	sort.Strings(snap.NodeGroupIDs)
	return snap, nil
}

// Matches reports whether c still accounts for everything want did: the same
// total reserved and no node group from want missing out of c.
//
// A node group in c that was NOT in want is deliberately allowed. That case is
// a provider that registered a moment after the baseline snapshot, or one that
// went briefly stale (Status.Available false, up to 90s after its last
// advertisement) and came back -- extra capacity showing up, not capacity
// gone. Only a MISSING node group is treated as suspect: a stale provider
// drops out of the Broker's list taking its ReservedChunks out of the sum
// with it, which is exactly the case ReadFederationCapacity's doc comment
// warns can make a real leak look clean by making the total add up on its
// own. Requiring exact equality of the two ID sets would treat that harmless
// "arrived late" case as a leak too and abort a perfectly good run.
func (c FederationCapacity) Matches(want FederationCapacity) bool {
	if c.TotalReserved != want.TotalReserved {
		return false
	}
	have := make(map[string]bool, len(c.NodeGroupIDs))
	for _, id := range c.NodeGroupIDs {
		have[id] = true
	}
	for _, id := range want.NodeGroupIDs {
		if !have[id] {
			return false
		}
	}
	return true
}

// Diff describes how c departs from want, for an error message.
func (c FederationCapacity) Diff(want FederationCapacity) string {
	var parts []string
	if c.TotalReserved != want.TotalReserved {
		var held []string
		for _, id := range c.NodeGroupIDs {
			if n, ok := c.ReservedBy[id]; ok {
				held = append(held, fmt.Sprintf("%s=%d", id, n))
			}
		}
		msg := fmt.Sprintf("reserved chunks %d, want %d", c.TotalReserved, want.TotalReserved)
		if len(held) > 0 {
			msg += " (still held: " + strings.Join(held, ", ") + ")"
		}
		parts = append(parts, msg)
	}
	// Missing groups are the actionable half of a mismatch (see Matches);
	// extras are named too, but only as context -- they never caused this
	// Diff to be printed in the first place.
	missing := missingFrom(want.NodeGroupIDs, c.NodeGroupIDs)
	if len(missing) > 0 {
		parts = append(parts, "node groups gone: "+strings.Join(missing, ", "))
	}
	if extra := missingFrom(c.NodeGroupIDs, want.NodeGroupIDs); len(extra) > 0 {
		parts = append(parts, "node groups appeared (not a problem on their own): "+strings.Join(extra, ", "))
	}
	if len(parts) == 0 {
		return "no difference"
	}
	return strings.Join(parts, "; ")
}

func missingFrom(want, got []string) []string {
	have := make(map[string]bool, len(got))
	for _, id := range got {
		have[id] = true
	}
	var out []string
	for _, id := range want {
		if !have[id] {
			out = append(out, id)
		}
	}
	return out
}

// WaitForFederationCapacity polls until the federation is back to want, and
// fails if it never gets there.
//
// Polling rather than checking once is required, not defensive: a released
// chunk is credited back asynchronously by the reservation controller, so an
// immediate read right after a phase ends routinely still shows it held. What
// distinguishes controller lag from a real leak is only whether it clears, so
// the timeout is the whole measurement — give it a budget comfortably longer
// than a reconcile, and treat anything still outstanding afterwards as lost.
func WaitForFederationCapacity(ctx context.Context, bc *BrokerClient, want FederationCapacity, poll, timeout time.Duration) (FederationCapacity, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	var last FederationCapacity
	for {
		got, err := ReadFederationCapacity(ctx, bc)
		if err != nil {
			return last, err
		}
		last = got
		if got.Matches(want) {
			return got, nil
		}
		if !time.Now().Before(deadline) {
			return got, fmt.Errorf("federation did not return to its starting capacity within %s: %s", timeout, got.Diff(want))
		}
		select {
		case <-ctx.Done():
			return got, ctx.Err()
		case <-ticker.C:
		}
	}
}

// CreateReservation creates a reservation against the specified provider.
func (bc *BrokerClient) CreateReservation(ctx context.Context, reservationID string, req *brokerapi.ReservationRequest) (*brokerapi.ReservationResponse, error) {
	return bc.Raw.PostReservation(ctx, reservationID, req)
}

// DeleteReservation releases a reservation.
func (bc *BrokerClient) DeleteReservation(ctx context.Context, reservationID string) (*brokerapi.ReleaseResponse, error) {
	return bc.Raw.DeleteReservation(ctx, reservationID)
}

// WaitForPhase polls the reservation (via idempotent re-submission) until it
// reaches the target phase or the context expires. The Broker returns the
// current state on idempotent hits (same X-Reservation-Id).
func (bc *BrokerClient) WaitForPhase(ctx context.Context, reservationID string, req *brokerapi.ReservationRequest, targetPhase string, pollInterval time.Duration) (*brokerapi.ReservationResponse, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		resp, err := bc.Raw.PostReservation(ctx, reservationID, req)
		if err != nil {
			return nil, fmt.Errorf("poll reservation %s: %w", reservationID, err)
		}
		if string(resp.Status) == targetPhase {
			return resp, nil
		}
		if isTerminalPhase(string(resp.Status)) {
			return resp, fmt.Errorf("reservation %s reached terminal phase %s (wanted %s)", reservationID, resp.Status, targetPhase)
		}
		select {
		case <-ctx.Done():
			return resp, fmt.Errorf("timed out waiting for reservation %s to reach %s (current: %s)", reservationID, targetPhase, resp.Status)
		case <-ticker.C:
		}
	}
}

func isTerminalPhase(phase string) bool {
	switch phase {
	case "Failed", "Released", "Expired":
		return true
	}
	return false
}
