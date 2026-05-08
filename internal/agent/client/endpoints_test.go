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

package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// fakeBroker captures the last incoming request so each endpoint test can
// assert method/path/body shape without rebuilding plumbing per spec.
// Provide handler at construction time; observed fields are populated as
// a side-effect of ServeHTTP.
type fakeBroker struct {
	t       *testing.T
	srv     *httptest.Server
	method  string
	path    string
	headers http.Header
	body    []byte
}

func newFakeBroker(t *testing.T, handler http.HandlerFunc) *fakeBroker {
	t.Helper()
	fb := &fakeBroker{t: t}
	fb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.method = r.Method
		fb.path = r.URL.Path
		fb.headers = r.Header.Clone()
		fb.body, _ = io.ReadAll(r.Body)
		handler(w, r)
	}))
	t.Cleanup(fb.srv.Close)
	return fb
}

func (fb *fakeBroker) client() *Client {
	return newTestClient(fb.t, fb.srv, Options{
		MaxRetries: 0,
	})
}

func TestPostAdvertisement(t *testing.T) {
	fb := newFakeBroker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.AdvertisementResponse{
			Accepted:       true,
			ChunkCount:     4,
			ChunkResources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			NextReportIn:   "30s",
		})
	})

	resp, err := fb.client().PostAdvertisement(context.Background(), &brokerapi.AdvertisementRequest{
		ClusterID:     "provider-a",
		LiqoClusterID: "liqo-provider-a",
		Resources: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("8"),
			corev1.ResourceMemory: resource.MustParse("16Gi"),
		},
	})
	if err != nil {
		t.Fatalf("PostAdvertisement: %v", err)
	}
	if fb.method != http.MethodPost || fb.path != "/api/v1/advertisements" {
		t.Errorf("wrong method/path: %s %s", fb.method, fb.path)
	}
	if got := fb.headers.Get("Content-Type"); got != brokerapi.ContentTypeJSON {
		t.Errorf("Content-Type: want %q, got %q", brokerapi.ContentTypeJSON, got)
	}
	if !resp.Accepted || resp.ChunkCount != 4 {
		t.Errorf("response decode mismatch: %+v", resp)
	}

	// Body shape sanity: the JSON must contain the clusterId we sent.
	var sent brokerapi.AdvertisementRequest
	if err := json.Unmarshal(fb.body, &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent.ClusterID != "provider-a" {
		t.Errorf("sent body mismatch: %+v", sent)
	}
}

func TestPostHeartbeat(t *testing.T) {
	fb := newFakeBroker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.HeartbeatResponse{
			AckAt: metav1.NewTime(metav1.Now().Time),
		})
	})

	resp, err := fb.client().PostHeartbeat(context.Background(), &brokerapi.HeartbeatRequest{
		ClusterID:     "consumer-a",
		LiqoClusterID: "liqo-consumer-a",
	})
	if err != nil {
		t.Fatalf("PostHeartbeat: %v", err)
	}
	if fb.method != http.MethodPost || fb.path != "/api/v1/heartbeat" {
		t.Errorf("wrong method/path: %s %s", fb.method, fb.path)
	}
	if resp.AckAt.IsZero() {
		t.Error("expected non-zero AckAt")
	}
}

func TestGetNodeGroups(t *testing.T) {
	fb := newFakeBroker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.NodeGroupListResponse{
			NodeGroups: []brokerapi.NodeGroupView{
				{ID: "provider-a/standard", ProviderClusterID: "provider-a", Type: brokerv1alpha1.ChunkTypeStandard, MinSize: 0, MaxSize: 4},
			},
			Generation: 7,
		})
	})

	resp, err := fb.client().GetNodeGroups(context.Background())
	if err != nil {
		t.Fatalf("GetNodeGroups: %v", err)
	}
	if fb.method != http.MethodGet || fb.path != "/api/v1/nodegroups" {
		t.Errorf("wrong method/path: %s %s", fb.method, fb.path)
	}
	if len(resp.NodeGroups) != 1 || resp.NodeGroups[0].ID != "provider-a/standard" {
		t.Errorf("decode mismatch: %+v", resp)
	}
	if resp.Generation != 7 {
		t.Errorf("generation: want 7, got %d", resp.Generation)
	}
}

func TestPostReservation_AttachesIdempotencyHeader(t *testing.T) {
	fb := newFakeBroker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(brokerapi.ReservationResponse{
			ReservationID: "res-abc",
			Status:        brokerv1alpha1.ReservationPhasePending,
			ChunkCount:    2,
			CreatedAt:     metav1.NewTime(metav1.Now().Time),
		})
	})

	resp, err := fb.client().PostReservation(context.Background(), "res-abc", &brokerapi.ReservationRequest{
		ProviderClusterID: "provider-a",
		ChunkCount:        2,
		ChunkType:         brokerv1alpha1.ChunkTypeStandard,
	})
	if err != nil {
		t.Fatalf("PostReservation: %v", err)
	}
	if fb.method != http.MethodPost || fb.path != "/api/v1/reservations" {
		t.Errorf("wrong method/path: %s %s", fb.method, fb.path)
	}
	if got := fb.headers.Get(brokerapi.HeaderReservationID); got != "res-abc" {
		t.Errorf("X-Reservation-Id: want %q, got %q", "res-abc", got)
	}
	if resp.ReservationID != "res-abc" || resp.Status != brokerv1alpha1.ReservationPhasePending {
		t.Errorf("response mismatch: %+v", resp)
	}
}

func TestPostReservation_RejectsEmptyKey(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("server should not be reached when reservationID is empty")
	})), Options{})
	if _, err := c.PostReservation(context.Background(), "", &brokerapi.ReservationRequest{}); err == nil {
		t.Fatal("expected error for empty reservationID")
	}
}

func TestDeleteReservation(t *testing.T) {
	fb := newFakeBroker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.ReleaseResponse{
			ReservationID:       "res-xyz",
			Status:              brokerv1alpha1.ReservationPhaseUnpeering,
			RemainingChunkCount: 0,
		})
	})

	resp, err := fb.client().DeleteReservation(context.Background(), "res-xyz")
	if err != nil {
		t.Fatalf("DeleteReservation: %v", err)
	}
	if fb.method != http.MethodDelete || fb.path != "/api/v1/reservations/res-xyz" {
		t.Errorf("wrong method/path: %s %s", fb.method, fb.path)
	}
	if resp.Status != brokerv1alpha1.ReservationPhaseUnpeering {
		t.Errorf("phase: want Unpeering, got %s", resp.Status)
	}
}

func TestDeleteReservation_RejectsEmptyID(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("server should not be reached when reservationID is empty")
	})), Options{})
	if _, err := c.DeleteReservation(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty reservationID")
	}
}

func TestGetInstructions(t *testing.T) {
	fb := newFakeBroker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.InstructionsResponse{
			Instructions: []brokerapi.InstructionView{
				{ID: "gk-res-1", Kind: "GenerateKubeconfig", ReservationID: "res-1"},
			},
		})
	})

	resp, err := fb.client().GetInstructions(context.Background())
	if err != nil {
		t.Fatalf("GetInstructions: %v", err)
	}
	if fb.method != http.MethodGet || fb.path != "/api/v1/instructions" {
		t.Errorf("wrong method/path: %s %s", fb.method, fb.path)
	}
	if len(resp.Instructions) != 1 || resp.Instructions[0].ID != "gk-res-1" {
		t.Errorf("decode mismatch: %+v", resp)
	}
}

func TestPostInstructionResult(t *testing.T) {
	fb := newFakeBroker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.InstructionResultResponse{Accepted: true})
	})

	err := fb.client().PostInstructionResult(context.Background(), "gk-res-1", &brokerapi.InstructionResultRequest{
		Status: brokerapi.ResultStatusSucceeded,
		Payload: &brokerapi.ResultPayload{
			Kind:       brokerapi.PayloadKindKubeconfig,
			Kubeconfig: "AAAA",
		},
	})
	if err != nil {
		t.Fatalf("PostInstructionResult: %v", err)
	}
	if fb.method != http.MethodPost || fb.path != "/api/v1/instructions/gk-res-1/result" {
		t.Errorf("wrong method/path: %s %s", fb.method, fb.path)
	}

	// Body must round-trip the payload kind.
	var sent brokerapi.InstructionResultRequest
	if err := json.Unmarshal(fb.body, &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent.Status != brokerapi.ResultStatusSucceeded {
		t.Errorf("sent status: want Succeeded, got %s", sent.Status)
	}
	if sent.Payload == nil || sent.Payload.Kind != brokerapi.PayloadKindKubeconfig {
		t.Errorf("sent payload: %+v", sent.Payload)
	}
}

func TestPostInstructionResult_RejectsEmptyID(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("server should not be reached when instructionID is empty")
	})), Options{})
	err := c.PostInstructionResult(context.Background(), "", &brokerapi.InstructionResultRequest{Status: brokerapi.ResultStatusSucceeded})
	if err == nil {
		t.Fatal("expected error for empty instructionID")
	}
}

// TestEndpoints_NilRequestBodyRejected covers the symmetrically-applied
// nil-guard on every method that requires a request struct.
func TestEndpoints_NilRequestBodyRejected(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("server should not be reached for nil-body cases")
	})), Options{})
	cases := []struct {
		name string
		fn   func() error
	}{
		{"PostAdvertisement", func() error {
			_, err := c.PostAdvertisement(context.Background(), nil)
			return err
		}},
		{"PostHeartbeat", func() error {
			_, err := c.PostHeartbeat(context.Background(), nil)
			return err
		}},
		{"PostReservation", func() error {
			_, err := c.PostReservation(context.Background(), "res-1", nil)
			return err
		}},
		{"PostInstructionResult", func() error {
			return c.PostInstructionResult(context.Background(), "i-1", nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err == nil {
				t.Fatal("expected error for nil request body")
			}
		})
	}
}
