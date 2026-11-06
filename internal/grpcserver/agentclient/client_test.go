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

package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// fakeLocalAPI captures the last incoming request so each endpoint
// test can assert method/path/body shape without rebuilding plumbing.
type fakeLocalAPI struct {
	mu      sync.Mutex
	srv     *httptest.Server
	method  string
	path    string
	headers http.Header
	body    []byte
}

func newFakeLocalAPI(t *testing.T, handler http.HandlerFunc) *fakeLocalAPI {
	t.Helper()
	fb := &fakeLocalAPI{}
	fb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.method = r.Method
		fb.path = r.URL.Path
		fb.headers = r.Header.Clone()
		fb.body, _ = io.ReadAll(r.Body)
		fb.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(fb.srv.Close)
	return fb
}

func (fb *fakeLocalAPI) snapshot() (method, path string, headers http.Header, body []byte) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.method, fb.path, fb.headers.Clone(), append([]byte(nil), fb.body...)
}

func (fb *fakeLocalAPI) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(Options{BaseURL: fb.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// -----------------------------------------------------------------------------
// Validation
// -----------------------------------------------------------------------------

func TestNew_Validation(t *testing.T) {
	cases := []struct {
		name   string
		opts   Options
		errSub string
	}{
		{"missing url", Options{}, "BaseURL is required"},
		{"bad scheme", Options{BaseURL: "ftp://foo"}, "must be http"},
		{"missing host", Options{BaseURL: "http://"}, "no host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("expected error containing %q, got %v", tc.errSub, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// GET /local/nodegroups
// -----------------------------------------------------------------------------

func TestGetNodeGroups(t *testing.T) {
	fb := newFakeLocalAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.NodeGroupListResponse{
			NodeGroups: []brokerapi.NodeGroupView{
				{ID: "p1/standard", ProviderClusterID: "p1", Type: brokerv1alpha1.ChunkTypeStandard, MaxSize: 4},
			},
			Generation: 7,
		})
	})

	resp, err := fb.client(t).GetNodeGroups(context.Background())
	if err != nil {
		t.Fatalf("GetNodeGroups: %v", err)
	}
	m, p, _, _ := fb.snapshot()
	if m != http.MethodGet || p != nodeGroupsPath {
		t.Errorf("wrong method/path: %s %s", m, p)
	}
	if len(resp.NodeGroups) != 1 || resp.NodeGroups[0].ID != "p1/standard" || resp.Generation != 7 {
		t.Errorf("decode mismatch: %+v", resp)
	}
}

// nodeGroupsPath is the local API route both node-group calls hit.
const nodeGroupsPath = "/local/nodegroups"

func TestGetNodeGroupsWithinSendsBound(t *testing.T) {
	var gotQuery string
	fb := newFakeLocalAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.NodeGroupListResponse{})
	})
	bound := &autoscalingv1alpha1.LatencyBound{MaxLatencyMs: 3}
	if _, err := fb.client(t).GetNodeGroupsWithin(context.Background(), bound); err != nil {
		t.Fatalf("GetNodeGroupsWithin: %v", err)
	}
	if _, p, _, _ := fb.snapshot(); p != nodeGroupsPath {
		t.Errorf("path = %q, want /local/nodegroups (query must not be folded into the path)", p)
	}
	q, _ := url.ParseQuery(gotQuery)
	decoded, err := localapi.DecodeLatencyBound(q)
	if err != nil || decoded == nil || decoded.MaxLatencyMs != 3 {
		t.Errorf("bound did not round-trip: query %q → %+v, %v", gotQuery, decoded, err)
	}

	// No bound: no query at all.
	if _, err := fb.client(t).GetNodeGroups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "" {
		t.Errorf("GetNodeGroups must send no query, got %q", gotQuery)
	}
}

// -----------------------------------------------------------------------------
// POST /local/reservations
// -----------------------------------------------------------------------------

func TestPostReservation_PropagatesIdempotencyKey(t *testing.T) {
	fb := newFakeLocalAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(brokerapi.ReservationResponse{
			ReservationID: "res-from-grpc",
			Status:        brokerv1alpha1.ReservationPhasePending,
			ChunkCount:    1,
			CreatedAt:     metav1.NewTime(metav1.Now().Time),
		})
	})

	resp, err := fb.client(t).PostReservation(context.Background(), "res-from-grpc",
		&brokerapi.ReservationRequest{
			ProviderClusterID: "p1",
			ChunkCount:        1,
			ChunkType:         brokerv1alpha1.ChunkTypeStandard,
		})
	if err != nil {
		t.Fatalf("PostReservation: %v", err)
	}
	m, p, hdrs, body := fb.snapshot()
	if m != http.MethodPost || p != "/local/reservations" {
		t.Errorf("wrong method/path: %s %s", m, p)
	}
	if got := hdrs.Get(brokerapi.HeaderReservationID); got != "res-from-grpc" {
		t.Errorf("X-Reservation-Id: want res-from-grpc, got %q", got)
	}
	var sent brokerapi.ReservationRequest
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent.ProviderClusterID != "p1" {
		t.Errorf("sent body mismatch: %+v", sent)
	}
	if resp.Status != brokerv1alpha1.ReservationPhasePending {
		t.Errorf("response phase mismatch: %s", resp.Status)
	}
}

func TestPostReservation_RejectsEmptyKey(t *testing.T) {
	fb := newFakeLocalAPI(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("server should not be reached")
	})
	if _, err := fb.client(t).PostReservation(context.Background(), "", &brokerapi.ReservationRequest{}); err == nil {
		t.Fatal("expected error for empty reservationID")
	}
}

func TestPostReservation_RejectsNilBody(t *testing.T) {
	fb := newFakeLocalAPI(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("server should not be reached")
	})
	if _, err := fb.client(t).PostReservation(context.Background(), "res-1", nil); err == nil {
		t.Fatal("expected error for nil body")
	}
}

// -----------------------------------------------------------------------------
// DELETE /local/reservations/{id}
// -----------------------------------------------------------------------------

func TestDeleteReservation(t *testing.T) {
	fb := newFakeLocalAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.ReleaseResponse{
			ReservationID:       "res-xyz",
			Status:              brokerv1alpha1.ReservationPhaseUnpeering,
			RemainingChunkCount: 0,
		})
	})

	resp, err := fb.client(t).DeleteReservation(context.Background(), "res-xyz")
	if err != nil {
		t.Fatalf("DeleteReservation: %v", err)
	}
	m, p, _, _ := fb.snapshot()
	if m != http.MethodDelete || p != "/local/reservations/res-xyz" {
		t.Errorf("wrong method/path: %s %s", m, p)
	}
	if resp.Status != brokerv1alpha1.ReservationPhaseUnpeering {
		t.Errorf("phase mismatch: %s", resp.Status)
	}
}

func TestDeleteReservation_RejectsEmptyID(t *testing.T) {
	fb := newFakeLocalAPI(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("server should not be reached")
	})
	if _, err := fb.client(t).DeleteReservation(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty reservationID")
	}
}

// -----------------------------------------------------------------------------
// GET /local/virtual-nodes
// -----------------------------------------------------------------------------

func TestGetVirtualNodes(t *testing.T) {
	fb := newFakeLocalAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(localapi.VirtualNodeListResponse{
			VirtualNodes: []localapi.VirtualNodeView{
				{Name: "liqo-virt-1", ReservationID: "res-1", NodeGroupID: "p1/standard"},
			},
		})
	})

	resp, err := fb.client(t).GetVirtualNodes(context.Background())
	if err != nil {
		t.Fatalf("GetVirtualNodes: %v", err)
	}
	m, p, _, _ := fb.snapshot()
	if m != http.MethodGet || p != "/local/virtual-nodes" {
		t.Errorf("wrong method/path: %s %s", m, p)
	}
	if len(resp.VirtualNodes) != 1 || resp.VirtualNodes[0].Name != "liqo-virt-1" {
		t.Errorf("decode mismatch: %+v", resp)
	}
}

// -----------------------------------------------------------------------------
// Error mapping
// -----------------------------------------------------------------------------

func TestErrorBody_Classification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		code      brokerapi.ErrorCode
		predicate func(error) bool
	}{
		{"bad request", http.StatusBadRequest, brokerapi.ErrCodeInvalidRequest, IsBadRequest},
		{"forbidden", http.StatusForbidden, brokerapi.ErrCodeForbidden, IsForbidden},
		{"not found", http.StatusNotFound, brokerapi.ErrCodeNotFound, IsNotFound},
		{"conflict", http.StatusConflict, brokerapi.ErrCodeConflict, IsConflict},
		{"precondition", http.StatusPreconditionFailed, brokerapi.ErrCodeServiceUnavailable, IsPreconditionFailed},
		{"too many", http.StatusTooManyRequests, brokerapi.ErrCodeTooManyRequests, IsTooManyRequests},
		{"transient", http.StatusBadGateway, brokerapi.ErrCodeUpstreamError, IsTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := newFakeLocalAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(brokerapi.ErrorResponse{
					Code: tc.code, Message: tc.name, RequestID: "rid-1",
				})
			})
			_, err := fb.client(t).GetNodeGroups(context.Background())
			if !tc.predicate(err) {
				t.Fatalf("predicate did not match for %s: err=%v", tc.name, err)
			}
			var typed *Error
			if !errors.As(err, &typed) {
				t.Fatalf("expected *Error, got %T", err)
			}
			if typed.Status != tc.status {
				t.Errorf("status: want %d, got %d", tc.status, typed.Status)
			}
			if typed.Code != tc.code {
				t.Errorf("code: want %s, got %s", tc.code, typed.Code)
			}
			if typed.RequestID != "rid-1" {
				t.Errorf("request id: want rid-1, got %q", typed.RequestID)
			}
		})
	}
}

func TestErrorBody_EmptyResponse(t *testing.T) {
	fb := newFakeLocalAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	_, err := fb.client(t).GetNodeGroups(context.Background())
	if !IsBadRequest(err) {
		t.Fatalf("want bad request, got %v", err)
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.Message == "" {
		t.Fatalf("expected non-empty fallback message, got %v", err)
	}
}

func TestNetworkError_Transient(t *testing.T) {
	// Build a client pointing at a never-listening port; Dial fails.
	c, err := New(Options{BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetNodeGroups(context.Background())
	if !IsTransient(err) {
		t.Fatalf("want transient, got %v", err)
	}
}
