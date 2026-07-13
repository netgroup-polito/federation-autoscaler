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

// dashboard.go implements the Broker's read-only web dashboard surface: a
// single aggregate Overview projection of all Broker state plus the handlers
// that serve it. It is served on a SEPARATE, plain-HTTP listener (see
// dashboard_runnable.go) — never on the mTLS API port — so a browser can reach
// it without a client certificate.
//
// Everything here is strictly read-only: buildOverview only List()s the four
// Broker-owned CRDs and snapshots the in-memory consumer registry. It performs
// no writes, advances no phases, and — unlike the agent-facing instruction
// path (collectInstructionsForCaller) — never inlines or exposes peering-user
// kubeconfig material.

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// dashboardIndexHTML is the self-contained single-page dashboard, embedded into
// the broker binary so there is no static-asset directory to ship.
//
//go:embed dashboard_assets/index.html
var dashboardIndexHTML []byte

// -----------------------------------------------------------------------------
// Wire types
// -----------------------------------------------------------------------------

// Overview is the read-only aggregate the dashboard renders. It is assembled
// fresh on every GET /api/v1/overview from the four Broker-owned CRDs plus the
// in-memory consumer registry — a pure projection of live state.
type Overview struct {
	GeneratedAt             metav1.Time                `json:"generatedAt"`
	Namespace               string                     `json:"namespace"`
	Capacity                CapacitySummary            `json:"capacity"`
	Advertisements          []AdvertisementSnapshot    `json:"advertisements"`
	Reservations            []DashboardReservationView `json:"reservations"`
	ProviderInstructions    []DashboardInstructionView `json:"providerInstructions"`
	ReservationInstructions []DashboardInstructionView `json:"reservationInstructions"`
	Consumers               []ConsumerView             `json:"consumers"`
}

// CapacitySummary is the federation-wide chunk budget rolled up across every
// advertised provider, plus a per-chunk-type breakdown.
type CapacitySummary struct {
	TotalChunks            int32                        `json:"totalChunks"`
	ReservedChunks         int32                        `json:"reservedChunks"`
	AvailableChunks        int32                        `json:"availableChunks"`
	ProviderCount          int                          `json:"providerCount"`
	AvailableProviderCount int                          `json:"availableProviderCount"`
	ByChunkType            map[string]ChunkTypeCapacity `json:"byChunkType"`

	// ChunkSizes is the canonical per-chunk shape the Broker's sizer assigns
	// to each chunk type (one chunk's worth of cpu/memory/gpu), keyed by chunk
	// type. Independent of what providers currently advertise — it is the
	// federation's fixed chunk definition, so it always carries both
	// "standard" and "gpu".
	ChunkSizes map[string]corev1.ResourceList `json:"chunkSizes"`
}

// ChunkTypeCapacity is one row of CapacitySummary.ByChunkType.
type ChunkTypeCapacity struct {
	TotalChunks     int32 `json:"totalChunks"`
	ReservedChunks  int32 `json:"reservedChunks"`
	AvailableChunks int32 `json:"availableChunks"`
}

// DashboardReservationView reuses the agent-facing ReservationResponse (so the
// full phase machine, consumer/provider identity, chunk shape and virtual-node
// names come for free) and adds the terminal-GC timestamp the dashboard shows.
// ReservationResponse is embedded anonymously so encoding/json inlines its
// fields at the top level.
type DashboardReservationView struct {
	ReservationResponse
	TerminatedAt *metav1.Time `json:"terminatedAt,omitempty"`
}

// DashboardInstructionView is the read-only, dashboard-specific projection of a
// ProviderInstruction or ReservationInstruction. Unlike the agent-facing
// InstructionView it carries the full status phase machine (Enforced, Attempts,
// the delivery timestamps, Message) and, deliberately, NO kubeconfig field —
// peering-user credentials must never be exposed on the unauthenticated
// dashboard port.
type DashboardInstructionView struct {
	ID              string       `json:"id"`
	Kind            string       `json:"kind"`
	ReservationID   string       `json:"reservationId"`
	TargetClusterID string       `json:"targetClusterId"`
	ChunkCount      int32        `json:"chunkCount,omitempty"`
	LastChunk       bool         `json:"lastChunk,omitempty"`
	Enforced        bool         `json:"enforced"`
	Attempts        int32        `json:"attempts,omitempty"`
	IssuedAt        *metav1.Time `json:"issuedAt,omitempty"`
	LastDeliveredAt *metav1.Time `json:"lastDeliveredAt,omitempty"`
	LastUpdateTime  *metav1.Time `json:"lastUpdateTime,omitempty"`
	ExpiresAt       *metav1.Time `json:"expiresAt,omitempty"`
	Message         string       `json:"message,omitempty"`

	// Set on ProviderInstructions (kind ∈ GenerateKubeconfig|Cleanup|Reconcile).
	ConsumerClusterID     string `json:"consumerClusterId,omitempty"`
	ConsumerLiqoClusterID string `json:"consumerLiqoClusterId,omitempty"`

	// Set on ReservationInstructions (kind ∈ Peer|Unpeer|Cleanup|Reconcile).
	ProviderClusterID  string   `json:"providerClusterId,omitempty"`
	ResourceSliceNames []string `json:"resourceSliceNames,omitempty"`
}

// ConsumerView is one row of the in-memory consumer registry: a cluster that
// has heartbeated since this Broker pod started.
type ConsumerView struct {
	ClusterID     string `json:"clusterId"`
	LiqoClusterID string `json:"liqoClusterId"`
	// Placement is the consumer's pushed placement-policy type (e.g. "Price",
	// "Eco", "Latency"). Empty means no preference (Broker default).
	Placement string `json:"placement,omitempty"`
	// Region is the consumer's pushed region (may be empty); the latency strategy
	// measures provider distances from this consumer's coordinates.
	Region string `json:"region,omitempty"`
	// City is the consumer's auto-discovered city (may be empty); informational.
	City     string      `json:"city,omitempty"`
	LastSeen metav1.Time `json:"lastSeen"`
}

// -----------------------------------------------------------------------------
// Overview assembly (read-only)
// -----------------------------------------------------------------------------

// buildOverview lists every Broker-owned CR in the namespace (no per-caller
// filtering — the dashboard shows global state, including enforced instructions
// and stale providers) plus the consumer registry, and projects them onto the
// read-only Overview. It performs only reads.
func (s *Server) buildOverview(ctx context.Context) (Overview, error) {
	ov := Overview{
		GeneratedAt: metav1.Now(),
		Namespace:   s.namespace,
		Capacity:    CapacitySummary{ByChunkType: map[string]ChunkTypeCapacity{}},
	}
	ov.Capacity.ChunkSizes = s.chunkSizes()

	// Advertisements + capacity aggregates. The Status.{Total,Reserved,
	// Available}Chunks counters are the authoritative values the
	// ClusterAdvertisement reconciler maintains, so the dashboard totals match
	// `kubectl get cadv`.
	var cadvs brokerv1alpha1.ClusterAdvertisementList
	if err := s.client.List(ctx, &cadvs, client.InNamespace(s.namespace)); err != nil {
		return Overview{}, fmt.Errorf("list ClusterAdvertisement: %w", err)
	}
	ov.Advertisements = make([]AdvertisementSnapshot, 0, len(cadvs.Items))
	for i := range cadvs.Items {
		cadv := &cadvs.Items[i]
		snap := advertisementSnapshotFromCR(cadv)
		if cost, ok := s.perChunkCost(cadv); ok {
			snap.CostPerChunk = &cost
		}
		ov.Advertisements = append(ov.Advertisements, snap)

		ov.Capacity.ProviderCount++
		if cadv.Status.Available {
			ov.Capacity.AvailableProviderCount++
		}
		ov.Capacity.TotalChunks += cadv.Status.TotalChunks
		ov.Capacity.ReservedChunks += cadv.Status.ReservedChunks
		ov.Capacity.AvailableChunks += cadv.Status.AvailableChunks

		key := string(cadv.Spec.ClusterType)
		bucket := ov.Capacity.ByChunkType[key]
		bucket.TotalChunks += cadv.Status.TotalChunks
		bucket.ReservedChunks += cadv.Status.ReservedChunks
		bucket.AvailableChunks += cadv.Status.AvailableChunks
		ov.Capacity.ByChunkType[key] = bucket
	}
	sort.Slice(ov.Advertisements, func(i, j int) bool {
		return ov.Advertisements[i].ClusterID < ov.Advertisements[j].ClusterID
	})

	// Reservations.
	var resvs brokerv1alpha1.ReservationList
	if err := s.client.List(ctx, &resvs, client.InNamespace(s.namespace)); err != nil {
		return Overview{}, fmt.Errorf("list Reservation: %w", err)
	}
	ov.Reservations = make([]DashboardReservationView, 0, len(resvs.Items))
	for i := range resvs.Items {
		ov.Reservations = append(ov.Reservations, dashboardReservationView(&resvs.Items[i]))
	}
	sort.Slice(ov.Reservations, func(i, j int) bool {
		return ov.Reservations[i].ReservationID < ov.Reservations[j].ReservationID
	})

	// Provider instructions (all of them — enforced included).
	var pis autoscalingv1alpha1.ProviderInstructionList
	if err := s.client.List(ctx, &pis, client.InNamespace(s.namespace)); err != nil {
		return Overview{}, fmt.Errorf("list ProviderInstruction: %w", err)
	}
	ov.ProviderInstructions = make([]DashboardInstructionView, 0, len(pis.Items))
	for i := range pis.Items {
		ov.ProviderInstructions = append(ov.ProviderInstructions, dashboardViewFromProviderInstruction(&pis.Items[i]))
	}
	sort.Slice(ov.ProviderInstructions, func(i, j int) bool {
		return ov.ProviderInstructions[i].ID < ov.ProviderInstructions[j].ID
	})

	// Reservation instructions (all of them — enforced included).
	var ris autoscalingv1alpha1.ReservationInstructionList
	if err := s.client.List(ctx, &ris, client.InNamespace(s.namespace)); err != nil {
		return Overview{}, fmt.Errorf("list ReservationInstruction: %w", err)
	}
	ov.ReservationInstructions = make([]DashboardInstructionView, 0, len(ris.Items))
	for i := range ris.Items {
		ov.ReservationInstructions = append(ov.ReservationInstructions, dashboardViewFromReservationInstruction(&ris.Items[i]))
	}
	sort.Slice(ov.ReservationInstructions, func(i, j int) bool {
		return ov.ReservationInstructions[i].ID < ov.ReservationInstructions[j].ID
	})

	// Consumers (in-memory registry snapshot, already sorted by ClusterID).
	entries := s.consumers.Snapshot()
	ov.Consumers = make([]ConsumerView, 0, len(entries))
	for _, e := range entries {
		ov.Consumers = append(ov.Consumers, ConsumerView{
			ClusterID:     e.ClusterID,
			LiqoClusterID: e.LiqoClusterID,
			Placement:     string(e.Placement.Type),
			Region:        e.Region,
			City:          e.City,
			LastSeen:      metav1.NewTime(e.LastSeen),
		})
	}

	return ov, nil
}

// chunkSizes returns the canonical per-chunk shape the Broker's sizer assigns
// to each chunk type, keyed by chunk type string. It asks the live sizer
// (rather than re-hardcoding the figures) by classifying a reference
// advertisement large enough to yield a chunk of each kind — Size always
// returns the fixed PerChunk regardless of how many chunks fit. A non-GPU
// reference yields the standard size; a GPU reference yields the gpu size.
func (s *Server) chunkSizes() map[string]corev1.ResourceList {
	out := map[string]corev1.ResourceList{}

	standardRef := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1000"),
		corev1.ResourceMemory: resource.MustParse("1000Gi"),
	}
	gpuRef := corev1.ResourceList{
		corev1.ResourceCPU:                    resource.MustParse("1000"),
		corev1.ResourceMemory:                 resource.MustParse("1000Gi"),
		corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1000"),
	}

	for _, r := range s.sizer.Size(standardRef) {
		out[string(r.Type)] = r.PerChunk
	}
	for _, r := range s.sizer.Size(gpuRef) {
		out[string(r.Type)] = r.PerChunk
	}
	return out
}

// dashboardReservationView projects a Reservation CR onto the dashboard view,
// reusing reservationResponseFromCR and surfacing the terminal-GC timestamp.
func dashboardReservationView(r *brokerv1alpha1.Reservation) DashboardReservationView {
	return DashboardReservationView{
		ReservationResponse: reservationResponseFromCR(r),
		TerminatedAt:        r.Status.TerminatedAt,
	}
}

// dashboardViewFromProviderInstruction is the read-only counterpart of
// viewFromProviderInstruction: it copies the full status phase machine and
// performs no writes (no touchInstructionDelivered) and no kubeconfig inlining.
func dashboardViewFromProviderInstruction(pi *autoscalingv1alpha1.ProviderInstruction) DashboardInstructionView {
	return DashboardInstructionView{
		ID:                    pi.Name,
		Kind:                  string(pi.Spec.Kind),
		ReservationID:         pi.Spec.ReservationID,
		TargetClusterID:       pi.Spec.TargetClusterID,
		ChunkCount:            pi.Spec.ChunkCount,
		LastChunk:             pi.Spec.LastChunk,
		Enforced:              pi.Status.Enforced,
		Attempts:              pi.Status.Attempts,
		IssuedAt:              pi.Status.IssuedAt,
		LastDeliveredAt:       pi.Status.LastDeliveredAt,
		LastUpdateTime:        pi.Status.LastUpdateTime,
		ExpiresAt:             pi.Spec.ExpiresAt,
		Message:               pi.Status.Message,
		ConsumerClusterID:     pi.Spec.ConsumerClusterID,
		ConsumerLiqoClusterID: pi.Spec.ConsumerLiqoClusterID,
	}
}

// dashboardViewFromReservationInstruction is the read-only counterpart of
// viewFromReservationInstruction. Same contract: full status, no writes, and
// no kubeconfig (KubeconfigRef/inlined bytes are intentionally omitted).
func dashboardViewFromReservationInstruction(ri *autoscalingv1alpha1.ReservationInstruction) DashboardInstructionView {
	return DashboardInstructionView{
		ID:                 ri.Name,
		Kind:               string(ri.Spec.Kind),
		ReservationID:      ri.Spec.ReservationID,
		TargetClusterID:    ri.Spec.TargetClusterID,
		ChunkCount:         ri.Spec.ChunkCount,
		LastChunk:          ri.Spec.LastChunk,
		Enforced:           ri.Status.Enforced,
		Attempts:           ri.Status.Attempts,
		IssuedAt:           ri.Status.IssuedAt,
		LastDeliveredAt:    ri.Status.LastDeliveredAt,
		LastUpdateTime:     ri.Status.LastUpdateTime,
		ExpiresAt:          ri.Spec.ExpiresAt,
		Message:            ri.Status.Message,
		ProviderClusterID:  ri.Spec.ProviderClusterID,
		ResourceSliceNames: ri.Spec.ResourceSliceNames,
	}
}

// -----------------------------------------------------------------------------
// Handlers + dashboard-only routing
// -----------------------------------------------------------------------------

// handleOverview serves GET /api/v1/overview: a read-only JSON snapshot of all
// Broker state. Served only on the dashboard listener (no authentication).
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	requestID := r.Header.Get(HeaderRequestID)
	ov, err := s.buildOverview(r.Context())
	if err != nil {
		s.log.Error(err, "build dashboard overview failed", "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code: ErrCodeInternalError, Message: "overview read failed", RequestID: requestID,
		})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, ov)
}

// handleDashboardIndex serves the embedded single-page dashboard at exactly
// "/". ServeMux's "GET /" pattern is a catch-all for otherwise-unmatched paths,
// so anything other than the root is answered with 404 rather than silently
// returning the page (which would mask a mistyped URL).
func (s *Server) handleDashboardIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(dashboardIndexHTML)
}

// DashboardHandler returns the bare mux for the read-only dashboard listener:
// the embedded page, the JSON overview, and a health probe. It deliberately
// exposes NONE of the mTLS-authenticated routes from Handler().
func (s *Server) DashboardHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/overview", s.handleOverview)
	mux.HandleFunc("GET /", s.handleDashboardIndex)
	return mux
}

// DashboardMiddlewareChain wraps the dashboard mux with only the safe, auth-free
// middleware: panic recovery, request-ID propagation, and request logging. It
// intentionally OMITS ClusterIDMiddleware (there is no client certificate on
// this listener) and RateLimitMiddleware (which keys off the cluster ID that
// ClusterIDMiddleware would have extracted).
func (s *Server) DashboardMiddlewareChain(handler http.Handler) http.Handler {
	return chain(handler,
		s.RecoverMiddleware,
		s.RequestIDMiddleware,
		s.LoggerMiddleware,
	)
}
