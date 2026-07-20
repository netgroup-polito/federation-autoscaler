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

// This file collects the mutating CloudProvider RPCs (step 10d):
// NodeGroupIncreaseSize, NodeGroupDeleteNodes,
// NodeGroupDecreaseTargetSize. Each translates CA's request into one
// or more `POST /local/reservations` / `DELETE /local/reservations/
// {id}` calls against the Consumer Agent's loopback REST and surfaces
// broker error categories as the matching gRPC code via
// mapAgentError (shared with the read-mostly RPCs in 10c).

package grpcserver

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/protos"
)

// NodeGroupIncreaseSize translates CA's "grow group <id> by <delta>"
// into a POST /local/reservations against the node group's provider.
// CA may call this multiple times for the same group as it iterates
// its scaling plan; each call is a separate Reservation so the
// X-Reservation-Id is a freshly-minted UUID per call. (Idempotency
// inside a single RPC invocation is handled by the agentclient layer;
// stronger cross-call idempotency would require CA to thread a stable
// key, which the proto doesn't allow.)
func (s *Server) NodeGroupIncreaseSize(ctx context.Context, req *protos.NodeGroupIncreaseSizeRequest) (*protos.NodeGroupIncreaseSizeResponse, error) {
	if req == nil || req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if req.Delta <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "delta must be > 0, got %d", req.Delta)
	}
	if s.agent == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent client not configured")
	}

	group, err := s.findNodeGroup(ctx, req.Id)
	if err != nil {
		return nil, err
	}

	// One Reservation per chunk, NOT one Reservation of Delta chunks.
	//
	// A Reservation materialises exactly one Liqo ResourceSlice, hence exactly
	// one borrowed node (the slice name becomes the node name). A single
	// Reservation carrying N chunks would therefore be billed N and reported to
	// CA as N via NodeGroupTargetSize while delivering one node — CA would keep
	// seeing its Pods pending against capacity that does not exist. Fanning out
	// keeps the ledger honest by construction: N reservations, N slices, N
	// nodes, and releasing one node releases exactly one chunk.
	ids := make([]string, 0, req.Delta)
	for range req.Delta {
		reservationID := "res-" + uuid.NewString()
		resReq := &brokerapi.ReservationRequest{
			ProviderClusterID: group.ProviderClusterID,
			NodeGroupID:       group.ID,
			ChunkCount:        1,
			ChunkType:         group.Type,
		}
		if _, err := s.agent.PostReservation(ctx, reservationID, resReq); err != nil {
			// Partial success is safe to surface as an error: the reservations
			// already placed stand on their own (each is independently
			// releasable), and CA retries with a delta recomputed from the
			// TargetSize those reservations now report.
			if len(ids) > 0 {
				s.log.V(1).Info("scale-up partially applied before failing",
					"nodeGroupId", req.Id, "requested", req.Delta, "placed", len(ids))
			}
			return nil, mapAgentError(err, "PostReservation")
		}
		ids = append(ids, reservationID)
	}
	s.log.V(1).Info("scaled up node group",
		"nodeGroupId", req.Id,
		"delta", req.Delta,
		"reservationIds", ids)
	return &protos.NodeGroupIncreaseSizeResponse{}, nil
}

// NodeGroupDeleteNodes maps each requested virtual-node name to its
// underlying Reservation (via GET /local/virtual-nodes) and issues
// one DELETE per distinct reservation. Since a Reservation is exactly one
// chunk and therefore exactly one node, deleting a node releases precisely
// that node's chunk — there is no partial-release case to defer, and sibling
// nodes borrowed from the same provider are unaffected (the broker ref-counts
// the shared peering; see handleUnpeering's LastChunk).
//
// The RPC is best-effort across nodes: if half the deletions fail
// after the other half succeeded, the first error wins. CA retries
// the whole call on its next loop; the broker's CRD-backed dedup
// (same name → same CR) keeps re-deletions safe.
func (s *Server) NodeGroupDeleteNodes(ctx context.Context, req *protos.NodeGroupDeleteNodesRequest) (*protos.NodeGroupDeleteNodesResponse, error) {
	if req == nil || req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if len(req.Nodes) == 0 {
		return nil, status.Error(codes.InvalidArgument, "nodes is required")
	}
	if s.agent == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent client not configured")
	}

	list, err := s.agent.GetVirtualNodes(ctx)
	if err != nil {
		return nil, mapAgentError(err, "GetVirtualNodes")
	}
	byName := make(map[string]string, len(list.VirtualNodes))
	for i := range list.VirtualNodes {
		vn := &list.VirtualNodes[i]
		byName[vn.Name] = vn.ReservationID
	}

	releasing := map[string]struct{}{}
	for _, n := range req.Nodes {
		if n == nil || n.Name == "" {
			return nil, status.Error(codes.InvalidArgument, "node name is required")
		}
		resID, ok := byName[n.Name]
		if !ok {
			return nil, status.Errorf(codes.NotFound,
				"node %q is not known to this provider", n.Name)
		}
		if resID == "" {
			return nil, status.Errorf(codes.FailedPrecondition,
				"node %q has no reservation id", n.Name)
		}
		releasing[resID] = struct{}{}
	}

	for resID := range releasing {
		if _, err := s.agent.DeleteReservation(ctx, resID); err != nil {
			return nil, mapAgentError(err, fmt.Sprintf("DeleteReservation(%s)", resID))
		}
		s.log.V(1).Info("released reservation",
			"nodeGroupId", req.Id, "reservationId", resID)
	}
	return &protos.NodeGroupDeleteNodesResponse{}, nil
}

// NodeGroupDecreaseTargetSize is invoked by CA when it believes the
// cloud provider has provisioned more nodes than have registered
// with the kube-apiserver — i.e. there are "phantom" nodes to garbage
// collect. The federation-autoscaler model has no such phantoms: a
// reservation either has VirtualNodeState CRs reflecting real Liqo
// virtual nodes, or it doesn't. We therefore make this a no-op
// success, matching the convention several upstream cloud providers
// follow for the same reason. Negative or zero deltas are rejected
// to keep CA's contract honest.
func (s *Server) NodeGroupDecreaseTargetSize(_ context.Context, req *protos.NodeGroupDecreaseTargetSizeRequest) (*protos.NodeGroupDecreaseTargetSizeResponse, error) {
	if req == nil || req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if req.Delta >= 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"delta must be < 0 for DecreaseTargetSize, got %d", req.Delta)
	}
	s.log.V(1).Info("decrease-target-size is a no-op in v1",
		"nodeGroupId", req.Id, "delta", req.Delta)
	return &protos.NodeGroupDecreaseTargetSizeResponse{}, nil
}

// findNodeGroup is a tiny helper for the mutating RPCs: it fetches
// the broker's view of node groups and returns the one matching id,
// with a NotFound status when absent.
func (s *Server) findNodeGroup(ctx context.Context, id string) (*brokerapi.NodeGroupView, error) {
	resp, err := s.agent.GetNodeGroups(ctx)
	if err != nil {
		return nil, mapAgentError(err, "GetNodeGroups")
	}
	for i := range resp.NodeGroups {
		if resp.NodeGroups[i].ID == id {
			return &resp.NodeGroups[i], nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "node group %q not found", id)
}
