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

package grpcserver

import (
	"context"
	"math"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/protos"
)

// quantityPtr returns a pointer to a resource.Quantity parsed from s.
func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// -----------------------------------------------------------------------------
// PricingNodePrice
// -----------------------------------------------------------------------------

func TestPricingNodePrice_ScalesByHours(t *testing.T) {
	fb := &fakeLocalAPI{
		nodeGroups: &brokerapi.NodeGroupListResponse{
			NodeGroups: []brokerapi.NodeGroupView{
				{
					ID: "p1/standard", ProviderClusterID: "p1",
					Type: brokerv1alpha1.ChunkTypeStandard,
					Cost: quantityPtr("100m"), // 0.1 per chunk-hour
				},
			},
		},
		virtualNodes: &localapi.VirtualNodeListResponse{
			VirtualNodes: []localapi.VirtualNodeView{
				{Name: "vn-1", NodeGroupID: "p1/standard", Phase: autoscalingv1alpha1.VirtualNodeStatePhaseRunning},
			},
		},
	}
	client := newRunningServer(t, fb)

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)

	resp, err := client.PricingNodePrice(context.Background(), &protos.PricingNodePriceRequest{
		Node:      &protos.ExternalGrpcNode{Name: "vn-1"},
		StartTime: &metav1.Time{Time: start},
		EndTime:   &metav1.Time{Time: end},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := 0.1 * 2 // 0.1 / hour * 2 hours
	if math.Abs(resp.Price-want) > 1e-9 {
		t.Errorf("price: want %f, got %f", want, resp.Price)
	}
}

func TestPricingNodePrice_UnknownNode_Zero(t *testing.T) {
	fb := &fakeLocalAPI{
		virtualNodes: &localapi.VirtualNodeListResponse{},
	}
	client := newRunningServer(t, fb)
	now := time.Now()
	resp, err := client.PricingNodePrice(context.Background(), &protos.PricingNodePriceRequest{
		Node:      &protos.ExternalGrpcNode{Name: "unknown"},
		StartTime: &metav1.Time{Time: now},
		EndTime:   &metav1.Time{Time: now.Add(time.Hour)},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Price != 0 {
		t.Errorf("price: want 0 for unknown node, got %f", resp.Price)
	}
}

func TestPricingNodePrice_MissingCost_Zero(t *testing.T) {
	fb := &fakeLocalAPI{
		nodeGroups: &brokerapi.NodeGroupListResponse{
			NodeGroups: []brokerapi.NodeGroupView{
				{ID: "p1/standard", ProviderClusterID: "p1", Type: brokerv1alpha1.ChunkTypeStandard},
			},
		},
		virtualNodes: &localapi.VirtualNodeListResponse{
			VirtualNodes: []localapi.VirtualNodeView{{Name: "vn-1", NodeGroupID: "p1/standard"}},
		},
	}
	client := newRunningServer(t, fb)
	now := time.Now()
	resp, err := client.PricingNodePrice(context.Background(), &protos.PricingNodePriceRequest{
		Node:      &protos.ExternalGrpcNode{Name: "vn-1"},
		StartTime: &metav1.Time{Time: now},
		EndTime:   &metav1.Time{Time: now.Add(time.Hour)},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Price != 0 {
		t.Errorf("price: want 0 when Cost is unset, got %f", resp.Price)
	}
}

func TestPricingNodePrice_RejectsMissingNode(t *testing.T) {
	client := newRunningServer(t, &fakeLocalAPI{})
	_, err := client.PricingNodePrice(context.Background(), &protos.PricingNodePriceRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// PricingPodPrice
// -----------------------------------------------------------------------------

func TestPricingPodPrice_Zero(t *testing.T) {
	client := newRunningServer(t, &fakeLocalAPI{})
	// v1.32 externalgrpc proto: structured v1.Pod on Pod (was: marshalled
	// bytes blob on PodBytes).
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
	resp, err := client.PricingPodPrice(context.Background(), &protos.PricingPodPriceRequest{
		Pod:       pod,
		StartTime: &metav1.Time{Time: time.Now()},
		EndTime:   &metav1.Time{Time: time.Now().Add(time.Hour)},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Price != 0 {
		t.Errorf("price: want 0, got %f", resp.Price)
	}
}

func TestPricingPodPrice_RejectsEmptyPod(t *testing.T) {
	client := newRunningServer(t, &fakeLocalAPI{})
	_, err := client.PricingPodPrice(context.Background(), &protos.PricingPodPriceRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// GPULabel
// -----------------------------------------------------------------------------

func TestGPULabel_NVIDIA(t *testing.T) {
	client := newRunningServer(t, &fakeLocalAPI{})
	resp, err := client.GPULabel(context.Background(), &protos.GPULabelRequest{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Label != "nvidia.com/gpu" {
		t.Errorf("label: want nvidia.com/gpu, got %q", resp.Label)
	}
}

// -----------------------------------------------------------------------------
// GetAvailableGPUTypes
// -----------------------------------------------------------------------------

func TestGetAvailableGPUTypes_DerivesFromGPUGroups(t *testing.T) {
	fb := &fakeLocalAPI{
		nodeGroups: &brokerapi.NodeGroupListResponse{
			NodeGroups: []brokerapi.NodeGroupView{
				{ID: "p1/standard", ProviderClusterID: "p1", Type: brokerv1alpha1.ChunkTypeStandard},
				{ID: "p1/gpu", ProviderClusterID: "p1", Type: brokerv1alpha1.ChunkTypeGPU},
				{ID: "p2/gpu", ProviderClusterID: "p2", Type: brokerv1alpha1.ChunkTypeGPU},
				{ID: "p2/extra-gpu", ProviderClusterID: "p2", Type: brokerv1alpha1.ChunkTypeGPU}, // dedupes
			},
		},
	}
	client := newRunningServer(t, fb)
	resp, err := client.GetAvailableGPUTypes(context.Background(), &protos.GetAvailableGPUTypesRequest{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, ok := resp.GpuTypes["p1"]; !ok {
		t.Error("want p1 in GpuTypes")
	}
	if _, ok := resp.GpuTypes["p2"]; !ok {
		t.Error("want p2 in GpuTypes")
	}
	if len(resp.GpuTypes) != 2 {
		t.Errorf("want 2 GPU providers, got %d", len(resp.GpuTypes))
	}
}

// -----------------------------------------------------------------------------
// Refresh / Cleanup
// -----------------------------------------------------------------------------

func TestRefresh_NoOp(t *testing.T) {
	client := newRunningServer(t, &fakeLocalAPI{})
	if _, err := client.Refresh(context.Background(), &protos.RefreshRequest{}); err != nil {
		t.Fatalf("Refresh should succeed; got %v", err)
	}
}

func TestCleanup_NoOp(t *testing.T) {
	client := newRunningServer(t, &fakeLocalAPI{})
	if _, err := client.Cleanup(context.Background(), &protos.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup should succeed; got %v", err)
	}
}

// -----------------------------------------------------------------------------
// NodeGroupNodes
// -----------------------------------------------------------------------------

func TestNodeGroupNodes_MapsPhasesToInstanceStates(t *testing.T) {
	// Each materialised entry carries a VirtualNodeName (== the real
	// v1.Node Liqo registered). The Instance Id must be that name, not
	// the CR-name placeholder in Name.
	fb := &fakeLocalAPI{
		virtualNodes: &localapi.VirtualNodeListResponse{
			VirtualNodes: []localapi.VirtualNodeView{
				{Name: "vns-running", VirtualNodeName: "vn-running", NodeGroupID: "p1/standard", Phase: autoscalingv1alpha1.VirtualNodeStatePhaseRunning},
				{Name: "vns-creating", VirtualNodeName: "vn-creating", NodeGroupID: "p1/standard", Phase: autoscalingv1alpha1.VirtualNodeStatePhaseCreating},
				{Name: "vns-deleting", VirtualNodeName: "vn-deleting", NodeGroupID: "p1/standard", Phase: autoscalingv1alpha1.VirtualNodeStatePhaseDeleting},
				{Name: "vns-failed", VirtualNodeName: "vn-failed", NodeGroupID: "p1/standard", Phase: autoscalingv1alpha1.VirtualNodeStatePhaseFailed},
				{Name: "vns-other-group", VirtualNodeName: "vn-other-group", NodeGroupID: "p2/gpu", Phase: autoscalingv1alpha1.VirtualNodeStatePhaseRunning},
			},
		},
	}
	client := newRunningServer(t, fb)
	resp, err := client.NodeGroupNodes(context.Background(), &protos.NodeGroupNodesRequest{Id: "p1/standard"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(resp.Instances) != 4 {
		t.Fatalf("want 4 instances in p1/standard, got %d", len(resp.Instances))
	}
	byName := map[string]*protos.Instance{}
	for _, inst := range resp.Instances {
		byName[inst.Id] = inst
	}
	if got := byName["vn-running"].Status.InstanceState; got != protos.InstanceStatus_instanceRunning {
		t.Errorf("vn-running state: want Running, got %v", got)
	}
	if got := byName["vn-creating"].Status.InstanceState; got != protos.InstanceStatus_instanceCreating {
		t.Errorf("vn-creating state: want Creating, got %v", got)
	}
	if got := byName["vn-deleting"].Status.InstanceState; got != protos.InstanceStatus_instanceDeleting {
		t.Errorf("vn-deleting state: want Deleting, got %v", got)
	}
	if vn := byName["vn-failed"]; vn.Status.ErrorInfo == nil || vn.Status.ErrorInfo.ErrorCode == "" {
		t.Errorf("vn-failed: want non-empty ErrorInfo, got %+v", vn.Status)
	}
}

// TestNodeGroupNodes_SkipsUnmaterialisedNodes is the regression guard
// for the scale-down-stuck bug: a VNS whose v1.Node hasn't registered
// (VirtualNodeName empty) must NOT be reported, or CA sees an
// "unregistered node" that never appears and never scales down.
func TestNodeGroupNodes_SkipsUnmaterialisedNodes(t *testing.T) {
	fb := &fakeLocalAPI{
		virtualNodes: &localapi.VirtualNodeListResponse{
			VirtualNodes: []localapi.VirtualNodeView{
				// Registered: reported, Id == real v1.Node name.
				{Name: "vns-ready", VirtualNodeName: "provider-1", NodeGroupID: "p1/standard", Phase: autoscalingv1alpha1.VirtualNodeStatePhaseRunning},
				// In-flight: VirtualNodeName empty → skipped even though
				// the CR name placeholder is present in Name.
				{Name: "vns-inflight", VirtualNodeName: "", NodeGroupID: "p1/standard", Phase: autoscalingv1alpha1.VirtualNodeStatePhaseCreating},
			},
		},
	}
	client := newRunningServer(t, fb)
	resp, err := client.NodeGroupNodes(context.Background(), &protos.NodeGroupNodesRequest{Id: "p1/standard"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(resp.Instances) != 1 {
		t.Fatalf("want 1 instance (only the registered node), got %d", len(resp.Instances))
	}
	if got := resp.Instances[0].Id; got != "provider-1" {
		t.Errorf("Instance Id: want real v1.Node name provider-1, got %q", got)
	}
}

func TestNodeGroupNodes_RejectsEmptyID(t *testing.T) {
	client := newRunningServer(t, &fakeLocalAPI{})
	_, err := client.NodeGroupNodes(context.Background(), &protos.NodeGroupNodesRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}
