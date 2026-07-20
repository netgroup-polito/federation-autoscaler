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
	"sort"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/protos"
)

// -----------------------------------------------------------------------------
// NodeGroupIncreaseSize
// -----------------------------------------------------------------------------

func TestNodeGroupIncreaseSize_PostsReservationWithProviderAndType(t *testing.T) {
	fb := &fakeLocalAPI{
		nodeGroups: &brokerapi.NodeGroupListResponse{
			NodeGroups: []brokerapi.NodeGroupView{
				{ID: "p1/standard", ProviderClusterID: "p1", Type: brokerv1alpha1.ChunkTypeStandard, MaxSize: 10},
			},
		},
	}
	client := newRunningServer(t, fb)

	_, err := client.NodeGroupIncreaseSize(context.Background(),
		&protos.NodeGroupIncreaseSizeRequest{Id: "p1/standard", Delta: 3})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// One Reservation PER CHUNK. A Reservation materialises exactly one
	// ResourceSlice and therefore one node, so a single 3-chunk reservation
	// would be billed 3 and reported to CA as 3 while delivering one node.
	posted := fb.snapshotPosted()
	if len(posted) != 3 {
		t.Fatalf("want 3 PostReservations (one per chunk), got %d", len(posted))
	}
	seen := map[string]bool{}
	for i, p := range posted {
		if p.req.ProviderClusterID != "p1" ||
			p.req.ChunkCount != 1 ||
			p.req.ChunkType != brokerv1alpha1.ChunkTypeStandard ||
			p.req.NodeGroupID != "p1/standard" {
			t.Errorf("reservation %d request mismatch: %+v", i, p.req)
		}
		if !strings.HasPrefix(p.id, "res-") {
			t.Errorf("X-Reservation-Id should be 'res-<uuid>'; got %q", p.id)
		}
		if seen[p.id] {
			t.Errorf("reservation ids must be distinct; %q reused", p.id)
		}
		seen[p.id] = true
	}
}

func TestNodeGroupIncreaseSize_RejectsInvalidArgs(t *testing.T) {
	client := newRunningServer(t, &fakeLocalAPI{})
	cases := []struct {
		name string
		req  *protos.NodeGroupIncreaseSizeRequest
	}{
		{"empty id", &protos.NodeGroupIncreaseSizeRequest{Delta: 1}},
		{"zero delta", &protos.NodeGroupIncreaseSizeRequest{Id: "p1", Delta: 0}},
		{"negative delta", &protos.NodeGroupIncreaseSizeRequest{Id: "p1", Delta: -2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.NodeGroupIncreaseSize(context.Background(), tc.req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("want InvalidArgument, got %v", err)
			}
		})
	}
}

func TestNodeGroupIncreaseSize_UnknownGroup_NotFound(t *testing.T) {
	fb := &fakeLocalAPI{nodeGroups: &brokerapi.NodeGroupListResponse{}}
	client := newRunningServer(t, fb)
	_, err := client.NodeGroupIncreaseSize(context.Background(),
		&protos.NodeGroupIncreaseSizeRequest{Id: "missing", Delta: 1})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// NodeGroupDeleteNodes
// -----------------------------------------------------------------------------

func TestNodeGroupDeleteNodes_DedupesByReservation(t *testing.T) {
	fb := &fakeLocalAPI{
		virtualNodes: &localapi.VirtualNodeListResponse{
			VirtualNodes: []localapi.VirtualNodeView{
				{Name: "vn-1", ReservationID: "res-a", NodeGroupID: "p1/standard"},
				{Name: "vn-2", ReservationID: "res-a", NodeGroupID: "p1/standard"}, // same reservation
				{Name: "vn-3", ReservationID: "res-b", NodeGroupID: "p1/standard"},
			},
		},
	}
	client := newRunningServer(t, fb)

	_, err := client.NodeGroupDeleteNodes(context.Background(),
		&protos.NodeGroupDeleteNodesRequest{
			Id: "p1/standard",
			Nodes: []*protos.ExternalGrpcNode{
				{Name: "vn-1"},
				{Name: "vn-2"},
				{Name: "vn-3"},
			},
		})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	got := fb.snapshotDeleted()
	sort.Strings(got)
	want := []string{"res-a", "res-b"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("delete[%d]: want %s, got %s", i, w, got[i])
		}
	}
}

func TestNodeGroupDeleteNodes_RejectsInvalidArgs(t *testing.T) {
	client := newRunningServer(t, &fakeLocalAPI{})
	cases := []struct {
		name string
		req  *protos.NodeGroupDeleteNodesRequest
	}{
		{"empty id", &protos.NodeGroupDeleteNodesRequest{Nodes: []*protos.ExternalGrpcNode{{Name: "vn"}}}},
		{"empty nodes", &protos.NodeGroupDeleteNodesRequest{Id: "p1/standard"}},
		{"nil node", &protos.NodeGroupDeleteNodesRequest{
			Id:    "p1/standard",
			Nodes: []*protos.ExternalGrpcNode{nil},
		}},
		{"empty node name", &protos.NodeGroupDeleteNodesRequest{
			Id:    "p1/standard",
			Nodes: []*protos.ExternalGrpcNode{{Name: ""}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.NodeGroupDeleteNodes(context.Background(), tc.req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("want InvalidArgument, got %v", err)
			}
		})
	}
}

func TestNodeGroupDeleteNodes_UnknownNode_NotFound(t *testing.T) {
	fb := &fakeLocalAPI{virtualNodes: &localapi.VirtualNodeListResponse{}}
	client := newRunningServer(t, fb)
	_, err := client.NodeGroupDeleteNodes(context.Background(),
		&protos.NodeGroupDeleteNodesRequest{
			Id:    "p1/standard",
			Nodes: []*protos.ExternalGrpcNode{{Name: "vn-missing"}},
		})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// NodeGroupDecreaseTargetSize
// -----------------------------------------------------------------------------

func TestNodeGroupDecreaseTargetSize_NoOpSuccess(t *testing.T) {
	client := newRunningServer(t, &fakeLocalAPI{})
	_, err := client.NodeGroupDecreaseTargetSize(context.Background(),
		&protos.NodeGroupDecreaseTargetSizeRequest{Id: "p1/standard", Delta: -1})
	if err != nil {
		t.Fatalf("want success, got %v", err)
	}
}

func TestNodeGroupDecreaseTargetSize_RejectsInvalidArgs(t *testing.T) {
	client := newRunningServer(t, &fakeLocalAPI{})
	cases := []struct {
		name string
		req  *protos.NodeGroupDecreaseTargetSizeRequest
	}{
		{"empty id", &protos.NodeGroupDecreaseTargetSizeRequest{Delta: -1}},
		{"zero delta", &protos.NodeGroupDecreaseTargetSizeRequest{Id: "p1", Delta: 0}},
		{"positive delta", &protos.NodeGroupDecreaseTargetSizeRequest{Id: "p1", Delta: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.NodeGroupDecreaseTargetSize(context.Background(), tc.req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("want InvalidArgument, got %v", err)
			}
		})
	}
}
