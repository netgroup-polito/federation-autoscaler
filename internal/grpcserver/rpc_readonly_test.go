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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/agentclient"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/protos"
)

// fakeLocalAPI is a tiny httptest broker for the consumer agent's
// loopback REST. Tests pre-load its NodeGroups + VirtualNodes
// responses; both endpoints are served from the same struct so a
// single Server can be exercised from multiple RPCs.
type fakeLocalAPI struct {
	nodeGroups   *brokerapi.NodeGroupListResponse
	virtualNodes *localapi.VirtualNodeListResponse

	// Captured mutating-call state (10d).
	mu                 sync.Mutex
	postedReservations []capturedReservation
	deletedReservation []string
}

type capturedReservation struct {
	id  string // X-Reservation-Id header value
	req brokerapi.ReservationRequest
}

func (f *fakeLocalAPI) start(t *testing.T) (*httptest.Server, *agentclient.Client) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /local/nodegroups", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		if f.nodeGroups == nil {
			_ = json.NewEncoder(w).Encode(brokerapi.NodeGroupListResponse{})
			return
		}
		_ = json.NewEncoder(w).Encode(f.nodeGroups)
	})
	mux.HandleFunc("GET /local/virtual-nodes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		if f.virtualNodes == nil {
			_ = json.NewEncoder(w).Encode(localapi.VirtualNodeListResponse{})
			return
		}
		_ = json.NewEncoder(w).Encode(f.virtualNodes)
	})
	mux.HandleFunc("POST /local/reservations", func(w http.ResponseWriter, r *http.Request) {
		var req brokerapi.ReservationRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.postedReservations = append(f.postedReservations, capturedReservation{
			id:  r.Header.Get(brokerapi.HeaderReservationID),
			req: req,
		})
		f.mu.Unlock()
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(brokerapi.ReservationResponse{
			ReservationID: r.Header.Get(brokerapi.HeaderReservationID),
			Status:        brokerv1alpha1.ReservationPhasePending,
			ChunkCount:    req.ChunkCount,
		})
	})
	mux.HandleFunc("DELETE /local/reservations/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		f.deletedReservation = append(f.deletedReservation, id)
		f.mu.Unlock()
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.ReleaseResponse{
			ReservationID: id,
			Status:        brokerv1alpha1.ReservationPhaseUnpeering,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := agentclient.New(agentclient.Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

func (f *fakeLocalAPI) snapshotPosted() []capturedReservation {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedReservation, len(f.postedReservations))
	copy(out, f.postedReservations)
	return out
}

func (f *fakeLocalAPI) snapshotDeleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletedReservation...)
}

// newRunningServer boots a real gRPC Server wired to fb and returns a
// connected proto client. The server is shut down via t.Cleanup.
func newRunningServer(t *testing.T, fb *fakeLocalAPI) protos.CloudProviderClient {
	t.Helper()
	dir, clientTLS := stageCertDir(t)
	_, agent := fb.start(t)

	s, err := New(Options{
		BindAddress:     "127.0.0.1:0",
		TLS:             TLSConfig{CertDir: dir},
		AgentClient:     agent,
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("server did not exit within 3s")
		}
	})

	deadline := time.Now().Add(2 * time.Second)
	for s.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Addr() == nil {
		t.Fatal("server did not bind")
	}

	conn, err := grpc.NewClient(s.Addr().String(),
		grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return protos.NewCloudProviderClient(conn)
}

// -----------------------------------------------------------------------------
// NodeGroups
// -----------------------------------------------------------------------------

func TestNodeGroups_MapsAllFields(t *testing.T) {
	fb := &fakeLocalAPI{
		nodeGroups: &brokerapi.NodeGroupListResponse{
			NodeGroups: []brokerapi.NodeGroupView{
				{ID: "p1/standard", ProviderClusterID: "p1", Type: brokerv1alpha1.ChunkTypeStandard, MinSize: 0, MaxSize: 4, CurrentReserved: 1},
				{ID: "p2/gpu", ProviderClusterID: "p2", Type: brokerv1alpha1.ChunkTypeGPU, MinSize: 0, MaxSize: 2},
			},
		},
	}
	client := newRunningServer(t, fb)
	resp, err := client.NodeGroups(context.Background(), &protos.NodeGroupsRequest{})
	if err != nil {
		t.Fatalf("NodeGroups: %v", err)
	}
	if len(resp.NodeGroups) != 2 {
		t.Fatalf("want 2 node groups, got %d", len(resp.NodeGroups))
	}
	byID := map[string]*protos.NodeGroup{}
	for _, ng := range resp.NodeGroups {
		byID[ng.Id] = ng
	}
	if got := byID["p1/standard"]; got == nil || got.MinSize != 0 || got.MaxSize != 4 ||
		!strings.Contains(got.Debug, "p1") || !strings.Contains(got.Debug, "current=1") {
		t.Errorf("p1/standard mismatch: %+v", got)
	}
	if got := byID["p2/gpu"]; got == nil || got.MaxSize != 2 {
		t.Errorf("p2/gpu mismatch: %+v", got)
	}
}

func TestNodeGroups_EmptyList(t *testing.T) {
	client := newRunningServer(t, &fakeLocalAPI{})
	resp, err := client.NodeGroups(context.Background(), &protos.NodeGroupsRequest{})
	if err != nil {
		t.Fatalf("NodeGroups: %v", err)
	}
	if len(resp.NodeGroups) != 0 {
		t.Errorf("want empty list, got %+v", resp.NodeGroups)
	}
}

// -----------------------------------------------------------------------------
// NodeGroupForNode
// -----------------------------------------------------------------------------

func TestNodeGroupForNode_FoundAndNotFound(t *testing.T) {
	fb := &fakeLocalAPI{
		virtualNodes: &localapi.VirtualNodeListResponse{
			VirtualNodes: []localapi.VirtualNodeView{
				{Name: "liqo-virt-1", NodeGroupID: "p1/standard"},
				{Name: "liqo-virt-2", NodeGroupID: "p2/gpu"},
			},
		},
	}
	client := newRunningServer(t, fb)

	// Known node → resolves to its group.
	resp, err := client.NodeGroupForNode(context.Background(), &protos.NodeGroupForNodeRequest{
		Node: &protos.ExternalGrpcNode{Name: "liqo-virt-2"},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.NodeGroup == nil || resp.NodeGroup.Id != "p2/gpu" {
		t.Errorf("want p2/gpu, got %+v", resp.NodeGroup)
	}

	// Unknown node → empty NodeGroup, no error.
	resp, err = client.NodeGroupForNode(context.Background(), &protos.NodeGroupForNodeRequest{
		Node: &protos.ExternalGrpcNode{Name: "some-other-node"},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.NodeGroup == nil || resp.NodeGroup.Id != "" {
		t.Errorf("unknown node should yield empty NodeGroup, got %+v", resp.NodeGroup)
	}
}

func TestNodeGroupForNode_RejectsMissingNode(t *testing.T) {
	client := newRunningServer(t, &fakeLocalAPI{})
	_, err := client.NodeGroupForNode(context.Background(), &protos.NodeGroupForNodeRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// NodeGroupTargetSize
// -----------------------------------------------------------------------------

func TestNodeGroupTargetSize_ReturnsReservedChunks(t *testing.T) {
	// p1/standard has 2 chunks RESERVED but — deliberately — no materialized
	// virtual node yet (the ~100 s peering window). TargetSize must report 2
	// (reserved chunks), NOT 0, so CA sees its request landed and does not
	// re-issue IncreaseSize every loop up to MaxSize (the over-provision bug).
	fb := &fakeLocalAPI{
		nodeGroups: &brokerapi.NodeGroupListResponse{
			NodeGroups: []brokerapi.NodeGroupView{
				{ID: "p1/standard", ProviderClusterID: "p1", Type: brokerv1alpha1.ChunkTypeStandard, MinSize: 0, MaxSize: 3, CurrentReserved: 2},
				{ID: "p2/gpu", ProviderClusterID: "p2", Type: brokerv1alpha1.ChunkTypeGPU, MinSize: 0, MaxSize: 4, CurrentReserved: 0},
			},
		},
		// No virtual nodes have materialized — proves TargetSize is
		// reservation-driven, not live-node-count-driven.
	}
	client := newRunningServer(t, fb)

	resp, err := client.NodeGroupTargetSize(context.Background(),
		&protos.NodeGroupTargetSizeRequest{Id: "p1/standard"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.TargetSize != 2 {
		t.Errorf("want 2 (reserved chunks), got %d", resp.TargetSize)
	}

	// A group with no reservations → 0.
	resp, err = client.NodeGroupTargetSize(context.Background(),
		&protos.NodeGroupTargetSizeRequest{Id: "p2/gpu"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.TargetSize != 0 {
		t.Errorf("want 0 for a group with no reservations, got %d", resp.TargetSize)
	}

	// A group the broker no longer advertises → 0, not an error.
	resp, err = client.NodeGroupTargetSize(context.Background(),
		&protos.NodeGroupTargetSizeRequest{Id: "p3/standard"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.TargetSize != 0 {
		t.Errorf("want 0 for an unknown group, got %d", resp.TargetSize)
	}
}

func TestNodeGroupTargetSize_RejectsEmptyID(t *testing.T) {
	client := newRunningServer(t, &fakeLocalAPI{})
	_, err := client.NodeGroupTargetSize(context.Background(), &protos.NodeGroupTargetSizeRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// NodeGroupTemplateNodeInfo
// -----------------------------------------------------------------------------

func TestNodeGroupTemplateNodeInfo_BuildsAndSerializes(t *testing.T) {
	fb := &fakeLocalAPI{
		nodeGroups: &brokerapi.NodeGroupListResponse{
			NodeGroups: []brokerapi.NodeGroupView{
				{
					ID:                "p1/standard",
					ProviderClusterID: "p1",
					Type:              brokerv1alpha1.ChunkTypeStandard,
					MinSize:           0,
					MaxSize:           4,
					ChunkResources: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("2"),
						corev1.ResourceMemory: resource.MustParse("4Gi"),
					},
					Labels: map[string]string{"team": "platform"},
					Topology: &brokerv1alpha1.Topology{
						Region: "eu-west-1",
						Zone:   "eu-west-1a",
					},
				},
			},
		},
	}
	client := newRunningServer(t, fb)
	resp, err := client.NodeGroupTemplateNodeInfo(context.Background(),
		&protos.NodeGroupTemplateNodeInfoRequest{Id: "p1/standard"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// v1.32 externalgrpc returns the template as a structured v1.Node
	// in NodeInfo rather than a marshalled bytes blob.
	if resp.NodeInfo == nil {
		t.Fatal("NodeInfo is nil")
	}
	node := *resp.NodeInfo
	if node.Name != "template-p1/standard" {
		t.Errorf("template name: want template-p1/standard, got %q", node.Name)
	}
	if got := node.Labels["team"]; got != "platform" {
		t.Errorf("team label missing: %v", node.Labels)
	}
	if got := node.Labels[corev1.LabelTopologyRegion]; got != "eu-west-1" {
		t.Errorf("region label: want eu-west-1, got %q", got)
	}
	if got := node.Labels[corev1.LabelTopologyZone]; got != "eu-west-1a" {
		t.Errorf("zone label: want eu-west-1a, got %q", got)
	}
	if got := node.Status.Allocatable[corev1.ResourceCPU]; got.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("CPU allocatable: want 2, got %s", got.String())
	}
	if got := node.Status.Capacity[corev1.ResourceMemory]; got.Cmp(resource.MustParse("4Gi")) != 0 {
		t.Errorf("memory capacity: want 4Gi, got %s", got.String())
	}
	if !strings.HasPrefix(node.Spec.ProviderID, "liqo://") {
		t.Errorf("providerID: want liqo://… prefix, got %q", node.Spec.ProviderID)
	}
}

func TestNodeGroupTemplateNodeInfo_NotFound(t *testing.T) {
	fb := &fakeLocalAPI{nodeGroups: &brokerapi.NodeGroupListResponse{}}
	client := newRunningServer(t, fb)
	_, err := client.NodeGroupTemplateNodeInfo(context.Background(),
		&protos.NodeGroupTemplateNodeInfoRequest{Id: "missing"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestNodeGroupTemplateNodeInfo_RejectsEmptyID(t *testing.T) {
	client := newRunningServer(t, &fakeLocalAPI{})
	_, err := client.NodeGroupTemplateNodeInfo(context.Background(),
		&protos.NodeGroupTemplateNodeInfoRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}
