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
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// defaultReservationTimeout is the deadline by which a reservation must
// reach Peered (docs/design.md §6 — chunk-config ConfigMap default 5m).
// Step 4h surfaces the ConfigMap-backed value; 4g hard-codes it.
const defaultReservationTimeout = 5 * time.Minute

// nodeGroupID returns the canonical node-group identifier the Broker
// surfaces to consumers (`ng-<provider>-<chunkType>`). Used to validate the
// `nodeGroupId` field clients send in the reservation request body.
func nodeGroupID(providerClusterID string, chunkType brokerv1alpha1.ChunkType) string {
	return fmt.Sprintf("ng-%s-%s", providerClusterID, strings.ToLower(string(chunkType)))
}

// -----------------------------------------------------------------------------
// 7.3.5 — POST /api/v1/reservations   (Consumer Agent → Broker, synchronous)
// -----------------------------------------------------------------------------

func (s *Server) handleReservationCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := r.Header.Get(HeaderRequestID)
	consumerID := ClusterIDFromContext(ctx)

	var req ReservationRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrorResponse{
			Code: ErrCodeInvalidRequest, Message: err.Error(), RequestID: requestID,
		})
		return
	}
	if err := validateReservationRequest(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrorResponse{
			Code: ErrCodeInvalidRequest, Message: err.Error(), RequestID: requestID,
		})
		return
	}

	// Consumer must have heartbeated at least once so we know its Liqo
	// cluster ID. 412 is the canonical "missing prerequisite" status.
	consumerEntry, ok := s.consumers.Lookup(consumerID)
	if !ok {
		writeError(w, http.StatusPreconditionFailed, ErrorResponse{
			Code:      ErrCodeServiceUnavailable,
			Message:   "no heartbeat received for this consumer; send POST /api/v1/heartbeat first",
			RequestID: requestID,
		})
		return
	}

	// Resolve the matching ClusterAdvertisement and check available capacity.
	cadv, errResp, status := s.findAdvertisement(ctx, req.ProviderClusterID, req.ChunkType)
	if errResp != nil {
		errResp.RequestID = requestID
		writeError(w, status, *errResp)
		return
	}
	if cadv.Status.AvailableChunks < req.ChunkCount {
		writeError(w, http.StatusConflict, ErrorResponse{
			Code: ErrCodeInsufficientCapacity,
			Message: fmt.Sprintf("provider %q has %d chunks available; %d requested",
				req.ProviderClusterID, cadv.Status.AvailableChunks, req.ChunkCount),
			Details: map[string]any{
				"available": cadv.Status.AvailableChunks,
				"requested": req.ChunkCount,
			},
			RequestID: requestID,
		})
		return
	}

	// Idempotency key from header. Clients SHOULD send it; if they don't,
	// the Broker mints one and the call becomes effectively non-idempotent.
	resName, err := reservationName(r.Header.Get(HeaderReservationID))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrorResponse{
			Code: ErrCodeInvalidRequest, Message: err.Error(), RequestID: requestID,
		})
		return
	}

	// Idempotent hit: if a Reservation with this name exists, return its
	// current state. Treat a Spec mismatch as 409 Conflict — same key MUST
	// imply the same payload, otherwise it is a client bug.
	existing := &brokerv1alpha1.Reservation{}
	switch err := s.client.Get(ctx, types.NamespacedName{Name: resName, Namespace: s.namespace}, existing); {
	case err == nil:
		if !sameReservationRequest(&existing.Spec, &req, consumerID, consumerEntry.LiqoClusterID, cadv.Spec.LiqoClusterID) {
			writeError(w, http.StatusConflict, ErrorResponse{
				Code: ErrCodeConflict,
				Message: fmt.Sprintf("reservation %q already exists with a different spec",
					resName),
				RequestID: requestID,
			})
			return
		}
		writeJSON(w, http.StatusOK, reservationResponseFromCR(existing))
		return
	case !apierrors.IsNotFound(err):
		s.log.Error(err, "get Reservation failed", "name", resName, "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code: ErrCodeInternalError, Message: "reservation read failed",
			RequestID: requestID,
		})
		return
	}

	// Re-run the sizer over the provider's advertised capacity to recover
	// per-chunk resources. The CRD's spec.resources field is +required,
	// so we cannot leave it nil — it is also exactly the value Cluster
	// Autoscaler needs to build node templates.
	perChunk := s.perChunkResources(cadv)

	resv := buildReservation(resName, s.namespace, &req, consumerID,
		consumerEntry.LiqoClusterID, cadv.Spec.LiqoClusterID, perChunk)
	if err := s.client.Create(ctx, resv); err != nil {
		s.log.Error(err, "create Reservation failed", "name", resName, "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code: ErrCodeInternalError, Message: "reservation persistence failed",
			RequestID: requestID,
		})
		return
	}

	// Stamp initial status so the client sees Pending, not the empty zero
	// value. Best-effort: a failed Status().Update is just a delayed log.
	if err := s.initReservationStatus(ctx, resv); err != nil {
		s.log.Info("initial Reservation status update failed",
			"err", err.Error(), "name", resName, "requestId", requestID)
	}

	// Bump the advertisement's reserved count. Conflict-retry up to 3 times
	// because the ClusterAdvertisement reconciler may also update status.
	if err := s.adjustReservedChunks(ctx, cadv.Name, +req.ChunkCount); err != nil {
		s.log.Error(err, "increment reservedChunks failed",
			"clusterId", cadv.Name, "delta", req.ChunkCount, "requestId", requestID)
	}

	writeJSON(w, http.StatusCreated, reservationResponseFromCR(resv))
}

// validateReservationRequest enforces the wire-level invariants the CRD
// validators would also catch. Surfaced as 400s so misuse is visible
// without an etcd round trip.
func validateReservationRequest(req *ReservationRequest) error {
	switch {
	case req.ProviderClusterID == "":
		return errors.New("providerClusterId is required")
	case req.ChunkCount < 1:
		return errors.New("chunkCount must be >= 1")
	case req.ChunkType == "":
		return errors.New("chunkType is required")
	}
	if req.ChunkType != brokerv1alpha1.ChunkTypeStandard && req.ChunkType != brokerv1alpha1.ChunkTypeGPU {
		return fmt.Errorf("unknown chunkType %q", req.ChunkType)
	}
	if req.NodeGroupID != "" {
		want := nodeGroupID(req.ProviderClusterID, req.ChunkType)
		if req.NodeGroupID != want {
			return fmt.Errorf("nodeGroupId %q does not match (provider, chunkType) — want %q",
				req.NodeGroupID, want)
		}
	}
	return nil
}

// reservationName returns either the X-Reservation-Id header value (when
// it is a valid DNS-1123 subdomain) or `res-<uuid>` if the header is empty.
func reservationName(header string) (string, error) {
	if header == "" {
		return "res-" + uuid.NewString(), nil
	}
	if errs := validation.IsDNS1123Subdomain(header); len(errs) > 0 {
		return "", fmt.Errorf("X-Reservation-Id %q is not a valid DNS-1123 subdomain: %s",
			header, strings.Join(errs, "; "))
	}
	return header, nil
}

// findAdvertisement picks the ClusterAdvertisement matching a reservation
// request. Returns (nil, *ErrorResponse, status) on a recoverable failure.
func (s *Server) findAdvertisement(
	ctx context.Context, providerClusterID string, chunkType brokerv1alpha1.ChunkType,
) (*brokerv1alpha1.ClusterAdvertisement, *ErrorResponse, int) {
	cadv := &brokerv1alpha1.ClusterAdvertisement{}
	err := s.client.Get(ctx, types.NamespacedName{Name: providerClusterID, Namespace: s.namespace}, cadv)
	if apierrors.IsNotFound(err) {
		return nil, &ErrorResponse{
			Code:    ErrCodeNotFound,
			Message: fmt.Sprintf("no advertisement for provider %q", providerClusterID),
		}, http.StatusNotFound
	}
	if err != nil {
		s.log.Error(err, "get ClusterAdvertisement failed", "clusterId", providerClusterID)
		return nil, &ErrorResponse{
			Code:    ErrCodeInternalError,
			Message: "advertisement read failed",
		}, http.StatusInternalServerError
	}
	if cadv.Spec.ClusterType != chunkType {
		return nil, &ErrorResponse{
			Code: ErrCodeNotFound,
			Message: fmt.Sprintf("provider %q advertises chunkType %q, requested %q",
				providerClusterID, cadv.Spec.ClusterType, chunkType),
		}, http.StatusNotFound
	}
	if !cadv.Status.Available {
		return nil, &ErrorResponse{
			Code:    ErrCodeServiceUnavailable,
			Message: fmt.Sprintf("provider %q is not currently Available", providerClusterID),
		}, http.StatusServiceUnavailable
	}
	return cadv, nil, http.StatusOK
}

// buildReservation hydrates the CR object from the request. Spec is
// considered immutable from this point forward; the Reservation reconciler
// drives status transitions.
func buildReservation(
	name, namespace string, req *ReservationRequest,
	consumerID, consumerLiqoID, providerLiqoID string,
	perChunk corev1.ResourceList,
) *brokerv1alpha1.Reservation {
	return &brokerv1alpha1.Reservation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: brokerv1alpha1.ReservationSpec{
			ConsumerClusterID:     consumerID,
			ConsumerLiqoClusterID: consumerLiqoID,
			ProviderClusterID:     req.ProviderClusterID,
			ProviderLiqoClusterID: providerLiqoID,
			ChunkCount:            req.ChunkCount,
			ChunkType:             req.ChunkType,
			Resources:             perChunk,
			Namespaces:            req.Namespaces,
		},
	}
}

// perChunkResources returns the per-chunk capacity matching cadv's
// advertised resources, by re-running the configured sizer. Used by the
// reservation handler to populate the +required Reservation.spec.resources
// field. Returns an empty list if the sizer produced nothing — callers
// should validate against zero before calling Create on the CR.
func (s *Server) perChunkResources(cadv *brokerv1alpha1.ClusterAdvertisement) corev1.ResourceList {
	results := s.sizer.Size(cadv.Spec.Resources.Allocatable)
	for _, r := range results {
		if r.Type == cadv.Spec.ClusterType {
			return r.PerChunk
		}
	}
	if len(results) > 0 {
		return results[0].PerChunk
	}
	return corev1.ResourceList{}
}

// initReservationStatus stamps Phase=Pending plus CreatedAt/ExpiresAt so
// the response body has a deterministic shape from request #1 onwards.
func (s *Server) initReservationStatus(ctx context.Context, resv *brokerv1alpha1.Reservation) error {
	now := metav1.Now()
	expires := metav1.NewTime(now.Add(defaultReservationTimeout))
	resv.Status = brokerv1alpha1.ReservationStatus{
		ObservedGeneration: resv.Generation,
		Phase:              brokerv1alpha1.ReservationPhasePending,
		CreatedAt:          &now,
		ExpiresAt:          &expires,
	}
	return s.client.Status().Update(ctx, resv)
}

// adjustReservedChunks updates the ClusterAdvertisement's status fields by
// `delta` (positive on reservation, negative on release). Conflict-retry
// up to 3 times to ride out concurrent reconciler updates.
func (s *Server) adjustReservedChunks(ctx context.Context, clusterID string, delta int32) error {
	for attempt := 0; attempt < 3; attempt++ {
		cadv := &brokerv1alpha1.ClusterAdvertisement{}
		if err := s.client.Get(ctx, types.NamespacedName{Name: clusterID, Namespace: s.namespace}, cadv); err != nil {
			return err
		}
		next := cadv.Status.ReservedChunks + delta
		if next < 0 {
			next = 0
		}
		cadv.Status.ReservedChunks = next
		avail := cadv.Status.TotalChunks - next
		if avail < 0 {
			avail = 0
		}
		cadv.Status.AvailableChunks = avail
		if err := s.client.Status().Update(ctx, cadv); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return err
		}
		return nil
	}
	return errors.New("exhausted conflict retries updating ClusterAdvertisement status")
}

// sameReservationRequest reports whether an existing Reservation matches a
// fresh request — used by the X-Reservation-Id idempotency path.
func sameReservationRequest(
	spec *brokerv1alpha1.ReservationSpec, req *ReservationRequest,
	consumerID, consumerLiqoID, providerLiqoID string,
) bool {
	if spec.ConsumerClusterID != consumerID ||
		spec.ConsumerLiqoClusterID != consumerLiqoID ||
		spec.ProviderClusterID != req.ProviderClusterID ||
		spec.ProviderLiqoClusterID != providerLiqoID ||
		spec.ChunkCount != req.ChunkCount ||
		spec.ChunkType != req.ChunkType {
		return false
	}
	if len(spec.Namespaces) != len(req.Namespaces) {
		return false
	}
	for i := range spec.Namespaces {
		if spec.Namespaces[i] != req.Namespaces[i] {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// 7.3.8 — GET /api/v1/reservations/{id}   (both agents)
// -----------------------------------------------------------------------------

func (s *Server) handleReservationGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := r.Header.Get(HeaderRequestID)
	resName := r.PathValue("id")
	callerID := ClusterIDFromContext(ctx)

	if resName == "" {
		writeError(w, http.StatusBadRequest, ErrorResponse{
			Code: ErrCodeInvalidRequest, Message: "id is required", RequestID: requestID,
		})
		return
	}

	resv := &brokerv1alpha1.Reservation{}
	if err := s.client.Get(ctx, types.NamespacedName{Name: resName, Namespace: s.namespace}, resv); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, ErrorResponse{
				Code:      ErrCodeNotFound,
				Message:   fmt.Sprintf("reservation %q not found", resName),
				RequestID: requestID,
			})
			return
		}
		s.log.Error(err, "get Reservation failed", "name", resName, "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code: ErrCodeInternalError, Message: "reservation read failed",
			RequestID: requestID,
		})
		return
	}

	if !isReservationParticipant(callerID, resv) {
		writeError(w, http.StatusForbidden, ErrorResponse{
			Code:      ErrCodeForbidden,
			Message:   "caller is neither the consumer nor the provider of this reservation",
			RequestID: requestID,
		})
		return
	}

	writeJSON(w, http.StatusOK, reservationResponseFromCR(resv))
}

func isReservationParticipant(clusterID string, resv *brokerv1alpha1.Reservation) bool {
	return clusterID == resv.Spec.ConsumerClusterID || clusterID == resv.Spec.ProviderClusterID
}

// -----------------------------------------------------------------------------
// 7.3.9 — DELETE /api/v1/reservations/{id}   (consumer — full release in 4g)
// -----------------------------------------------------------------------------

func (s *Server) handleReservationRelease(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := r.Header.Get(HeaderRequestID)
	resName := r.PathValue("id")
	callerID := ClusterIDFromContext(ctx)

	resv := &brokerv1alpha1.Reservation{}
	if err := s.client.Get(ctx, types.NamespacedName{Name: resName, Namespace: s.namespace}, resv); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, ErrorResponse{
				Code:      ErrCodeNotFound,
				Message:   fmt.Sprintf("reservation %q not found", resName),
				RequestID: requestID,
			})
			return
		}
		s.log.Error(err, "get Reservation failed", "name", resName, "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code: ErrCodeInternalError, Message: "reservation read failed",
			RequestID: requestID,
		})
		return
	}

	if callerID != resv.Spec.ConsumerClusterID {
		writeError(w, http.StatusForbidden, ErrorResponse{
			Code:      ErrCodeForbidden,
			Message:   "only the consumer of a reservation may release it",
			RequestID: requestID,
		})
		return
	}

	chunks, err := parseChunksQuery(r.URL.Query().Get("chunks"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrorResponse{
			Code: ErrCodeInvalidRequest, Message: err.Error(), RequestID: requestID,
		})
		return
	}
	// Step 4g supports full release only. Partial release lands once the
	// Reservation reconciler tracks remainingChunks (see docs/design.md §6).
	if chunks != 0 && chunks != resv.Spec.ChunkCount {
		writeError(w, http.StatusNotImplemented, ErrorResponse{
			Code: ErrCodeInternalError,
			Message: fmt.Sprintf("partial release is not implemented yet; pass chunks=%d or omit the query parameter",
				resv.Spec.ChunkCount),
			RequestID: requestID,
		})
		return
	}

	// Skip terminal phases — releasing an already-released reservation is a
	// no-op, not an error.
	switch resv.Status.Phase {
	case brokerv1alpha1.ReservationPhaseUnpeering,
		brokerv1alpha1.ReservationPhaseReleased,
		brokerv1alpha1.ReservationPhaseExpired,
		brokerv1alpha1.ReservationPhaseFailed:
		writeJSON(w, http.StatusAccepted, ReleaseResponse{
			ReservationID:       resv.Name,
			Status:              resv.Status.Phase,
			RemainingChunkCount: 0,
		})
		return
	}

	patched := resv.DeepCopy()
	patched.Status.Phase = brokerv1alpha1.ReservationPhaseUnpeering
	patched.Status.Message = "release requested by consumer"
	if err := s.client.Status().Update(ctx, patched); err != nil {
		s.log.Error(err, "update Reservation status to Unpeering failed",
			"name", resName, "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code: ErrCodeInternalError, Message: "reservation release failed",
			RequestID: requestID,
		})
		return
	}

	if err := s.adjustReservedChunks(ctx, resv.Spec.ProviderClusterID, -resv.Spec.ChunkCount); err != nil {
		s.log.Error(err, "decrement reservedChunks failed",
			"clusterId", resv.Spec.ProviderClusterID,
			"delta", -resv.Spec.ChunkCount, "requestId", requestID)
	}

	writeJSON(w, http.StatusAccepted, ReleaseResponse{
		ReservationID:       resv.Name,
		Status:              brokerv1alpha1.ReservationPhaseUnpeering,
		RemainingChunkCount: 0,
	})
}

// parseChunksQuery returns the integer value of the ?chunks=N query
// parameter, or 0 when absent. Negative or non-integer values are 400s.
func parseChunksQuery(raw string) (int32, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("chunks: %w", err)
	}
	if n < 0 {
		return 0, errors.New("chunks must be >= 0")
	}
	return int32(n), nil
}

// -----------------------------------------------------------------------------
// CR ↔ wire conversion
// -----------------------------------------------------------------------------

func reservationResponseFromCR(r *brokerv1alpha1.Reservation) ReservationResponse {
	out := ReservationResponse{
		ReservationID:         r.Name,
		Status:                r.Status.Phase,
		ProviderClusterID:     r.Spec.ProviderClusterID,
		ProviderLiqoClusterID: r.Spec.ProviderLiqoClusterID,
		ConsumerClusterID:     r.Spec.ConsumerClusterID,
		ConsumerLiqoClusterID: r.Spec.ConsumerLiqoClusterID,
		ChunkCount:            r.Spec.ChunkCount,
		ChunkType:             r.Spec.ChunkType,
		Resources:             r.Spec.Resources,
		VirtualNodeNames:      r.Status.VirtualNodeNames,
		Message:               r.Status.Message,
	}
	if r.Status.CreatedAt != nil {
		out.CreatedAt = *r.Status.CreatedAt
	} else {
		out.CreatedAt = r.CreationTimestamp
	}
	if r.Status.ExpiresAt != nil {
		out.ExpiresAt = r.Status.ExpiresAt
	}
	return out
}
