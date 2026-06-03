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
	corev1 "k8s.io/api/core/v1"
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

// ReservationFinalizer guards a Reservation so its reserved chunks are
// credited back to the provider's ClusterAdvertisement before the CR is
// removed. Without it, a `kubectl delete reservation` (operator hard
// delete, bypassing the consumer-initiated DELETE /api/v1/reservations
// path that decrements + stamps ChunksReleased) would drop the object
// before handleTerminal ran releaseChunks — leaking ReservedChunks and
// permanently shrinking the provider's AvailableChunks budget.
const ReservationFinalizer = "broker.federation-autoscaler.io/release-chunks"

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
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;delete

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

	// Finalizer bookkeeping.
	if !resv.DeletionTimestamp.IsZero() {
		// Being deleted: credit the reserved chunks back (releaseChunks
		// is gated by IsChunksReleased, so the normal consumer-initiated
		// release path that already decremented + stamped is a no-op
		// here — no double-release), then drop the finalizer so the CR
		// can be garbage-collected.
		return r.handleDeletion(ctx, log, &resv)
	}
	// Live object: ensure the finalizer is present so a future delete
	// routes through handleDeletion. The metadata Update is persisted
	// before any side effects below; controller-runtime writes the new
	// resourceVersion back into resv, so the subsequent status updates
	// (advancePhase) and instruction creates proceed in this same pass —
	// no extra reconcile needed.
	if controllerutil.AddFinalizer(&resv, ReservationFinalizer) {
		if err := r.Update(ctx, &resv); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
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
	case brokerv1alpha1.ReservationPhaseGeneratingKubeconfig:
		return r.handleGeneratingKubeconfig(ctx, log, &resv)
	case brokerv1alpha1.ReservationPhaseKubeconfigReady:
		return r.handleKubeconfigReady(ctx, log, &resv)
	case brokerv1alpha1.ReservationPhaseUnpeering:
		return r.handleUnpeering(ctx, log, &resv)
	case brokerv1alpha1.ReservationPhaseReleased,
		brokerv1alpha1.ReservationPhaseFailed,
		brokerv1alpha1.ReservationPhaseExpired:
		return r.handleTerminal(ctx, log, &resv)
	default:
		// Peering / Peered are moved by the instruction-result handler.
		return ctrl.Result{}, nil
	}
}

// -----------------------------------------------------------------------------
// Pending → GeneratingKubeconfig
// -----------------------------------------------------------------------------

func (r *ReservationReconciler) handlePending(
	ctx context.Context, log logr.Logger, resv *brokerv1alpha1.Reservation,
) (ctrl.Result, error) {
	// Fast-path: if the (consumer, provider) peering-user kubeconfig has
	// already been generated for a sibling Reservation, reuse it and skip
	// GenerateKubeconfig entirely. Re-issuing it would make the provider
	// run `liqoctl generate peering-user` a second time and fail with
	// "CSR already exists". See bug #5.
	exists, err := r.kubeconfigSecretExists(ctx, resv)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check kubeconfig secret: %w", err)
	}
	if exists {
		if err := r.advancePhase(ctx, resv, brokerv1alpha1.ReservationPhaseKubeconfigReady,
			"reusing cached peering-user kubeconfig"); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("reused cached kubeconfig; skipped GenerateKubeconfig",
			"provider", resv.Spec.ProviderClusterID, "consumer", resv.Spec.ConsumerClusterID)
		return ctrl.Result{}, nil
	}

	pi := &autoscalingv1alpha1.ProviderInstruction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      providerInstructionGKName(resv.Spec.ConsumerClusterID, resv.Spec.ProviderClusterID),
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

	// Shared instruction: no controller-owner reference. A GenerateKubeconfig
	// outlives the Reservation that first issued it (a sibling may still need
	// the credential), so it must not be garbage-collected when that one
	// Reservation is deleted. AlreadyExists is tolerated so two Pending
	// Reservations racing to the same provider don't double-issue.
	if err := r.ensureSharedInstruction(ctx, pi); err != nil {
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
// GeneratingKubeconfig → KubeconfigReady (sibling fast-forward)
// -----------------------------------------------------------------------------

// gkResyncInterval is how often a Reservation parked in
// GeneratingKubeconfig re-checks for the shared kubeconfig Secret.
const gkResyncInterval = 5 * time.Second

// handleGeneratingKubeconfig advances a Reservation to KubeconfigReady
// as soon as the shared (consumer, provider) kubeconfig Secret exists.
//
// The Reservation that issued the GenerateKubeconfig instruction is
// advanced directly by the API result handler when the provider reports
// back. But a *sibling* Reservation that entered GeneratingKubeconfig
// before that result landed (it raced past the handlePending fast-path
// while the Secret didn't yet exist) has no instruction of its own to
// trigger it — so it polls here until the credential materialises. The
// reconciler has no Secret watch, hence the bounded RequeueAfter.
func (r *ReservationReconciler) handleGeneratingKubeconfig(
	ctx context.Context, log logr.Logger, resv *brokerv1alpha1.Reservation,
) (ctrl.Result, error) {
	exists, err := r.kubeconfigSecretExists(ctx, resv)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check kubeconfig secret: %w", err)
	}
	if !exists {
		// Credential not ready yet; check back shortly.
		return ctrl.Result{RequeueAfter: gkResyncInterval}, nil
	}
	if err := r.advancePhase(ctx, resv, brokerv1alpha1.ReservationPhaseKubeconfigReady,
		"peering-user kubeconfig delivered"); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("kubeconfig ready; advancing to KubeconfigReady",
		"provider", resv.Spec.ProviderClusterID, "consumer", resv.Spec.ConsumerClusterID)
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
			KubeconfigRef:         kubeconfigSecretName(resv.Spec.ConsumerClusterID, resv.Spec.ProviderClusterID),
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
// Released / Failed / Expired → emit ProviderInstruction{Cleanup}
// -----------------------------------------------------------------------------

// handleTerminal credits chunks back and, when this is the *last*
// Reservation for its (consumer, provider) pair, tears down the shared
// peering-user credential: it queues a ProviderInstruction{Cleanup} so
// the provider agent drops the peering-user ServiceAccount/RoleBinding
// and deletes the staging kubeconfig Secret.
//
// Because the credential is shared across Reservations (bug #5), the
// teardown is ref-counted: if any *other* non-terminal Reservation still
// targets the same (consumer, provider), cleanup is skipped and the
// Reservation is requeued so the teardown fires once that sibling also
// terminates. This closes the race where two siblings reach a terminal
// phase near-simultaneously and both see the other as still active.
//
// The presence of the staging Secret is the signal that provider work
// actually happened: a Pending → Failed direct transition never created
// it, so there is nothing to clean up. (The Secret is more durable than
// the GenerateKubeconfig instruction, which the shared-bookkeeping GC
// reclaims after DefaultEnforcedTTL.)
//
// The cleanup is emitted even when the provider's ClusterAdvertisement
// is currently unavailable: the instruction sits in etcd until the
// provider re-appears and replays its instruction queue.
func (r *ReservationReconciler) handleTerminal(
	ctx context.Context, log logr.Logger, resv *brokerv1alpha1.Reservation,
) (ctrl.Result, error) {
	// Release the reservation's chunk count back to the provider's
	// AvailableChunks budget if no prior path has already done so. The
	// API DELETE handler stamps ChunksReleasedAnnotation when the
	// normal consumer-initiated release decrements; here we cover the
	// other terminal entry points (Failed / Expired from any
	// non-Unpeering phase) where nothing else credited the chunks back.
	if !brokerv1alpha1.IsChunksReleased(resv) {
		if err := r.releaseChunks(ctx, resv); err != nil {
			return ctrl.Result{}, fmt.Errorf("release chunks: %w", err)
		}
		log.Info("released reserved chunks", "provider", resv.Spec.ProviderClusterID,
			"chunkCount", resv.Spec.ChunkCount, "phase", resv.Status.Phase)
	}

	// No provider work ever happened (Pending → Failed direct) → no
	// peering-user to tear down. The staging Secret is the durable marker.
	exists, err := r.kubeconfigSecretExists(ctx, resv)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check kubeconfig secret: %w", err)
	}
	if !exists {
		return ctrl.Result{}, nil
	}

	// Per-reservation consumer cleanup (bug #7). Unlike the SHARED
	// peering-user credential torn down below, the consumer's Liqo state
	// (ResourceSlice / VirtualNodeState) is per-reservation — so it must
	// be cleaned regardless of the sibling ref-count gate, and emitted
	// BEFORE it. Without this, a Peered reservation that reaches a
	// terminal phase by EXPIRY/Failure (rather than the normal Unpeering
	// path, which already deletes it) leaks the consumer's ResourceSlice
	// and virtual node. Emitted un-owned (survives the Reservation's GC)
	// and idempotent: a harmless no-op when the consumer never peered or
	// already unpeered.
	if err := r.ensureConsumerCleanupShared(ctx, resv); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure consumer Cleanup: %w", err)
	}

	// Ref-count: a sibling Reservation may still need the shared
	// credential. Skip teardown and re-check shortly.
	others, err := r.hasOtherActiveReservation(ctx, resv)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check sibling reservations: %w", err)
	}
	if others {
		log.V(1).Info("peering-user still in use by a sibling reservation; deferring cleanup",
			"provider", resv.Spec.ProviderClusterID, "consumer", resv.Spec.ConsumerClusterID)
		return ctrl.Result{RequeueAfter: gkResyncInterval}, nil
	}

	if err := r.ensureProviderCleanupInstruction(ctx, resv); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure ProviderInstruction Cleanup: %w", err)
	}
	if err := r.deleteKubeconfigSecret(ctx, resv); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete staging kubeconfig secret: %w", err)
	}
	log.Info("queued provider Cleanup and removed staging kubeconfig",
		"provider", resv.Spec.ProviderClusterID, "consumer", resv.Spec.ConsumerClusterID, "phase", resv.Status.Phase)
	return ctrl.Result{}, nil
}

// handleDeletion runs when a Reservation carries a DeletionTimestamp.
// It credits the reservation's chunks back to the provider (idempotent
// via the ChunksReleased annotation), then removes ReservationFinalizer
// so the API server can finish deleting the object. Re-fetches before
// removing the finalizer because releaseChunks may have stamped the
// annotation and bumped the resourceVersion.
func (r *ReservationReconciler) handleDeletion(
	ctx context.Context, log logr.Logger, resv *brokerv1alpha1.Reservation,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(resv, ReservationFinalizer) {
		// Another reconcile already finalized; nothing to do.
		return ctrl.Result{}, nil
	}

	if !brokerv1alpha1.IsChunksReleased(resv) {
		if err := r.releaseChunks(ctx, resv); err != nil {
			return ctrl.Result{}, fmt.Errorf("release chunks on delete: %w", err)
		}
		log.Info("released reserved chunks on delete", "provider", resv.Spec.ProviderClusterID,
			"chunkCount", resv.Spec.ChunkCount, "phase", resv.Status.Phase)
	}

	// Tell the consumer agent to tear down its per-reservation Liqo state
	// (ResourceSlice / VirtualNodeState / kubeconfig Secret). The broker
	// can't delete cross-cluster resources directly, so this goes through
	// a Cleanup instruction. It is emitted UN-OWNED (shared) so it
	// survives this Reservation's own deletion — an owner-referenced
	// instruction would be garbage-collected along with the Reservation
	// before the agent could fetch it. The consumer Cleanup handler is
	// idempotent on missing, so emitting it even when no consumer state
	// exists (e.g. a Pending reservation deleted before peering) is a
	// harmless no-op. Without this, a `kubectl delete reservation` (or any
	// hard delete) leaks the consumer's ResourceSlice + virtual node.
	if err := r.ensureConsumerCleanupShared(ctx, resv); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure consumer cleanup on delete: %w", err)
	}

	for attempt := 0; attempt < 3; attempt++ {
		fresh := &brokerv1alpha1.Reservation{}
		if err := r.Get(ctx, types.NamespacedName{
			Name: resv.Name, Namespace: resv.Namespace,
		}, fresh); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if !controllerutil.RemoveFinalizer(fresh, ReservationFinalizer) {
			return ctrl.Result{}, nil
		}
		if err := r.Update(ctx, fresh); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, fmt.Errorf("exhausted conflict retries removing finalizer")
}

// releaseChunks decrements the provider's ReservedChunks by
// resv.Spec.ChunkCount and stamps ChunksReleasedAnnotation on the
// Reservation so subsequent reconciles know not to release again.
// Conflict-retry up to 3 times on both the ClusterAdvertisement status
// update and the Reservation annotation update. Idempotent: the
// annotation gates a re-entry from the next reconcile.
func (r *ReservationReconciler) releaseChunks(
	ctx context.Context, resv *brokerv1alpha1.Reservation,
) error {
	for attempt := 0; attempt < 3; attempt++ {
		cadv := &brokerv1alpha1.ClusterAdvertisement{}
		if err := r.Get(ctx, types.NamespacedName{
			Name: resv.Spec.ProviderClusterID, Namespace: resv.Namespace,
		}, cadv); err != nil {
			if apierrors.IsNotFound(err) {
				// Provider gone — nothing to credit back. Still stamp
				// the annotation so we don't try again on the next
				// reconcile.
				return r.stampChunksReleased(ctx, resv)
			}
			return fmt.Errorf("get ClusterAdvertisement: %w", err)
		}
		next := cadv.Status.ReservedChunks - resv.Spec.ChunkCount
		if next < 0 {
			next = 0
		}
		cadv.Status.ReservedChunks = next
		avail := cadv.Status.TotalChunks - next
		if avail < 0 {
			avail = 0
		}
		cadv.Status.AvailableChunks = avail
		if err := r.Status().Update(ctx, cadv); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return fmt.Errorf("update ClusterAdvertisement status: %w", err)
		}
		return r.stampChunksReleased(ctx, resv)
	}
	return fmt.Errorf("exhausted conflict retries updating ClusterAdvertisement %q",
		resv.Spec.ProviderClusterID)
}

// stampChunksReleased sets the ChunksReleasedAnnotation on the
// Reservation, re-fetching to avoid stale-conflict on update. Idempotent.
func (r *ReservationReconciler) stampChunksReleased(
	ctx context.Context, resv *brokerv1alpha1.Reservation,
) error {
	for attempt := 0; attempt < 3; attempt++ {
		fresh := &brokerv1alpha1.Reservation{}
		if err := r.Get(ctx, types.NamespacedName{
			Name: resv.Name, Namespace: resv.Namespace,
		}, fresh); err != nil {
			return err
		}
		if brokerv1alpha1.IsChunksReleased(fresh) {
			return nil
		}
		brokerv1alpha1.MarkChunksReleased(fresh)
		if err := r.Update(ctx, fresh); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("exhausted conflict retries stamping chunks-released annotation")
}

// ensureProviderCleanupInstruction emits a ProviderInstruction{Cleanup}
// that tears down the shared (consumer, provider) peering-user. Like the
// GenerateKubeconfig it pairs with, it is keyed by (consumer, provider)
// and carries no controller-owner reference — it must outlive the
// Reservation being torn down (which is about to be garbage-collected)
// so the provider agent can still fetch it. Idempotent: re-running when
// the Cleanup already exists is a no-op.
func (r *ReservationReconciler) ensureProviderCleanupInstruction(
	ctx context.Context, resv *brokerv1alpha1.Reservation,
) error {
	pi := &autoscalingv1alpha1.ProviderInstruction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      providerInstructionCleanupName(resv.Spec.ConsumerClusterID, resv.Spec.ProviderClusterID),
			Namespace: resv.Namespace,
		},
		Spec: autoscalingv1alpha1.ProviderInstructionSpec{
			ReservationID:         resv.Name,
			Kind:                  autoscalingv1alpha1.ProviderInstructionCleanup,
			TargetClusterID:       resv.Spec.ProviderClusterID,
			ConsumerClusterID:     resv.Spec.ConsumerClusterID,
			ConsumerLiqoClusterID: resv.Spec.ConsumerLiqoClusterID,
			ChunkCount:            resv.Spec.ChunkCount,
		},
	}
	return r.ensureSharedInstruction(ctx, pi)
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

// ensureSharedInstruction creates an instruction with NO controller-owner
// reference — used for the (consumer, provider)-scoped GenerateKubeconfig
// and provider Cleanup, which are shared across Reservations and so must
// not be garbage-collected when any single Reservation is deleted. Their
// lifecycle is the shared-bookkeeping DefaultEnforcedTTL GC, not owner
// references. Idempotent on AlreadyExists.
func (r *ReservationReconciler) ensureSharedInstruction(
	ctx context.Context, instruction client.Object,
) error {
	if err := r.Create(ctx, instruction); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	return nil
}

// kubeconfigSecretExists reports whether the shared (consumer, provider)
// staging kubeconfig Secret is already present in the Reservation's
// namespace.
func (r *ReservationReconciler) kubeconfigSecretExists(
	ctx context.Context, resv *brokerv1alpha1.Reservation,
) (bool, error) {
	var sec corev1.Secret
	err := r.Get(ctx, types.NamespacedName{
		Name:      kubeconfigSecretName(resv.Spec.ConsumerClusterID, resv.Spec.ProviderClusterID),
		Namespace: resv.Namespace,
	}, &sec)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// deleteKubeconfigSecret removes the shared staging kubeconfig Secret.
// Idempotent on missing. Called only once the last Reservation for a
// (consumer, provider) pair terminates, so a future Reservation re-runs
// GenerateKubeconfig against a freshly minted peering-user rather than
// fast-pathing onto a credential whose peering-user was just torn down.
func (r *ReservationReconciler) deleteKubeconfigSecret(
	ctx context.Context, resv *brokerv1alpha1.Reservation,
) error {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeconfigSecretName(resv.Spec.ConsumerClusterID, resv.Spec.ProviderClusterID),
			Namespace: resv.Namespace,
		},
	}
	if err := r.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// hasOtherActiveReservation reports whether any Reservation *other* than
// resv targets the same (consumer, provider) pair and is still alive —
// i.e. not in a terminal phase and not being deleted. Used to ref-count
// the shared peering-user so its teardown waits for the last sibling.
func (r *ReservationReconciler) hasOtherActiveReservation(
	ctx context.Context, resv *brokerv1alpha1.Reservation,
) (bool, error) {
	var list brokerv1alpha1.ReservationList
	if err := r.List(ctx, &list, client.InNamespace(resv.Namespace)); err != nil {
		return false, err
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == resv.Name {
			continue
		}
		if other.Spec.ConsumerClusterID != resv.Spec.ConsumerClusterID ||
			other.Spec.ProviderClusterID != resv.Spec.ProviderClusterID {
			continue
		}
		if !other.DeletionTimestamp.IsZero() {
			continue
		}
		switch other.Status.Phase {
		case brokerv1alpha1.ReservationPhaseReleased,
			brokerv1alpha1.ReservationPhaseFailed,
			brokerv1alpha1.ReservationPhaseExpired:
			continue
		}
		return true, nil
	}
	return false, nil
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
	return r.emitConsumerCleanup(ctx, resv, true /* owned */)
}

// ensureConsumerCleanupShared emits the same consumer Cleanup instruction
// as ensureCleanupInstruction but WITHOUT an owner reference, so it
// survives the Reservation's own deletion. Used by handleDeletion, where
// an owned instruction would be garbage-collected with the Reservation
// before the consumer agent could fetch it.
func (r *ReservationReconciler) ensureConsumerCleanupShared(
	ctx context.Context, resv *brokerv1alpha1.Reservation,
) error {
	return r.emitConsumerCleanup(ctx, resv, false /* shared/un-owned */)
}

// emitConsumerCleanup builds the consumer-side Cleanup instruction and
// creates it either owned by the Reservation (owned=true, for live-object
// callers like checkProviderAvailable) or un-owned (owned=false, for the
// deletion path). Idempotent on AlreadyExists.
func (r *ReservationReconciler) emitConsumerCleanup(
	ctx context.Context, resv *brokerv1alpha1.Reservation, owned bool,
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
	if owned {
		return r.ensureInstruction(ctx, resv, ri)
	}
	return r.ensureSharedInstruction(ctx, ri)
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

// peeringCredentialKey is the (consumer, provider)-scoped identity that
// the GenerateKubeconfig instruction, the staging kubeconfig Secret, and
// the provider-side Cleanup all key off. The Liqo peering-user the
// provider mints in response to GenerateKubeconfig is a singleton per
// (consumer, provider) pair — re-issuing it for a second Reservation
// fails with "CSR already exists" — so every artefact tied to that
// credential must be shared across Reservations rather than named per
// Reservation. See docs/design.md §5.3 / bug #5.
//
// The same scheme is mirrored in internal/broker/api/instructions.go
// (which persists the Secret on the GenerateKubeconfig result); the two
// must agree on these names to hand the credential off cleanly. The
// duplication is intentional — a controller MUST NOT import the HTTP
// layer.
func peeringCredentialKey(consumerClusterID, providerClusterID string) string {
	return consumerClusterID + "-" + providerClusterID
}

// providerInstructionGKName is keyed by (consumer, provider): one
// GenerateKubeconfig per credential, shared by every Reservation from
// that consumer to that provider.
func providerInstructionGKName(consumerClusterID, providerClusterID string) string {
	return "gk-" + peeringCredentialKey(consumerClusterID, providerClusterID)
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

// providerInstructionCleanupName is keyed by (consumer, provider) — like
// the GenerateKubeconfig it pairs with — because it tears down the
// shared peering-user. It uses a distinct prefix from the consumer-side
// cleanup-<resv> so the two terminal cleanups are trivially
// distinguishable in `kubectl get` output even though they live in
// different CRD kinds.
func providerInstructionCleanupName(consumerClusterID, providerClusterID string) string {
	return "pcleanup-" + peeringCredentialKey(consumerClusterID, providerClusterID)
}

// kubeconfigSecretName must match the value the API instruction handler
// (internal/broker/api/instructions.go) uses when persisting the
// peering-user kubeconfig. Keyed by (consumer, provider): the staging
// Secret is the shared credential, reused by every Reservation for that
// pair (this is what lets a second Reservation fast-path past
// GenerateKubeconfig entirely).
func kubeconfigSecretName(consumerClusterID, providerClusterID string) string {
	return "kubeconfig-" + peeringCredentialKey(consumerClusterID, providerClusterID)
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
