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

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/broker/chunk"
)

const dashboardTestNS = "federation-autoscaler-system"

// newDashboardTestServer builds a Server backed by a controller-runtime fake
// client seeded with objs — no envtest needed. The dashboard only reads, so a
// fake client is sufficient and keeps the test a fast unit test.
func newDashboardTestServer(t *testing.T, objs ...client.Object) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := brokerv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add broker scheme: %v", err)
	}
	if err := autoscalingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add autoscaling scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Server{
		log:       logr.Discard(),
		client:    c,
		namespace: dashboardTestNS,
		sizer:     chunk.NewDefaultSizer(),
		consumers: NewConsumerRegistry(),
	}
}

// dashboardSeed returns a representative spread of Broker state: two providers
// (one available standard, one stale gpu), one Peered reservation, an enforced
// and a pending instruction of each kind. The pending ReservationInstruction
// carries a KubeconfigRef so we can prove the dashboard never leaks it.
func dashboardSeed() []client.Object {
	return []client.Object{
		&brokerv1alpha1.ClusterAdvertisement{
			ObjectMeta: metav1.ObjectMeta{Name: providerCluster, Namespace: dashboardTestNS},
			Spec: brokerv1alpha1.ClusterAdvertisementSpec{
				ClusterID: providerCluster, LiqoClusterID: "liqo-pa",
				ClusterType: brokerv1alpha1.ChunkTypeStandard,
			},
			Status: brokerv1alpha1.ClusterAdvertisementStatus{
				Available: true, TotalChunks: 4, ReservedChunks: 1, AvailableChunks: 3,
			},
		},
		&brokerv1alpha1.ClusterAdvertisement{
			ObjectMeta: metav1.ObjectMeta{Name: "provider-b", Namespace: dashboardTestNS},
			Spec: brokerv1alpha1.ClusterAdvertisementSpec{
				ClusterID: "provider-b", LiqoClusterID: "liqo-pb",
				ClusterType: brokerv1alpha1.ChunkTypeGPU,
			},
			Status: brokerv1alpha1.ClusterAdvertisementStatus{
				Available: false, TotalChunks: 2, ReservedChunks: 0, AvailableChunks: 2,
			},
		},
		&brokerv1alpha1.Reservation{
			ObjectMeta: metav1.ObjectMeta{Name: "resv-1", Namespace: dashboardTestNS},
			Spec: brokerv1alpha1.ReservationSpec{
				ConsumerClusterID: consumerCluster, ConsumerLiqoClusterID: "liqo-ca",
				ProviderClusterID: providerCluster, ProviderLiqoClusterID: "liqo-pa",
				ChunkCount: 1, ChunkType: brokerv1alpha1.ChunkTypeStandard,
			},
			Status: brokerv1alpha1.ReservationStatus{
				Phase:            brokerv1alpha1.ReservationPhasePeered,
				VirtualNodeNames: []string{"liqo-provider-a"},
				Message:          "peering completed",
			},
		},
		&autoscalingv1alpha1.ProviderInstruction{
			ObjectMeta: metav1.ObjectMeta{Name: "pi-enforced", Namespace: dashboardTestNS},
			Spec: autoscalingv1alpha1.ProviderInstructionSpec{
				ReservationID: "resv-1", Kind: autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig,
				TargetClusterID: providerCluster, ConsumerClusterID: consumerCluster,
			},
			Status: autoscalingv1alpha1.ProviderInstructionStatus{Enforced: true, Attempts: 1},
		},
		&autoscalingv1alpha1.ProviderInstruction{
			ObjectMeta: metav1.ObjectMeta{Name: "pi-pending", Namespace: dashboardTestNS},
			Spec: autoscalingv1alpha1.ProviderInstructionSpec{
				ReservationID: "resv-1", Kind: autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig,
				TargetClusterID: providerCluster, ConsumerClusterID: consumerCluster,
			},
			Status: autoscalingv1alpha1.ProviderInstructionStatus{Enforced: false},
		},
		&autoscalingv1alpha1.ReservationInstruction{
			ObjectMeta: metav1.ObjectMeta{Name: "ri-enforced", Namespace: dashboardTestNS},
			Spec: autoscalingv1alpha1.ReservationInstructionSpec{
				ReservationID: "resv-1", Kind: autoscalingv1alpha1.ReservationInstructionPeer,
				TargetClusterID: consumerCluster, ProviderClusterID: providerCluster,
				// The dashboard must never surface this credential reference.
				KubeconfigRef: "kubeconfig-consumer-a-provider-a",
			},
			Status: autoscalingv1alpha1.ReservationInstructionStatus{Enforced: true, Attempts: 1},
		},
		&autoscalingv1alpha1.ReservationInstruction{
			ObjectMeta: metav1.ObjectMeta{Name: "ri-pending", Namespace: dashboardTestNS},
			Spec: autoscalingv1alpha1.ReservationInstructionSpec{
				ReservationID: "resv-1", Kind: autoscalingv1alpha1.ReservationInstructionUnpeer,
				TargetClusterID: consumerCluster, ProviderClusterID: providerCluster, LastChunk: true,
			},
			Status: autoscalingv1alpha1.ReservationInstructionStatus{Enforced: false},
		},
	}
}

// TestDashboardBuildOverview asserts buildOverview aggregates capacity, surfaces
// ALL instructions (enforced included), reflects registry consumers, and never
// exposes kubeconfig material.
func TestDashboardBuildOverview(t *testing.T) {
	s := newDashboardTestServer(t, dashboardSeed()...)
	s.consumers.Touch(consumerCluster, "liqo-ca")
	s.consumers.Touch("consumer-b", "liqo-cb")

	ov, err := s.buildOverview(context.Background())
	if err != nil {
		t.Fatalf("buildOverview: %v", err)
	}

	// Capacity rollup across both providers.
	if ov.Capacity.TotalChunks != 6 || ov.Capacity.ReservedChunks != 1 || ov.Capacity.AvailableChunks != 5 {
		t.Errorf("capacity totals = %+v, want total=6 reserved=1 available=5", ov.Capacity)
	}
	if ov.Capacity.ProviderCount != 2 || ov.Capacity.AvailableProviderCount != 1 {
		t.Errorf("provider counts = %d/%d, want 1/2 available/total",
			ov.Capacity.AvailableProviderCount, ov.Capacity.ProviderCount)
	}
	if std := ov.Capacity.ByChunkType["standard"]; std.TotalChunks != 4 || std.AvailableChunks != 3 {
		t.Errorf("standard chunk capacity = %+v, want total=4 available=3", std)
	}
	if gpu := ov.Capacity.ByChunkType["gpu"]; gpu.TotalChunks != 2 || gpu.AvailableChunks != 2 {
		t.Errorf("gpu chunk capacity = %+v, want total=2 available=2", gpu)
	}

	// Canonical per-chunk sizes are surfaced for both types (from the sizer).
	std := ov.Capacity.ChunkSizes["standard"]
	if std.Cpu().Value() != 2 || std.Memory().Value() != 4*1024*1024*1024 {
		t.Errorf("standard chunk size = %v, want cpu=2 / memory=4Gi", std)
	}
	gpu := ov.Capacity.ChunkSizes["gpu"]
	gpuQty := gpu["nvidia.com/gpu"]
	if gpu.Cpu().Value() != 4 || gpu.Memory().Value() != 8*1024*1024*1024 || gpuQty.Value() != 1 {
		t.Errorf("gpu chunk size = %v, want cpu=4 / memory=8Gi / gpu=1", gpu)
	}

	// Advertisements: both present, sorted by ClusterID.
	if len(ov.Advertisements) != 2 || ov.Advertisements[0].ClusterID != providerCluster {
		t.Errorf("advertisements = %+v, want 2 sorted with provider-a first", ov.Advertisements)
	}

	// Reservations: the single Peered reservation, not yet terminated.
	if len(ov.Reservations) != 1 {
		t.Fatalf("reservations len = %d, want 1", len(ov.Reservations))
	}
	if ov.Reservations[0].Status != brokerv1alpha1.ReservationPhasePeered {
		t.Errorf("reservation phase = %q, want Peered", ov.Reservations[0].Status)
	}
	if ov.Reservations[0].TerminatedAt != nil {
		t.Errorf("reservation TerminatedAt = %v, want nil", ov.Reservations[0].TerminatedAt)
	}

	// Instructions: enforced ones are INCLUDED (unlike the agent-facing path).
	if len(ov.ProviderInstructions) != 2 || len(ov.ReservationInstructions) != 2 {
		t.Errorf("instruction counts = %d provider / %d reservation, want 2 / 2",
			len(ov.ProviderInstructions), len(ov.ReservationInstructions))
	}
	if !hasEnforced(ov.ProviderInstructions) || !hasEnforced(ov.ReservationInstructions) {
		t.Errorf("expected at least one enforced instruction in each list")
	}

	// Consumers from the in-memory registry.
	if len(ov.Consumers) != 2 || ov.Consumers[0].ClusterID != consumerCluster {
		t.Errorf("consumers = %+v, want 2 sorted with consumer-a first", ov.Consumers)
	}

	// No kubeconfig material anywhere in the serialized overview. We check for
	// the staged-Secret reference and the credential field keys specifically —
	// not the bare substring "kubeconfig", which legitimately appears inside the
	// "GenerateKubeconfig" instruction KIND value.
	blob, err := json.Marshal(ov)
	if err != nil {
		t.Fatalf("marshal overview: %v", err)
	}
	lower := strings.ToLower(string(blob))
	for _, forbidden := range []string{
		"kubeconfig-consumer-a-provider-a", // the staged-Secret ref must never leak
		`"kubeconfig"`,                     // no inlined kubeconfig bytes field
		`"kubeconfigref"`,                  // no KubeconfigRef field
	} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("overview JSON leaks %q:\n%s", forbidden, blob)
		}
	}
}

func hasEnforced(list []DashboardInstructionView) bool {
	for _, v := range list {
		if v.Enforced {
			return true
		}
	}
	return false
}

// TestDashboardHandlersNoAuth proves the dashboard routes serve WITHOUT a client
// certificate (no ClusterIDMiddleware), return the right shapes, and 404 any
// non-root path.
func TestDashboardHandlersNoAuth(t *testing.T) {
	s := newDashboardTestServer(t, dashboardSeed()...)
	h := s.DashboardMiddlewareChain(s.DashboardHandler())

	// /api/v1/overview — no TLS on the request (req.TLS == nil) yet 200 OK.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("overview status = %d, want 200 (auth must be absent)", rr.Code)
	}
	var ov Overview
	if err := json.Unmarshal(rr.Body.Bytes(), &ov); err != nil {
		t.Fatalf("overview is not valid JSON: %v", err)
	}
	if len(ov.Advertisements) != 2 {
		t.Errorf("overview advertisements = %d, want 2", len(ov.Advertisements))
	}

	// / — the embedded HTML page.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("index status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("index content-type = %q, want text/html", ct)
	}
	if rr.Body.Len() == 0 {
		t.Errorf("index body is empty; embedded page missing")
	}

	// Any non-root path under the catch-all must 404, not serve the page.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/does-not-exist", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", rr.Code)
	}
}
