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

// This file collects the pricing + lifecycle CloudProvider RPCs
// (step 10e): PricingNodePrice, PricingPodPrice, GPULabel,
// GetAvailableGPUTypes, Refresh, Cleanup, NodeGroupNodes. The
// optional NodeGroupGetOptions stays Unimplemented — CA tolerates
// providers that opt out of per-group autoscaling overrides.

package grpcserver

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/protos"
)

// GPULabelKey is the conventional node label set by GPU device
// plugins (the NVIDIA k8s device plugin uses it). CA's GPU-aware
// scaling code reads this label to decide whether a node has a GPU.
const GPULabelKey = "nvidia.com/gpu"

// PricingNodePrice returns the theoretical minimum cost of running
// one node in the requested NodeGroup for the [start, end] window.
// We resolve the node → its NodeGroup → the broker's per-chunk Cost,
// then scale by the window duration in hours. Missing Cost on the
// node group → 0 (free-tier providers exist; not an error).
func (s *Server) PricingNodePrice(ctx context.Context, req *protos.PricingNodePriceRequest) (*protos.PricingNodePriceResponse, error) {
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
	var groupID string
	for i := range list.VirtualNodes {
		if list.VirtualNodes[i].Name == req.Node.Name {
			groupID = list.VirtualNodes[i].NodeGroupID
			break
		}
	}
	if groupID == "" {
		// Unknown node — return 0 so CA's price expander treats it as
		// neutral rather than failing the whole loop.
		return &protos.PricingNodePriceResponse{Price: 0}, nil
	}

	group, err := s.findNodeGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	// v1.32 / 1.33 / 1.34 externalgrpc proto uses StartTime/EndTime
	// (metav1.Time), not StartTimestamp/EndTimestamp (timestamppb).
	hours := windowHours(timeFromMetaV1(req.StartTime), timeFromMetaV1(req.EndTime))
	return &protos.PricingNodePriceResponse{Price: chunkCostFloat(group) * hours}, nil
}

// timeFromMetaV1 dereferences a *metav1.Time, returning zero time for
// nil. Used to bridge the v1.32 proto's metav1.Time field types into
// time.Time the chunk-pricing math expects.
func timeFromMetaV1(t *metav1.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}

// PricingPodPrice is intentionally always zero in v1: the broker
// doesn't surface pod-level pricing (chunks are the unit), and CA's
// price expander only consults pod price relatively, so any constant
// (including 0) keeps node-group preference deterministic.
func (s *Server) PricingPodPrice(_ context.Context, req *protos.PricingPodPriceRequest) (*protos.PricingPodPriceResponse, error) {
	if req == nil || req.Pod == nil {
		return nil, status.Error(codes.InvalidArgument, "pod is required")
	}
	return &protos.PricingPodPriceResponse{Price: 0}, nil
}

// GPULabel tells CA which node label marks a node as GPU-bearing.
// We hard-code the NVIDIA-device-plugin convention here. If a
// provider later wants a different label, the chunk-config ConfigMap
// is the right place to surface it; for v1 this matches what
// Multi-Cluster-Autoscaler and most upstream providers do.
func (s *Server) GPULabel(context.Context, *protos.GPULabelRequest) (*protos.GPULabelResponse, error) {
	return &protos.GPULabelResponse{Label: GPULabelKey}, nil
}

// GetAvailableGPUTypes derives the GPU type catalogue from the
// node-group advertisement: every NodeGroup whose ChunkType is GPU
// contributes its provider as a known GPU type. The proto's value
// is an opaque *anypb.Any; CA only uses the keys, so we send empty
// anypbs.
func (s *Server) GetAvailableGPUTypes(ctx context.Context, _ *protos.GetAvailableGPUTypesRequest) (*protos.GetAvailableGPUTypesResponse, error) {
	if s.agent == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent client not configured")
	}
	resp, err := s.agent.GetNodeGroups(ctx)
	if err != nil {
		return nil, mapAgentError(err, "GetNodeGroups")
	}
	out := &protos.GetAvailableGPUTypesResponse{
		GpuTypes: map[string]*anypb.Any{},
	}
	for i := range resp.NodeGroups {
		ng := &resp.NodeGroups[i]
		if ng.Type != brokerv1alpha1.ChunkTypeGPU {
			continue
		}
		// Key by provider so multiple GPU groups from one provider
		// collapse to a single entry — CA's map enumerates distinct
		// types, not distinct groups.
		out.GpuTypes[ng.ProviderClusterID] = &anypb.Any{}
	}
	return out, nil
}

// Refresh is a no-op: the gRPC server holds no state of its own —
// every RPC fetches fresh from the agent's loopback REST. CA invokes
// Refresh on each scaling iteration to give providers a chance to
// rebuild caches; we have none, so we acknowledge and move on.
func (s *Server) Refresh(context.Context, *protos.RefreshRequest) (*protos.RefreshResponse, error) {
	s.log.V(1).Info("Refresh: no-op (stateless gRPC server)")
	return &protos.RefreshResponse{}, nil
}

// Cleanup is a no-op too: we own no goroutines or caches to tear
// down here; cmd/grpc-server's signal handler cancels the embedded
// ctx and the gRPC server's GracefulStop runs from main(). CA's
// Cleanup contract permits this.
func (s *Server) Cleanup(context.Context, *protos.CleanupRequest) (*protos.CleanupResponse, error) {
	s.log.V(1).Info("Cleanup: no-op (server lifecycle is driven by ctx)")
	return &protos.CleanupResponse{}, nil
}

// NodeGroupNodes lists the nodes in the requested group that have a
// real v1.Node on the consumer cluster, projected to the proto Instance
// shape. CA uses this to map its node-by-node view (status, errors)
// back to specific scale-up calls and to target scale-downs.
//
// Only VirtualNodeStates whose Status.VirtualNodeName is populated are
// reported — i.e. nodes Liqo has actually materialised and registered.
// An in-flight chunk (VirtualNodeName still empty) is deliberately
// omitted: reporting its CR-name placeholder would make CA believe a
// node exists in the cloud that never registers in the cluster, which
// CA logs as "unregistered nodes present" and which wedges
// scaleDownInCooldown=true so NodeGroupDeleteNodes is never called —
// automatic scale-down then becomes impossible.
//
// The Instance Id MUST be the node's Spec.ProviderID, NOT its name: CA's
// clusterstate matches cloud instances to registered nodes by inserting
// each node's ProviderID into a set and flagging any instance Id absent
// from it (clusterstate.go getNotRegisteredNodes →
// registered.Insert(node.Spec.ProviderID)). Liqo sets virtual-node
// providerIDs to `liqo://…`, so returning the bare node name here makes
// every node look perpetually "unregistered" and CA churns phantom
// scale-up/scale-down reservations forever. ProviderID is published by
// the VirtualNodeStateReconciler once the node is Ready; a Ready node
// always has it, so the VirtualNodeName!="" guard already covers it.
func (s *Server) NodeGroupNodes(ctx context.Context, req *protos.NodeGroupNodesRequest) (*protos.NodeGroupNodesResponse, error) {
	if req == nil || req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if s.agent == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent client not configured")
	}

	list, err := s.agent.GetVirtualNodes(ctx)
	if err != nil {
		return nil, mapAgentError(err, "GetVirtualNodes")
	}
	out := &protos.NodeGroupNodesResponse{}
	for i := range list.VirtualNodes {
		vn := &list.VirtualNodes[i]
		if vn.NodeGroupID != req.Id {
			continue
		}
		// Skip chunks whose v1.Node hasn't registered yet, or whose
		// providerID hasn't been published — see the "unregistered nodes"
		// rationale above. A Ready node always carries both.
		if vn.VirtualNodeName == "" || vn.ProviderID == "" {
			continue
		}
		out.Instances = append(out.Instances, &protos.Instance{
			Id:     vn.ProviderID,
			Status: instanceStatusFromVNS(vn),
		})
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// windowHours converts a [start, end] timestamp pair into a positive
// floating-point hour count. Zero / inverted / unset windows fall
// back to one hour so callers that omit timestamps still get a
// sensible non-zero price.
func windowHours(start, end time.Time) float64 {
	dur := end.Sub(start)
	if dur <= 0 {
		return 1.0
	}
	return dur.Hours()
}

// chunkCostFloat converts a NodeGroupView's Cost (*resource.Quantity)
// into a float64. Returns 0 when Cost is unset — many providers
// don't price their tiers, and CA's price expander treats 0 as
// neutral.
func chunkCostFloat(group *brokerapi.NodeGroupView) float64 {
	if group == nil || group.Cost == nil {
		return 0
	}
	// Quantity.AsApproximateFloat64 is exact for the precisions
	// chunk-config uses (whole / fractional values in default
	// resource.Format). The "approximate" qualifier only matters at
	// extreme magnitudes (~1e18) we never produce for cost.
	return group.Cost.AsApproximateFloat64()
}

// instanceStatusFromVNS maps a VirtualNodeView's Phase onto the
// proto InstanceState enum CA understands. A Failed phase becomes
// an Unspecified state plus a populated ErrorInfo so CA's planner
// can attribute the failure.
func instanceStatusFromVNS(vn *localapi.VirtualNodeView) *protos.InstanceStatus {
	switch vn.Phase {
	case autoscalingv1alpha1.VirtualNodeStatePhaseRunning:
		return &protos.InstanceStatus{InstanceState: protos.InstanceStatus_instanceRunning}
	case autoscalingv1alpha1.VirtualNodeStatePhaseCreating:
		return &protos.InstanceStatus{InstanceState: protos.InstanceStatus_instanceCreating}
	case autoscalingv1alpha1.VirtualNodeStatePhaseDeleting:
		return &protos.InstanceStatus{InstanceState: protos.InstanceStatus_instanceDeleting}
	case autoscalingv1alpha1.VirtualNodeStatePhaseFailed:
		return &protos.InstanceStatus{
			InstanceState: protos.InstanceStatus_unspecified,
			ErrorInfo: &protos.InstanceErrorInfo{
				ErrorCode:    "FederationAutoscalerFailed",
				ErrorMessage: "virtual node materialisation failed",
			},
		}
	default:
		return &protos.InstanceStatus{InstanceState: protos.InstanceStatus_unspecified}
	}
}
