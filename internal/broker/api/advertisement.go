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
	"errors"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/broker/chunk"
)

// advertisementReportInterval is the cadence the agent should next call POST
// /api/v1/advertisements. It MUST stay in sync with the agent's hard-coded
// 30 s tick (docs/design.md §7.3.1).
const advertisementReportInterval = "30s"

// -----------------------------------------------------------------------------
// 7.3.1 — POST /api/v1/advertisements
// -----------------------------------------------------------------------------

// handleAdvertisementUpsert ingests a Provider Agent's 30-s advertisement,
// upserts the matching ClusterAdvertisement CR, and returns the chunk
// breakdown so the agent's logs match the Broker's truth.
//
// Auth model: ClusterIDMiddleware has already pinned the caller's cluster
// from its mTLS leaf cert. Here we double-check that body.ClusterID matches
// — a mismatch is a 403, not a 400, because it implies the caller is
// trying to advertise on behalf of someone else.
func (s *Server) handleAdvertisementUpsert(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := r.Header.Get(HeaderRequestID)
	clusterID := ClusterIDFromContext(ctx)

	var req AdvertisementRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrorResponse{
			Code: ErrCodeInvalidRequest, Message: err.Error(), RequestID: requestID,
		})
		return
	}
	if err := validateAdvertisementRequest(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrorResponse{
			Code: ErrCodeInvalidRequest, Message: err.Error(), RequestID: requestID,
		})
		return
	}
	if req.ClusterID != clusterID {
		writeError(w, http.StatusForbidden, ErrorResponse{
			Code: ErrCodeForbidden,
			Message: fmt.Sprintf("body.clusterId %q does not match certificate CN %q",
				req.ClusterID, clusterID),
			RequestID: requestID,
		})
		return
	}

	// Pick the first sizer result. DefaultSizer always returns exactly one.
	// When ConfigMap-driven sizing produces multiple groups we'll iterate.
	results := s.sizer.Size(req.Resources)
	if len(results) == 0 {
		writeError(w, http.StatusBadRequest, ErrorResponse{
			Code:      ErrCodeInsufficientCapacity,
			Message:   "advertised resources do not yield even one full chunk",
			RequestID: requestID,
		})
		return
	}
	chunkResult := results[0]

	cadv, err := s.upsertClusterAdvertisement(ctx, &req, chunkResult)
	if err != nil {
		s.log.Error(err, "upsert ClusterAdvertisement failed",
			"clusterId", clusterID, "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code: ErrCodeInternalError, Message: "advertisement persistence failed",
			RequestID: requestID,
		})
		return
	}

	// Status update is best-effort: a failed write only delays the
	// "Available" gate. The next 30 s advertisement re-tries it.
	if err := s.touchClusterAdvertisementStatus(ctx, cadv, chunkResult); err != nil {
		s.log.Info("status update failed (will retry next advertisement)",
			"err", err.Error(), "clusterId", clusterID, "requestId", requestID)
	}

	// Piggyback any pending instructions for this provider onto the
	// advertisement response (docs/design.md §7.3.1) so common-case
	// instruction latency is bounded by the 30 s advertisement cadence.
	instr, err := s.collectInstructionsForCaller(ctx, clusterID)
	if err != nil {
		s.log.Info("instruction piggyback skipped",
			"err", err.Error(), "clusterId", clusterID, "requestId", requestID)
		instr = nil
	}

	writeJSON(w, http.StatusOK, AdvertisementResponse{
		Accepted:       true,
		ChunkCount:     chunkResult.Count,
		ChunkResources: chunkResult.PerChunk,
		NextReportIn:   advertisementReportInterval,
		Instructions:   instr,
	})
}

// validateAdvertisementRequest enforces the minimum schema invariants the
// CRD validators would also catch — surfacing them as 400s here saves a
// kube-apiserver round trip.
func validateAdvertisementRequest(req *AdvertisementRequest) error {
	switch {
	case req.ClusterID == "":
		return errors.New("clusterId is required")
	case req.LiqoClusterID == "":
		return errors.New("liqoClusterId is required")
	case len(req.Resources) == 0:
		return errors.New("resources must not be empty")
	}
	return nil
}

// upsertClusterAdvertisement reconciles the request body into a CR named
// after the calling cluster. Spec is fully overwritten on every call (the
// provider is the source of truth); status is left untouched here and
// updated separately via the /status subresource.
func (s *Server) upsertClusterAdvertisement(
	ctx context.Context, req *AdvertisementRequest, chunkResult chunk.Result,
) (*brokerv1alpha1.ClusterAdvertisement, error) {
	cadv := &brokerv1alpha1.ClusterAdvertisement{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.ClusterID,
			Namespace: s.namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, s.client, cadv, func() error {
		cadv.Spec = brokerv1alpha1.ClusterAdvertisementSpec{
			ClusterID:     req.ClusterID,
			LiqoClusterID: req.LiqoClusterID,
			ClusterType:   chunkResult.Type,
			Resources: brokerv1alpha1.AdvertisedResources{
				Allocatable: req.Resources,
			},
			Topology: req.Topology,
			Price:    req.Price,
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("createOrUpdate %s/%s: %w",
			cadv.Namespace, cadv.Name, err)
	}
	return cadv, nil
}

// touchClusterAdvertisementStatus refreshes LastSeen / Available and the
// chunk counts. Reservation accounting (ReservedChunks) is owned by the
// Reservation reconciler; we only ever set TotalChunks and recompute
// AvailableChunks based on the live ReservedChunks we read back.
func (s *Server) touchClusterAdvertisementStatus(
	ctx context.Context, cadv *brokerv1alpha1.ClusterAdvertisement, chunkResult chunk.Result,
) error {
	now := metav1.Now()
	patch := cadv.DeepCopy()

	patch.Status.ObservedGeneration = cadv.Generation
	patch.Status.LastSeen = &now
	patch.Status.Available = true
	patch.Status.TotalChunks = chunkResult.Count

	available := chunkResult.Count - patch.Status.ReservedChunks
	if available < 0 {
		available = 0
	}
	patch.Status.AvailableChunks = available

	return s.client.Status().Update(ctx, patch)
}

// -----------------------------------------------------------------------------
// 7.3.2 — GET /api/v1/advertisements/{clusterId}
// -----------------------------------------------------------------------------

// handleAdvertisementGet returns the Broker's authoritative view of one
// advertisement so the Provider Agent can preserve Broker-managed fields
// (currently ReservedChunks) across re-submissions.
func (s *Server) handleAdvertisementGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := r.Header.Get(HeaderRequestID)
	pathClusterID := r.PathValue("clusterId")
	authClusterID := ClusterIDFromContext(ctx)

	if pathClusterID == "" {
		writeError(w, http.StatusBadRequest, ErrorResponse{
			Code: ErrCodeInvalidRequest, Message: "clusterId is required",
			RequestID: requestID,
		})
		return
	}
	if pathClusterID != authClusterID {
		writeError(w, http.StatusForbidden, ErrorResponse{
			Code: ErrCodeForbidden,
			Message: fmt.Sprintf("path clusterId %q does not match certificate CN %q",
				pathClusterID, authClusterID),
			RequestID: requestID,
		})
		return
	}

	var cadv brokerv1alpha1.ClusterAdvertisement
	key := types.NamespacedName{Name: pathClusterID, Namespace: s.namespace}
	if err := s.client.Get(ctx, key, &cadv); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, ErrorResponse{
				Code:      ErrCodeNotFound,
				Message:   fmt.Sprintf("no advertisement for cluster %q", pathClusterID),
				RequestID: requestID,
			})
			return
		}
		s.log.Error(err, "get ClusterAdvertisement failed",
			"clusterId", pathClusterID, "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code: ErrCodeInternalError, Message: "advertisement read failed",
			RequestID: requestID,
		})
		return
	}

	writeJSON(w, http.StatusOK, advertisementSnapshotFromCR(&cadv))
}

func advertisementSnapshotFromCR(cadv *brokerv1alpha1.ClusterAdvertisement) AdvertisementSnapshot {
	out := AdvertisementSnapshot{
		ClusterID:       cadv.Spec.ClusterID,
		LiqoClusterID:   cadv.Spec.LiqoClusterID,
		Resources:       cadv.Spec.Resources.Allocatable,
		Topology:        cadv.Spec.Topology,
		Price:           cadv.Spec.Price,
		ChunkCount:      cadv.Status.TotalChunks,
		ReservedChunks:  cadv.Status.ReservedChunks,
		AvailableChunks: cadv.Status.AvailableChunks,
		Available:       cadv.Status.Available,
	}
	if cadv.Status.LastSeen != nil {
		out.LastSeen = *cadv.Status.LastSeen
	}
	// LiqoLabels / LiqoTaints aren't on the spec yet (designed but not in
	// the CRD types as of step 2). When they land, populate them here.
	return out
}

// -----------------------------------------------------------------------------
// Shared decode helper
// -----------------------------------------------------------------------------

// decodeJSONBody is a small helper that enforces strict JSON decoding
// (UnknownFields is an error). Keeps every handler one liner away from
// proper 400 responses without sprinkling json.Decoder boilerplate.
func decodeJSONBody(r *http.Request, out any) error {
	if r.Body == nil {
		return errors.New("request body is empty")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}
