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

package broker

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// ReservationReconciler drives a Reservation through the asynchronous
// Broker-side phase machine described in docs/design.md §5.3:
//
//	Pending           → emit ProviderInstruction{GenerateKubeconfig}
//	                  → status.phase = GeneratingKubeconfig
//	KubeconfigReady   → emit ReservationInstruction{Peer}
//	                  → status.phase = Peering
//	Unpeering         → emit ReservationInstruction{Unpeer, LastChunk=true}
//
// All other phases (GeneratingKubeconfig / Peering / Peered / Released /
// Expired / Failed) are advanced by the Broker's instruction-result HTTP
// handler (internal/broker/api/instructions.go) when the agents report
// outcomes; the reconciler is purely a workspawner for the three "we
// need to push something to an agent" transitions above.
//
// Phase Expired is enforced here too: when a non-terminal Reservation
// has a populated ExpiresAt that has elapsed, status.phase is moved to
// Expired and Message records the deadline. No instruction is emitted.
type ReservationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=broker.federation-autoscaler.io,resources=reservations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=broker.federation-autoscaler.io,resources=reservations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=broker.federation-autoscaler.io,resources=reservations/finalizers,verbs=update
// +kubebuilder:rbac:groups=autoscaling.federation-autoscaler.io,resources=providerinstructions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.federation-autoscaler.io,resources=reservationinstructions,verbs=get;list;watch;create;update;patch;delete

// Reconcile dispatches on the current Reservation phase and emits the
// matching instruction CR. The function is idempotent: instruction
// objects use deterministic names per (reservation, kind) so re-running a
// transition merely no-ops.
func (r *ReservationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("reservation", req.String())

	var resv brokerv1alpha1.Reservation
	if err := r.Get(ctx, req.NamespacedName, &resv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Expiry guard: if a non-terminal reservation has blown past its
	// ExpiresAt, take it straight to Expired. No instruction emission.
	if expired, requeue := r.checkExpired(ctx, &resv); expired {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}

	// Provider-availability guard: if the provider's ClusterAdvertisement
	// went missing or unavailable while a non-terminal Reservation still
	// depends on it, fail the reservation (and queue a Cleanup for the
	// consumer when peering had already started).
	if abandoned, err := r.checkProviderAvailable(ctx, log, &resv); abandoned || err != nil {
		return ctrl.Result{}, err
	}

	switch resv.Status.Phase {
	case "", brokerv1alpha1.ReservationPhasePending:
		return r.handlePending(ctx, log, &resv)
	case brokerv1alpha1.ReservationPhaseKubeconfigReady:
		return r.handleKubeconfigReady(ctx, log, &resv)
	case brokerv1alpha1.ReservationPhaseUnpeering:
		return r.handleUnpeering(ctx, log, &resv)
	default:
		// GeneratingKubeconfig / Peering / Peered / Released / Expired /
		// Failed are all moved by the instruction-result handler.
		return ctrl.Result{}, nil
	}
}

// -----------------------------------------------------------------------------
// Pending → GeneratingKubeconfig
// -----------------------------------------------------------------------------

func (r *ReservationReconciler) handlePending(
	ctx context.Context, log logr.Logger, resv *brokerv1alpha1.Reservation,
) (ctrl.Result, error) {
	pi := &autoscalingv1alpha1.ProviderInstruction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      providerInstructionGKName(resv.Name),
			Namespace: resv.Namespace,
		},
		Spec: autoscalingv1alpha1.ProviderInstructionSpec{
			ReservationID:         resv.Name,
			Kind:                  autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig,
			TargetClusterID:       resv.Spec.ProviderClusterID,
			ConsumerClusterID:     resv.Spec.ConsumerClusterID,
			ConsumerLiqoClusterID: resv.Spec.ConsumerLiqoClusterID,
			ChunkCount:            resv.Spec.ChunkCount,
			ExpiresAt:             resv.Status.ExpiresAt,
		},
	}

	if err := r.ensureInstruction(ctx, resv, pi); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure GenerateKubeconfig instruction: %w", err)
	}

	if err := r.advancePhase(ctx, resv, brokerv1alpha1.ReservationPhaseGeneratingKubeconfig,
		"queued GenerateKubeconfig instruction for provider"); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("queued GenerateKubeconfig", "provider", resv.Spec.ProviderClusterID)
	return ctrl.Result{}, nil
}

// -----------------------------------------------------------------------------
// KubeconfigReady → Peering
// -----------------------------------------------------------------------------

func (r *ReservationReconciler) handleKubeconfigReady(
	ctx context.Context, log logr.Logger, resv *brokerv1alpha1.Reservation,
) (ctrl.Result, error) {
	ri := &autoscalingv1alpha1.ReservationInstruction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      reservationInstructionPeerName(resv.Name),
			Namespace: resv.Namespace,
		},
		Spec: autoscalingv1alpha1.ReservationInstructionSpec{
			ReservationID:         resv.Name,
			Kind:                  autoscalingv1alpha1.ReservationInstructionPeer,
			TargetClusterID:       resv.Spec.ConsumerClusterID,
			ProviderClusterID:     resv.Spec.ProviderClusterID,
			ProviderLiqoClusterID: resv.Spec.ProviderLiqoClusterID,
			ChunkCount:            resv.Spec.ChunkCount,
			KubeconfigRef:         kubeconfigSecretName(resv.Name),
			ExpiresAt:             resv.Status.ExpiresAt,
		},
	}

	if err := r.ensureInstruction(ctx, resv, ri); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure Peer instruction: %w", err)
	}

	if err := r.advancePhase(ctx, resv, brokerv1alpha1.ReservationPhasePeering,
		"queued Peer instruction for consumer"); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("queued Peer", "consumer", resv.Spec.ConsumerClusterID)
	return ctrl.Result{}, nil
}

// -----------------------------------------------------------------------------
// Unpeering → (instruction emitted; phase advances to Released on result)
// -----------------------------------------------------------------------------

func (r *ReservationReconciler) handleUnpeering(
	ctx context.Context, log logr.Logger, resv *brokerv1alpha1.Reservation,
) (ctrl.Result, error) {
	ri := &autoscalingv1alpha1.ReservationInstruction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      reservationInstructionUnpeerName(resv.Name),
			Namespace: resv.Namespace,
		},
		Spec: autoscalingv1alpha1.ReservationInstructionSpec{
			ReservationID:         resv.Name,
			Kind:                  autoscalingv1alpha1.ReservationInstructionUnpeer,
			TargetClusterID:       resv.Spec.ConsumerClusterID,
			ProviderClusterID:     resv.Spec.ProviderClusterID,
			ProviderLiqoClusterID: resv.Spec.ProviderLiqoClusterID,
			ChunkCount:            resv.Spec.ChunkCount,
			// v1 limitation: a Reservation always releases every chunk at
			// once. Per-chunk decrement is a v2 feature; the field stays in
			// the spec to keep the wire / CRD shape forward-compatible.
			LastChunk: true,
		},
	}

	if err := r.ensureInstruction(ctx, resv, ri); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure Unpeer instruction: %w", err)
	}
	log.Info("queued Unpeer", "consumer", resv.Spec.ConsumerClusterID)
	return ctrl.Result{}, nil
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// ensureInstruction creates the given instruction CR if it doesn't exist
// yet. Existing instances are left untouched — they may already have been
// delivered or marked Enforced. The Reservation is set as the controller
// owner so deleting the Reservation cascades the instruction.
func (r *ReservationReconciler) ensureInstruction(
	ctx context.Context, resv *brokerv1alpha1.Reservation, instruction client.Object,
) error {
	if err := controllerutil.SetControllerReference(resv, instruction, r.Scheme); err != nil {
		return fmt.Errorf("set controller reference: %w", err)
	}
	if err := r.Create(ctx, instruction); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	return nil
}

// advancePhase patches status.phase / status.message and bumps
// observedGeneration. No-op when the phase is already what we want
// (avoids etcd churn from concurrent reconciles).
func (r *ReservationReconciler) advancePhase(
	ctx context.Context, resv *brokerv1alpha1.Reservation,
	next brokerv1alpha1.ReservationPhase, message string,
) error {
	if resv.Status.Phase == next {
		return nil
	}
	patched := resv.DeepCopy()
	patched.Status.Phase = next
	patched.Status.Message = message
	patched.Status.ObservedGeneration = resv.Generation
	return r.Status().Update(ctx, patched)
}

// checkProviderAvailable fails a Reservation whose provider's
// ClusterAdvertisement is missing or has flipped Available=false. The
// guard is what closes the gap between the API handler — which validates
// availability *at create time* — and a long-running reservation whose
// provider may go away mid-flight.
//
// Phase Unpeering is excluded: it is already winding down via the Unpeer
// instruction emitted by handleUnpeering, and we let the
// instruction-result handler advance it to Released or Failed naturally.
//
// When the consumer had already begun peering (Peering / Peered), a
// ReservationInstruction{Cleanup} is queued so the consumer agent can
// drop the local Liqo state (ResourceSlice, NamespaceOffloading, virtual
// node). Provider-side cleanup is intentionally skipped: the provider is
// unreachable by definition, and a returning provider replays its own
// state via ProviderInstructionReconcile.
func (r *ReservationReconciler) checkProviderAvailable(
	ctx context.Context, log logr.Logger, resv *brokerv1alpha1.Reservation,
) (bool, error) {
	if isReservationTerminal(resv.Status.Phase) {
		return false, nil
	}
	if resv.Status.Phase == brokerv1alpha1.ReservationPhaseUnpeering {
		return false, nil
	}

	var cadv brokerv1alpha1.ClusterAdvertisement
	err := r.Get(ctx, types.NamespacedName{
		Name: resv.Spec.ProviderClusterID, Namespace: resv.Namespace,
	}, &cadv)
	switch {
	case err == nil && cadv.Status.Available:
		return false, nil
	case err != nil && !apierrors.IsNotFound(err):
		return false, fmt.Errorf("get ClusterAdvertisement %q: %w", resv.Spec.ProviderClusterID, err)
	}

	msg := fmt.Sprintf("provider %q advertisement no longer available", resv.Spec.ProviderClusterID)
	if apierrors.IsNotFound(err) {
		msg = fmt.Sprintf("provider %q advertisement no longer exists", resv.Spec.ProviderClusterID)
	}

	if needsConsumerCleanup(resv.Status.Phase) {
		if cerr := r.ensureCleanupInstruction(ctx, resv); cerr != nil {
			return false, fmt.Errorf("ensure Cleanup instruction: %w", cerr)
		}
		log.Info("queued Cleanup after provider went unavailable",
			"consumer", resv.Spec.ConsumerClusterID, "provider", resv.Spec.ProviderClusterID)
	}

	if err := r.advancePhase(ctx, resv, brokerv1alpha1.ReservationPhaseFailed, msg); err != nil {
		return false, err
	}
	log.Info("reservation failed: provider unavailable",
		"provider", resv.Spec.ProviderClusterID, "phase", resv.Status.Phase)
	return true, nil
}

// needsConsumerCleanup returns true for phases where the consumer has
// applied (or is in the process of applying) Liqo state that would leak
// without an explicit teardown.
func needsConsumerCleanup(p brokerv1alpha1.ReservationPhase) bool {
	switch p {
	case brokerv1alpha1.ReservationPhasePeering,
		brokerv1alpha1.ReservationPhasePeered:
		return true
	}
	return false
}

// ensureCleanupInstruction emits a ReservationInstruction{Cleanup}
// targeted at the consumer. Same idempotency guarantees as
// ensureInstruction: re-running this on a Reservation that already has a
// cleanup instruction is a no-op.
func (r *ReservationReconciler) ensureCleanupInstruction(
	ctx context.Context, resv *brokerv1alpha1.Reservation,
) error {
	ri := &autoscalingv1alpha1.ReservationInstruction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      reservationInstructionCleanupName(resv.Name),
			Namespace: resv.Namespace,
		},
		Spec: autoscalingv1alpha1.ReservationInstructionSpec{
			ReservationID:         resv.Name,
			Kind:                  autoscalingv1alpha1.ReservationInstructionCleanup,
			TargetClusterID:       resv.Spec.ConsumerClusterID,
			ProviderClusterID:     resv.Spec.ProviderClusterID,
			ProviderLiqoClusterID: resv.Spec.ProviderLiqoClusterID,
			ChunkCount:            resv.Spec.ChunkCount,
			LastChunk:             true,
		},
	}
	return r.ensureInstruction(ctx, resv, ri)
}

// checkExpired flips a non-terminal Reservation to Expired when its
// ExpiresAt has elapsed. Returns (true, 0) on a successful flip; returns
// (false, requeue) — with requeue == time-until-expiry — when the
// reservation is still alive but we want to wake up at the deadline.
func (r *ReservationReconciler) checkExpired(
	ctx context.Context, resv *brokerv1alpha1.Reservation,
) (bool, time.Duration) {
	if resv.Status.ExpiresAt == nil {
		return false, 0
	}
	if isReservationTerminal(resv.Status.Phase) {
		return false, 0
	}
	now := time.Now()
	if resv.Status.ExpiresAt.After(now) {
		return false, time.Until(resv.Status.ExpiresAt.Time)
	}
	if err := r.advancePhase(ctx, resv, brokerv1alpha1.ReservationPhaseExpired,
		fmt.Sprintf("reservation timeout elapsed at %s", resv.Status.ExpiresAt.UTC().Format(time.RFC3339))); err != nil {
		// Best-effort: a missed flip is recovered on the next reconcile.
		logf.FromContext(ctx).Info("expire status update failed", "err", err.Error())
	}
	return true, 0
}

func isReservationTerminal(p brokerv1alpha1.ReservationPhase) bool {
	switch p {
	case brokerv1alpha1.ReservationPhaseReleased,
		brokerv1alpha1.ReservationPhaseExpired,
		brokerv1alpha1.ReservationPhaseFailed:
		return true
	}
	return false
}

// -----------------------------------------------------------------------------
// Naming helpers
// -----------------------------------------------------------------------------
//
// Deterministic names guarantee that the reconciler converges even when
// it races with itself: re-creating an instruction with the same name is
// a NoOp.

func providerInstructionGKName(reservationName string) string {
	return "gk-" + reservationName
}

func reservationInstructionPeerName(reservationName string) string {
	return "peer-" + reservationName
}

func reservationInstructionUnpeerName(reservationName string) string {
	return "unpeer-" + reservationName
}

func reservationInstructionCleanupName(reservationName string) string {
	return "cleanup-" + reservationName
}

// kubeconfigSecretName must match the value the API instruction handler
// (internal/broker/api/instructions.go) uses when persisting the
// peering-user kubeconfig — both have to agree on the Secret name to
// hand it off cleanly. The duplication is intentional to keep this
// package free of internal/broker/api imports (a controller MUST NOT
// depend on the HTTP layer).
func kubeconfigSecretName(reservationName string) string {
	return "kubeconfig-" + reservationName
}

// SetupWithManager sets up the controller with the Manager.
//
// The Watches on ClusterAdvertisement is what wakes Reservations up when
// their provider's freshness flips — without it, a stale provider would
// leave dependent Reservations dangling until their ExpiresAt fires.
func (r *ReservationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&brokerv1alpha1.Reservation{}).
		Owns(&autoscalingv1alpha1.ProviderInstruction{}).
		Owns(&autoscalingv1alpha1.ReservationInstruction{}).
		Watches(
			&brokerv1alpha1.ClusterAdvertisement{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForAdvertisement),
		).
		Named("broker-reservation").
		Complete(r)
}

// requestsForAdvertisement enqueues every non-terminal Reservation in the
// advertisement's namespace whose Spec.ProviderClusterID matches the CA
// name. Terminal-phase Reservations are skipped — they are already done
// and re-reconciling them would only generate etcd churn.
func (r *ReservationReconciler) requestsForAdvertisement(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	cadv, ok := obj.(*brokerv1alpha1.ClusterAdvertisement)
	if !ok {
		return nil
	}
	var resvs brokerv1alpha1.ReservationList
	if err := r.List(ctx, &resvs, client.InNamespace(cadv.Namespace)); err != nil {
		logf.FromContext(ctx).Error(err, "list Reservations for advertisement watch",
			"advertisement", cadv.Name)
		return nil
	}
	out := make([]reconcile.Request, 0, len(resvs.Items))
	for i := range resvs.Items {
		resv := &resvs.Items[i]
		if resv.Spec.ProviderClusterID != cadv.Name {
			continue
		}
		if isReservationTerminal(resv.Status.Phase) {
			continue
		}
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: resv.Name, Namespace: resv.Namespace},
		})
	}
	return out
}
