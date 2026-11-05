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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// listResv builds a one-chunk standard Reservation owned by consumer on provider.
func listResv(name, consumer, provider string, phase brokerv1alpha1.ReservationPhase) *brokerv1alpha1.Reservation {
	return &brokerv1alpha1.Reservation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: dashboardTestNS},
		Spec: brokerv1alpha1.ReservationSpec{
			ConsumerClusterID: consumer,
			ProviderClusterID: provider,
			ChunkCount:        1,
			ChunkType:         brokerv1alpha1.ChunkTypeStandard,
		},
		Status: brokerv1alpha1.ReservationStatus{Phase: phase},
	}
}

// callReservationList drives GET /api/v1/reservations through the real mux as
// the given cluster (identity on the context, mirroring ClusterIDMiddleware).
func callReservationList(t *testing.T, s *Server, callerID string) (ReservationListResponse, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reservations", nil)
	req = req.WithContext(NewContextWithClusterID(req.Context(), callerID))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out ReservationListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out, rec.Body.String()
}

// TestNodeGroupsReportOwnChunksPerConsumer: two consumers share provider p1
// (3 chunks, 2 reserved: one each). Each consumer's view must report only its
// own chunk as CurrentReserved (the Cluster Autoscaler's target size), while
// the head-room stays the provider's 1 free chunk for both.
func TestNodeGroupsReportOwnChunksPerConsumer(t *testing.T) {
	s := newDashboardTestServer(t,
		stdAdv("p1", 2, nil),
		listResv("res-a", consumerCluster, "p1", brokerv1alpha1.ReservationPhasePeered),
		listResv("res-b", "consumer-b", "p1", brokerv1alpha1.ReservationPhasePeered),
		listResv("res-a-old", consumerCluster, "p1", brokerv1alpha1.ReservationPhaseReleased),
	)
	for _, consumer := range []string{consumerCluster, "consumer-b"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/nodegroups", nil)
		req = req.WithContext(NewContextWithClusterID(req.Context(), consumer))
		rec := httptest.NewRecorder()
		s.handleNodeGroupsList(rec, req)
		var out NodeGroupListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.NodeGroups) != 1 {
			t.Fatalf("%s: status %d, body %s", consumer, rec.Code, rec.Body.String())
		}
		v := out.NodeGroups[0]
		if v.CurrentReserved != 1 || v.MaxSize != 2 {
			t.Errorf("%s: CurrentReserved/MaxSize = %d/%d, want 1/2 (own chunk + 1 free)",
				consumer, v.CurrentReserved, v.MaxSize)
		}
	}
}

func TestReservationList(t *testing.T) {
	s := newDashboardTestServer(t,
		listResv("res-b", consumerCluster, "p1", brokerv1alpha1.ReservationPhasePeering),
		listResv("res-a", consumerCluster, "p2", brokerv1alpha1.ReservationPhasePeered),
		listResv("res-c", consumerCluster, "p1", brokerv1alpha1.ReservationPhaseReleased),
		listResv("res-other", "consumer-b", "p1", brokerv1alpha1.ReservationPhasePeered),
	)

	t.Run("returns only the caller's reservations, every phase, sorted by ID", func(t *testing.T) {
		out, _ := callReservationList(t, s, consumerCluster)
		var ids []string
		for _, r := range out.Reservations {
			ids = append(ids, r.ReservationID)
			if r.ConsumerClusterID != consumerCluster {
				t.Errorf("%s belongs to %q, not the caller", r.ReservationID, r.ConsumerClusterID)
			}
		}
		if got, want := strings.Join(ids, ","), "res-a,res-b,res-c"; got != want {
			t.Errorf("ids = %s, want %s", got, want)
		}
		if out.Reservations[1].Status != brokerv1alpha1.ReservationPhasePeering ||
			out.Reservations[1].ProviderClusterID != "p1" {
			t.Errorf("res-b fields not mapped: %+v", out.Reservations[1])
		}
	})

	t.Run("a provider (never a consumer) gets an empty list, not null", func(t *testing.T) {
		out, raw := callReservationList(t, s, "p1")
		if len(out.Reservations) != 0 {
			t.Errorf("want no reservations, got %+v", out.Reservations)
		}
		if !strings.Contains(raw, `"reservations":[]`) {
			t.Errorf("empty list must encode as [], body = %s", raw)
		}
	})
}
