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

// This file collects the read-mostly CloudProvider RPCs (step 10c):
// NodeGroups, NodeGroupForNode, NodeGroupTargetSize,
// NodeGroupTemplateNodeInfo. They each translate CA's request into
// one HTTP call against the Consumer Agent's loopback REST
// (`GET /local/nodegroups` and/or `GET /local/virtual-nodes`) and
// reshape the response into the externalgrpc proto types.

package grpcserver

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/agentclient"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/protos"
)

// NodeGroups returns every node group the broker is advertising for
// this consumer cluster. Mapped 1:1 from
// brokerapi.NodeGroupView{ID, MinSize, MaxSize} onto protos.NodeGroup.
func (s *Server) NodeGroups(ctx context.Context, _ *protos.NodeGroupsRequest) (*protos.NodeGroupsResponse, error) {
	if s.agent == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent client not configured")
	}
	resp, err := s.agent.GetNodeGroups(ctx)
	if err != nil {
		return nil, mapAgentError(err, "GetNodeGroups")
	}
	out := &protos.NodeGroupsResponse{
		NodeGroups: make([]*protos.NodeGroup, 0, len(resp.NodeGroups)),
	}
	for i := range resp.NodeGroups {
		ng := &resp.NodeGroups[i]
		out.NodeGroups = append(out.NodeGroups, &protos.NodeGroup{
			Id:      ng.ID,
			MinSize: ng.MinSize,
			MaxSize: ng.MaxSize,
			Debug: fmt.Sprintf("provider=%s type=%s current=%d max=%d",
				ng.ProviderClusterID, ng.Type, ng.CurrentReserved, ng.MaxSize),
		})
	}
	return out, nil
}

// NodeGroupForNode resolves which group a virtual node belongs to.
// The proto convention is to return NodeGroup{Id: ""} when the node
// is not managed by this provider — CA treats that as "ignore", not
// an error.
func (s *Server) NodeGroupForNode(ctx context.Context, req *protos.NodeGroupForNodeRequest) (*protos.NodeGroupForNodeResponse, error) {
	if req == nil || req.Node == nil || req.Node.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "node is required")
	}
	if s.agent == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent client not configured")
	}
	list, err := s.agent.GetVirtualNodes(ctx)
	if err != nil {
		return nil, mapAgentError(err, "GetVirtualNodes")
	}
	for i := range list.VirtualNodes {
		vn := &list.VirtualNodes[i]
		if vn.Name == req.Node.Name {
			return &protos.NodeGroupForNodeResponse{
				NodeGroup: &protos.NodeGroup{Id: vn.NodeGroupID},
			}, nil
		}
	}
	// Not ours — return an empty NodeGroup per the proto contract.
	return &protos.NodeGroupForNodeResponse{NodeGroup: &protos.NodeGroup{}}, nil
}

// NodeGroupTargetSize returns the group's current target size — the number of
// CHUNKS the Broker has reserved for this consumer on the group's provider
// (NodeGroupView.CurrentReserved), which counts in-flight reservations whose
// virtual node has not materialised yet.
//
// It deliberately does NOT count only the live virtual nodes. A borrowed node
// takes ~100 s to peer; during that window a node-only count reports 0, so CA —
// still seeing the Pending Pods — re-issues NodeGroupIncreaseSize every reconcile
// loop until it hits MaxSize. That over-provisions reservations on any provider
// with more than one chunk (capped at 1 on single-chunk providers, which hid the
// bug) and orphans the surplus reservations on scale-down. Reporting reserved
// chunks — in the same chunk units as MaxSize and the IncreaseSize delta — lets
// CA see its request land immediately and then grow only as real demand requires.
func (s *Server) NodeGroupTargetSize(ctx context.Context, req *protos.NodeGroupTargetSizeRequest) (*protos.NodeGroupTargetSizeResponse, error) {
	if req == nil || req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if s.agent == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent client not configured")
	}
	resp, err := s.agent.GetNodeGroups(ctx)
	if err != nil {
		return nil, mapAgentError(err, "GetNodeGroups")
	}
	for i := range resp.NodeGroups {
		if resp.NodeGroups[i].ID == req.Id {
			return &protos.NodeGroupTargetSizeResponse{TargetSize: resp.NodeGroups[i].CurrentReserved}, nil
		}
	}
	// Group no longer advertised → size 0 (CA treats it as scaled to nothing).
	return &protos.NodeGroupTargetSizeResponse{TargetSize: 0}, nil
}

// NodeGroupTemplateNodeInfo builds a synthetic v1.Node that mirrors
// the shape every real virtual node in this group would carry —
// allocatable resources, labels, taints, topology — and returns it
// proto-serialised in NodeBytes. CA feeds those bytes through its
// scheduler simulator to decide whether the group can accommodate
// the pending pod set.
func (s *Server) NodeGroupTemplateNodeInfo(ctx context.Context, req *protos.NodeGroupTemplateNodeInfoRequest) (*protos.NodeGroupTemplateNodeInfoResponse, error) {
	if req == nil || req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if s.agent == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent client not configured")
	}
	resp, err := s.agent.GetNodeGroups(ctx)
	if err != nil {
		return nil, mapAgentError(err, "GetNodeGroups")
	}
	for i := range resp.NodeGroups {
		ng := &resp.NodeGroups[i]
		if ng.ID != req.Id {
			continue
		}
		node := buildNodeTemplate(ng)
		if node == nil {
			return nil, status.Errorf(codes.Internal,
				"buildNodeTemplate returned nil for node group %q", ng.ID)
		}
		// CA's externalgrpc provider in cluster-autoscaler 1.32 / 1.33 /
		// 1.34 expects the response to carry a structured Node (proto
		// field 1, name `nodeInfo`), not a serialized-bytes blob.
		// Returning a nil NodeInfo trips a known crash deep in CA's
		// nodeinfosprovider (panics dereferencing the resulting nil
		// framework.NodeInfo), so this code path must always set the
		// field.
		return &protos.NodeGroupTemplateNodeInfoResponse{NodeInfo: node}, nil
	}
	return nil, status.Errorf(codes.NotFound, "node group %q not found", req.Id)
}

// mapAgentError translates an agentclient typed error into the gRPC
// status code CA understands. Substep 10d's mutating RPCs share this
// helper so the entire externalgrpc surface uses one consistent
// mapping — without it, CA would see opaque internal errors for what
// are really "broker said 412".
func mapAgentError(err error, rpc string) error {
	switch {
	case agentclient.IsBadRequest(err):
		return status.Errorf(codes.InvalidArgument, "%s: %v", rpc, err)
	case agentclient.IsUnauthenticated(err):
		return status.Errorf(codes.Unauthenticated, "%s: %v", rpc, err)
	case agentclient.IsForbidden(err):
		return status.Errorf(codes.PermissionDenied, "%s: %v", rpc, err)
	case agentclient.IsNotFound(err):
		return status.Errorf(codes.NotFound, "%s: %v", rpc, err)
	case agentclient.IsConflict(err):
		return status.Errorf(codes.AlreadyExists, "%s: %v", rpc, err)
	case agentclient.IsPreconditionFailed(err):
		return status.Errorf(codes.FailedPrecondition, "%s: %v", rpc, err)
	case agentclient.IsTooManyRequests(err):
		return status.Errorf(codes.ResourceExhausted, "%s: %v", rpc, err)
	case agentclient.IsTransient(err):
		return status.Errorf(codes.Unavailable, "%s: %v", rpc, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", rpc, err)
	}
}
