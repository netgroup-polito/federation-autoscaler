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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// kubeconfigSecretKey is the data key under which the peering-user
// kubeconfig is stored in the broker-side staging Secret.
const kubeconfigSecretKey = "kubeconfig"

// kubeconfigSecretName returns the canonical staging-Secret name for a
// reservation. Kept short so it fits in the 253-char DNS subdomain limit.
func kubeconfigSecretName(reservationID string) string {
	return "kubeconfig-" + reservationID
}

// -----------------------------------------------------------------------------
// 7.3.6 — GET /api/v1/instructions   (both agents, every 5 s)
// -----------------------------------------------------------------------------

// handleInstructionsList returns every pending instruction targeting the
// caller's cluster, regardless of role. The response is polymorphic
// (InstructionView.Kind disambiguates) — a cluster that is both consumer
// and provider will see entries of both flavours.
func (s *Server) handleInstructionsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := r.Header.Get(HeaderRequestID)
	callerID := ClusterIDFromContext(ctx)

	views, err := s.collectInstructionsForCaller(ctx, callerID)
	if err != nil {
		s.log.Error(err, "list instructions failed",
			"clusterId", callerID, "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code: ErrCodeInternalError, Message: "instruction read failed",
			RequestID: requestID,
		})
		return
	}

	writeJSON(w, http.StatusOK, InstructionsResponse{Instructions: views})
}

// collectInstructionsForCaller is shared by GET /api/v1/instructions and
// the POST /api/v1/advertisements piggyback path: it lists every pending
// instruction for callerID and converts each into the wire view.
func (s *Server) collectInstructionsForCaller(
	ctx context.Context, callerID string,
) ([]InstructionView, error) {
	out := make([]InstructionView, 0)

	// Provider-side work (GenerateKubeconfig / Cleanup / Reconcile).
	var pis autoscalingv1alpha1.ProviderInstructionList
	if err := s.client.List(ctx, &pis, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list ProviderInstruction: %w", err)
	}
	for i := range pis.Items {
		pi := &pis.Items[i]
		if pi.Spec.TargetClusterID != callerID || pi.Status.Enforced {
			continue
		}
		out = append(out, viewFromProviderInstruction(pi))
		s.touchInstructionDelivered(ctx, pi)
	}

	// Consumer-side work (Peer / Unpeer / Cleanup / Reconcile).
	var ris autoscalingv1alpha1.ReservationInstructionList
	if err := s.client.List(ctx, &ris, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list ReservationInstruction: %w", err)
	}
	for i := range ris.Items {
		ri := &ris.Items[i]
		if ri.Spec.TargetClusterID != callerID || ri.Status.Enforced {
			continue
		}

		view := viewFromReservationInstruction(ri)
		// Inline the peering-user kubeconfig only on the wire; never write
		// it back into a CRD or log it. The Secret is read fresh on every
		// poll so rotation is automatic.
		if ri.Spec.Kind == autoscalingv1alpha1.ReservationInstructionPeer && ri.Spec.KubeconfigRef != "" {
			kc, err := s.loadKubeconfig(ctx, ri.Spec.KubeconfigRef)
			if err != nil {
				s.log.Info("inlining kubeconfig failed; skipping instruction this cycle",
					"err", err.Error(),
					"reservationId", ri.Spec.ReservationID,
					"secret", ri.Spec.KubeconfigRef)
				continue
			}
			view.Kubeconfig = kc
		}

		// Decorate Peer / Unpeer with the per-chunk resource shape and
		// requested namespaces from the owning Reservation. Failure to
		// fetch the Reservation is logged but doesn't drop the instruction
		// — the agent can still execute the bare op.
		if ri.Spec.Kind == autoscalingv1alpha1.ReservationInstructionPeer ||
			ri.Spec.Kind == autoscalingv1alpha1.ReservationInstructionUnpeer {
			if resv, err := s.getReservation(ctx, ri.Spec.ReservationID); err == nil {
				view.ResourceSliceResources = resv.Spec.Resources
				view.Namespaces = resv.Spec.Namespaces
			} else if !apierrors.IsNotFound(err) {
				s.log.Info("reservation lookup failed during instruction decoration",
					"err", err.Error(), "reservationId", ri.Spec.ReservationID)
			}
		}

		out = append(out, view)
		s.touchInstructionDelivered(ctx, ri)
	}

	return out, nil
}

// touchInstructionDelivered best-effort updates LastDeliveredAt so the
// Broker can observe that the instruction has been transmitted at least
// once. A failed write is non-fatal; the next poll will re-try.
func (s *Server) touchInstructionDelivered(ctx context.Context, obj client.Object) {
	now := metav1.Now()

	switch o := obj.(type) {
	case *autoscalingv1alpha1.ProviderInstruction:
		patched := o.DeepCopy()
		patched.Status.LastDeliveredAt = &now
		if err := s.client.Status().Update(ctx, patched); err != nil {
			s.log.V(1).Info("ProviderInstruction status update skipped",
				"name", o.Name, "err", err.Error())
		}
	case *autoscalingv1alpha1.ReservationInstruction:
		patched := o.DeepCopy()
		patched.Status.LastDeliveredAt = &now
		if err := s.client.Status().Update(ctx, patched); err != nil {
			s.log.V(1).Info("ReservationInstruction status update skipped",
				"name", o.Name, "err", err.Error())
		}
	}
}

func viewFromProviderInstruction(pi *autoscalingv1alpha1.ProviderInstruction) InstructionView {
	v := InstructionView{
		ID:                    pi.Name,
		Kind:                  string(pi.Spec.Kind),
		ReservationID:         pi.Spec.ReservationID,
		ChunkCount:            pi.Spec.ChunkCount,
		LastChunk:             pi.Spec.LastChunk,
		ConsumerClusterID:     pi.Spec.ConsumerClusterID,
		ConsumerLiqoClusterID: pi.Spec.ConsumerLiqoClusterID,
	}
	if pi.Status.IssuedAt != nil {
		v.IssuedAt = *pi.Status.IssuedAt
	} else {
		v.IssuedAt = pi.CreationTimestamp
	}
	if pi.Spec.ExpiresAt != nil {
		v.ExpiresAt = *pi.Spec.ExpiresAt
	}
	return v
}

func viewFromReservationInstruction(ri *autoscalingv1alpha1.ReservationInstruction) InstructionView {
	v := InstructionView{
		ID:                    ri.Name,
		Kind:                  string(ri.Spec.Kind),
		ReservationID:         ri.Spec.ReservationID,
		ChunkCount:            ri.Spec.ChunkCount,
		LastChunk:             ri.Spec.LastChunk,
		ProviderClusterID:     ri.Spec.ProviderClusterID,
		ProviderLiqoClusterID: ri.Spec.ProviderLiqoClusterID,
		ResourceSliceNames:    ri.Spec.ResourceSliceNames,
	}
	if ri.Status.IssuedAt != nil {
		v.IssuedAt = *ri.Status.IssuedAt
	} else {
		v.IssuedAt = ri.CreationTimestamp
	}
	if ri.Spec.ExpiresAt != nil {
		v.ExpiresAt = *ri.Spec.ExpiresAt
	}
	return v
}

// loadKubeconfig reads the staged peering-user kubeconfig from a Secret in
// the Broker namespace. Bytes are returned verbatim (PEM); the wire layer
// puts them in InstructionView.Kubeconfig.
func (s *Server) loadKubeconfig(ctx context.Context, secretName string) (string, error) {
	var sec corev1.Secret
	if err := s.client.Get(ctx, types.NamespacedName{Name: secretName, Namespace: s.namespace}, &sec); err != nil {
		return "", fmt.Errorf("get secret %s/%s: %w", s.namespace, secretName, err)
	}
	b, ok := sec.Data[kubeconfigSecretKey]
	if !ok || len(b) == 0 {
		return "", fmt.Errorf("secret %s/%s has no %q key", s.namespace, secretName, kubeconfigSecretKey)
	}
	return string(b), nil
}

func (s *Server) getReservation(ctx context.Context, name string) (*brokerv1alpha1.Reservation, error) {
	var resv brokerv1alpha1.Reservation
	if err := s.client.Get(ctx, types.NamespacedName{Name: name, Namespace: s.namespace}, &resv); err != nil {
		return nil, err
	}
	return &resv, nil
}

// -----------------------------------------------------------------------------
// 7.3.7 — POST /api/v1/instructions/{id}/result   (both agents)
// -----------------------------------------------------------------------------

func (s *Server) handleInstructionResult(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := r.Header.Get(HeaderRequestID)
	callerID := ClusterIDFromContext(ctx)
	id := r.PathValue("id")

	if id == "" {
		writeError(w, http.StatusBadRequest, ErrorResponse{
			Code: ErrCodeInvalidRequest, Message: "id is required", RequestID: requestID,
		})
		return
	}

	var req InstructionResultRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrorResponse{
			Code: ErrCodeInvalidRequest, Message: err.Error(), RequestID: requestID,
		})
		return
	}
	if err := validateInstructionResult(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrorResponse{
			Code: ErrCodeInvalidRequest, Message: err.Error(), RequestID: requestID,
		})
		return
	}

	// Try ProviderInstruction first; fall back to ReservationInstruction.
	pi := &autoscalingv1alpha1.ProviderInstruction{}
	switch err := s.client.Get(ctx, types.NamespacedName{Name: id, Namespace: s.namespace}, pi); {
	case err == nil:
		s.applyProviderResult(ctx, w, pi, &req, callerID, requestID)
		return
	case !apierrors.IsNotFound(err):
		s.log.Error(err, "get ProviderInstruction failed", "name", id, "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code: ErrCodeInternalError, Message: "instruction read failed", RequestID: requestID,
		})
		return
	}

	ri := &autoscalingv1alpha1.ReservationInstruction{}
	switch err := s.client.Get(ctx, types.NamespacedName{Name: id, Namespace: s.namespace}, ri); {
	case err == nil:
		s.applyReservationResult(ctx, w, ri, &req, callerID, requestID)
		return
	case apierrors.IsNotFound(err):
		writeError(w, http.StatusNotFound, ErrorResponse{
			Code: ErrCodeNotFound, Message: fmt.Sprintf("instruction %q not found", id),
			RequestID: requestID,
		})
		return
	default:
		s.log.Error(err, "get ReservationInstruction failed", "name", id, "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code: ErrCodeInternalError, Message: "instruction read failed", RequestID: requestID,
		})
	}
}

func validateInstructionResult(req *InstructionResultRequest) error {
	switch req.Status {
	case ResultStatusSucceeded:
	case ResultStatusFailed:
		if req.Error == nil || req.Error.Message == "" {
			return errors.New("status=Failed requires a non-empty error.message")
		}
	default:
		return fmt.Errorf("invalid status %q (want %q | %q)",
			req.Status, ResultStatusSucceeded, ResultStatusFailed)
	}
	if req.Status == ResultStatusSucceeded && req.Payload != nil {
		switch req.Payload.Kind {
		case PayloadKindKubeconfig, PayloadKindPeer, PayloadKindUnpeer, PayloadKindReconcile, "":
		default:
			return fmt.Errorf("invalid payload.kind %q", req.Payload.Kind)
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// Provider-side result handling
// -----------------------------------------------------------------------------

func (s *Server) applyProviderResult(
	ctx context.Context, w http.ResponseWriter,
	pi *autoscalingv1alpha1.ProviderInstruction, req *InstructionResultRequest,
	callerID, requestID string,
) {
	if pi.Spec.TargetClusterID != callerID {
		writeError(w, http.StatusForbidden, ErrorResponse{
			Code:      ErrCodeForbidden,
			Message:   "instruction is not targeted at this cluster",
			RequestID: requestID,
		})
		return
	}
	if pi.Status.Enforced {
		writeJSON(w, http.StatusOK, InstructionResultResponse{Accepted: true})
		return
	}

	if req.Status == ResultStatusSucceeded &&
		pi.Spec.Kind == autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig {
		if err := s.persistKubeconfigPayload(ctx, pi, req.Payload); err != nil {
			s.log.Error(err, "persist kubeconfig failed",
				"reservationId", pi.Spec.ReservationID, "requestId", requestID)
			writeError(w, http.StatusInternalServerError, ErrorResponse{
				Code: ErrCodeInternalError, Message: "kubeconfig persistence failed",
				RequestID: requestID,
			})
			return
		}
	}

	if err := s.markProviderInstructionEnforced(ctx, pi, req); err != nil {
		s.log.Error(err, "mark ProviderInstruction enforced failed",
			"name", pi.Name, "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code: ErrCodeInternalError, Message: "instruction status update failed",
			RequestID: requestID,
		})
		return
	}

	writeJSON(w, http.StatusOK, InstructionResultResponse{Accepted: true})
}

// persistKubeconfigPayload stores the peering-user kubeconfig in a Secret
// keyed by reservation ID, then points the matching Peer instruction at it
// via Spec.KubeconfigRef. This is the bridge between Provider Agent's
// `liqoctl generate peering-user` output and the Consumer Agent's
// upcoming `liqoctl peer` invocation.
func (s *Server) persistKubeconfigPayload(
	ctx context.Context, pi *autoscalingv1alpha1.ProviderInstruction, payload *ResultPayload,
) error {
	if payload == nil || payload.Kubeconfig == "" {
		return errors.New("KubeconfigPayload requires a non-empty kubeconfig")
	}
	secretName := kubeconfigSecretName(pi.Spec.ReservationID)

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: s.namespace,
			Labels: map[string]string{
				"federation-autoscaler.io/reservation": pi.Spec.ReservationID,
			},
		},
		Type: corev1.SecretTypeOpaque,
	}
	op, err := createOrUpdateSecret(ctx, s.client, sec, func() error {
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data[kubeconfigSecretKey] = []byte(payload.Kubeconfig)
		return nil
	})
	if err != nil {
		return fmt.Errorf("upsert kubeconfig secret: %w", err)
	}
	s.log.V(1).Info("staged peering-user kubeconfig",
		"secret", secretName, "operation", op,
		"reservationId", pi.Spec.ReservationID)

	// Point any existing Peer instruction for this reservation at the
	// staged Secret. The instruction reconciler (future step) is the one
	// that creates Peer instructions; here we only update an existing one.
	if err := s.attachKubeconfigToPeer(ctx, pi.Spec.ReservationID, secretName); err != nil {
		s.log.Info("could not attach kubeconfig to Peer instruction (will be picked up later)",
			"err", err.Error(), "reservationId", pi.Spec.ReservationID)
	}

	// Advance Reservation phase if it's still on the kubeconfig step.
	if err := s.advanceReservationPhase(ctx, pi.Spec.ReservationID,
		brokerv1alpha1.ReservationPhaseGeneratingKubeconfig,
		brokerv1alpha1.ReservationPhaseKubeconfigReady,
		"kubeconfig delivered"); err != nil {
		s.log.Info("phase advance skipped", "err", err.Error())
	}
	return nil
}

// attachKubeconfigToPeer locates the (at most one) ReservationInstruction
// of kind Peer for this reservation and writes the Secret name into its
// spec. If none exists yet (the instruction reconciler hasn't created it),
// this is a no-op — the create-time path will pick the Secret up.
func (s *Server) attachKubeconfigToPeer(
	ctx context.Context, reservationID, secretName string,
) error {
	var ris autoscalingv1alpha1.ReservationInstructionList
	if err := s.client.List(ctx, &ris, client.InNamespace(s.namespace)); err != nil {
		return err
	}
	for i := range ris.Items {
		ri := &ris.Items[i]
		if ri.Spec.ReservationID != reservationID ||
			ri.Spec.Kind != autoscalingv1alpha1.ReservationInstructionPeer {
			continue
		}
		if ri.Spec.KubeconfigRef == secretName {
			return nil // already pointing here
		}
		patched := ri.DeepCopy()
		patched.Spec.KubeconfigRef = secretName
		return s.client.Update(ctx, patched)
	}
	return nil
}

// markProviderInstructionEnforced flips status.enforced=true and bumps the
// bookkeeping counters. Conflict-retry is intentionally absent — the Agent
// will re-POST the result on any 5xx and the operation is idempotent.
func (s *Server) markProviderInstructionEnforced(
	ctx context.Context, pi *autoscalingv1alpha1.ProviderInstruction, req *InstructionResultRequest,
) error {
	now := metav1.Now()
	patched := pi.DeepCopy()
	patched.Status.Enforced = true
	patched.Status.Attempts++
	patched.Status.LastUpdateTime = &now
	patched.Status.Message = resultMessage(req)
	return s.client.Status().Update(ctx, patched)
}

// -----------------------------------------------------------------------------
// Consumer-side result handling
// -----------------------------------------------------------------------------

func (s *Server) applyReservationResult(
	ctx context.Context, w http.ResponseWriter,
	ri *autoscalingv1alpha1.ReservationInstruction, req *InstructionResultRequest,
	callerID, requestID string,
) {
	if ri.Spec.TargetClusterID != callerID {
		writeError(w, http.StatusForbidden, ErrorResponse{
			Code:      ErrCodeForbidden,
			Message:   "instruction is not targeted at this cluster",
			RequestID: requestID,
		})
		return
	}
	if ri.Status.Enforced {
		writeJSON(w, http.StatusOK, InstructionResultResponse{Accepted: true})
		return
	}

	switch req.Status {
	case ResultStatusSucceeded:
		switch ri.Spec.Kind {
		case autoscalingv1alpha1.ReservationInstructionPeer:
			if err := s.applyPeerPayload(ctx, ri, req.Payload); err != nil {
				s.log.Error(err, "apply PeerPayload failed",
					"reservationId", ri.Spec.ReservationID, "requestId", requestID)
				writeError(w, http.StatusInternalServerError, ErrorResponse{
					Code: ErrCodeInternalError, Message: "peer result persistence failed",
					RequestID: requestID,
				})
				return
			}
		case autoscalingv1alpha1.ReservationInstructionUnpeer:
			s.applyUnpeerPayload(ctx, ri)
		}
	case ResultStatusFailed:
		_ = s.advanceReservationPhase(ctx, ri.Spec.ReservationID, "",
			brokerv1alpha1.ReservationPhaseFailed, req.Error.Message)
	}

	if err := s.markReservationInstructionEnforced(ctx, ri, req); err != nil {
		s.log.Error(err, "mark ReservationInstruction enforced failed",
			"name", ri.Name, "requestId", requestID)
		writeError(w, http.StatusInternalServerError, ErrorResponse{
			Code: ErrCodeInternalError, Message: "instruction status update failed",
			RequestID: requestID,
		})
		return
	}

	writeJSON(w, http.StatusOK, InstructionResultResponse{Accepted: true})
}

// applyPeerPayload copies the consumer-reported virtual-node names onto the
// owning Reservation and advances it to Peered. Idempotent: re-application
// of the same payload converges on the same state.
func (s *Server) applyPeerPayload(
	ctx context.Context, ri *autoscalingv1alpha1.ReservationInstruction, payload *ResultPayload,
) error {
	resv, err := s.getReservation(ctx, ri.Spec.ReservationID)
	if err != nil {
		return err
	}
	patched := resv.DeepCopy()
	if payload != nil {
		patched.Status.VirtualNodeNames = mergeStrings(patched.Status.VirtualNodeNames, payload.VirtualNodeNames)
	}
	patched.Status.Phase = brokerv1alpha1.ReservationPhasePeered
	patched.Status.Message = "peering completed"
	return s.client.Status().Update(ctx, patched)
}

// applyUnpeerPayload advances Released-on-last-chunk; partial release
// transitions stay on Unpeering until the next chunk's result arrives.
func (s *Server) applyUnpeerPayload(
	ctx context.Context, ri *autoscalingv1alpha1.ReservationInstruction,
) {
	if !ri.Spec.LastChunk {
		return
	}
	if err := s.advanceReservationPhase(ctx, ri.Spec.ReservationID,
		brokerv1alpha1.ReservationPhaseUnpeering,
		brokerv1alpha1.ReservationPhaseReleased,
		"all chunks released"); err != nil {
		s.log.Info("phase advance to Released skipped", "err", err.Error())
	}
}

func (s *Server) markReservationInstructionEnforced(
	ctx context.Context, ri *autoscalingv1alpha1.ReservationInstruction, req *InstructionResultRequest,
) error {
	now := metav1.Now()
	patched := ri.DeepCopy()
	patched.Status.Enforced = true
	patched.Status.Attempts++
	patched.Status.LastUpdateTime = &now
	patched.Status.Message = resultMessage(req)
	return s.client.Status().Update(ctx, patched)
}

// -----------------------------------------------------------------------------
// Reservation phase helper + small utilities
// -----------------------------------------------------------------------------

// advanceReservationPhase moves a Reservation from `from` to `to` in a
// single Status Update. If `from` is empty, advance unconditionally (used
// for terminal Failed transitions). Always sets Status.Message.
func (s *Server) advanceReservationPhase(
	ctx context.Context, reservationID string,
	from, to brokerv1alpha1.ReservationPhase, message string,
) error {
	resv, err := s.getReservation(ctx, reservationID)
	if err != nil {
		return err
	}
	if from != "" && resv.Status.Phase != from {
		return fmt.Errorf("phase is %q, want %q", resv.Status.Phase, from)
	}
	patched := resv.DeepCopy()
	patched.Status.Phase = to
	patched.Status.Message = message
	return s.client.Status().Update(ctx, patched)
}

// resultMessage extracts a short human description from req for the
// instruction's Status.Message. Bounded to keep etcd entries small.
func resultMessage(req *InstructionResultRequest) string {
	if req.Status == ResultStatusFailed && req.Error != nil {
		return fmt.Sprintf("Failed: %s", req.Error.Message)
	}
	return "Succeeded"
}

func mergeStrings(existing, fresh []string) []string {
	if len(fresh) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing)+len(fresh))
	out := make([]string, 0, len(existing)+len(fresh))
	for _, s := range existing {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range fresh {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// createOrUpdateSecret is a tiny wrapper around the controller-runtime
// CreateOrUpdate idiom, kept here to avoid importing controllerutil into
// every result-handler file.
func createOrUpdateSecret(
	ctx context.Context, c client.Client, sec *corev1.Secret, mutate func() error,
) (string, error) {
	existing := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: sec.Name, Namespace: sec.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		if err := mutate(); err != nil {
			return "", err
		}
		return "created", c.Create(ctx, sec)
	}
	if err != nil {
		return "", err
	}
	// Copy server-generated metadata onto our object so the Update succeeds.
	sec.ResourceVersion = existing.ResourceVersion
	sec.UID = existing.UID
	if err := mutate(); err != nil {
		return "", err
	}
	return "updated", c.Update(ctx, sec)
}
