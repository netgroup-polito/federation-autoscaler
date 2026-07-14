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

package autoscaling

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/agentclient"
)

// resourceRequestFinalizer keeps a user-created ResourceRequest around long
// enough for the controller to release its broker reservation (unpeer + free
// chunks) before the object is removed — the fix for the origin's delete-leak.
const resourceRequestFinalizer = "autoscaling.federation-autoscaler.io/release-reservation"

// gpuResourceName is the resource key that classifies a request (and a chunk)
// as GPU-typed.
const gpuResourceName corev1.ResourceName = "nvidia.com/gpu"

// pendingRequeue / failedRequeue / holdRequeue bound how often the controller
// retries a request that could not (yet) be reserved, and how often it re-asserts
// scale-down protection on a held reservation's node(s).
const (
	pendingRequeue = 15 * time.Second
	failedRequeue  = 30 * time.Second
	holdRequeue    = 60 * time.Second
)

// scaleDownDisabledAnnotation is the well-known Cluster Autoscaler node
// annotation that excludes a node from scale-down. A manual reservation holds
// capacity with NO pods on it, so without this CA would see an empty borrowed
// node, wait scale-down-unneeded-time, and delete it — releasing the reservation
// the user explicitly asked to keep. We set it on the reservation's node(s).
const scaleDownDisabledAnnotation = "cluster-autoscaler.kubernetes.io/scale-down-disabled"

// scaleDownDisabledValue is the annotation's "on" value.
const scaleDownDisabledValue = "true"

// virtualNodeReservationLabel keys a VirtualNodeState to its Broker reservation.
// MUST match VirtualNodeStateReservationLabel in
// internal/agent/consumer/instructions (the consumer agent stamps it).
const virtualNodeReservationLabel = "federation-autoscaler.io/reservation"

// ReservationClient is the subset of the consumer agent's loopback client the
// controller needs. *agentclient.Client satisfies it; tests supply a fake. It is
// the SAME path the gRPC server's NodeGroupIncreaseSize / DeleteNodes use, so a
// manual request reuses the entire reservation + peering machine unchanged.
type ReservationClient interface {
	GetNodeGroups(ctx context.Context) (*brokerapi.NodeGroupListResponse, error)
	PostReservation(ctx context.Context, reservationID string, req *brokerapi.ReservationRequest) (*brokerapi.ReservationResponse, error)
	DeleteReservation(ctx context.Context, reservationID string) (*brokerapi.ReleaseResponse, error)
}

// ResourceRequestReconciler turns a user-created ResourceRequest into a broker
// reservation (create → reserve + peer) and releases it on delete (delete →
// unpeer + free), reusing the Cluster-Autoscaler reservation path via the
// co-located consumer agent's loopback API.
type ResourceRequestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Agent  ReservationClient

	// ReEvalInterval is how often an active manual reservation is re-evaluated for
	// a better provider (feature 7), and the minimum gap between two migrations of
	// the same reservation (the anti-flap debounce). 0 disables re-evaluation.
	ReEvalInterval time.Duration
}

// +kubebuilder:rbac:groups=autoscaling.federation-autoscaler.io,resources=resourcerequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.federation-autoscaler.io,resources=resourcerequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.federation-autoscaler.io,resources=resourcerequests/finalizers,verbs=update
// +kubebuilder:rbac:groups=autoscaling.federation-autoscaler.io,resources=virtualnodestates,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch

// Reconcile drives one ResourceRequest. The reservation ID is derived
// deterministically from the object UID ("mr-<uid>") so a re-post is idempotent
// at the broker even if a status write was lost.
func (r *ResourceRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var rr autoscalingv1alpha1.ResourceRequest
	if err := r.Get(ctx, req.NamespacedName, &rr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if r.Agent == nil {
		return ctrl.Result{}, fmt.Errorf("resourcerequest: agent client not configured")
	}

	reservationID := "mr-" + string(rr.UID)

	// Deletion: release the reservation, then drop the finalizer.
	if !rr.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&rr, resourceRequestFinalizer) {
			if rr.Status.ReservationID != "" {
				// A NotFound means the reservation is already gone (e.g. it was
				// released before this fix, or a race) — treat it as released so the
				// finalizer can be dropped instead of getting stuck retrying.
				if _, err := r.Agent.DeleteReservation(ctx, rr.Status.ReservationID); err != nil && !agentclient.IsNotFound(err) {
					log.Error(err, "releasing manual reservation failed; will retry",
						"reservationId", rr.Status.ReservationID)
					return ctrl.Result{}, err
				}
				log.V(1).Info("released manual reservation", "reservationId", rr.Status.ReservationID)
			}
			controllerutil.RemoveFinalizer(&rr, resourceRequestFinalizer)
			if err := r.Update(ctx, &rr); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure the finalizer before we ever reserve, so a delete can always release.
	if !controllerutil.ContainsFinalizer(&rr, resourceRequestFinalizer) {
		controllerutil.AddFinalizer(&rr, resourceRequestFinalizer)
		if err := r.Update(ctx, &rr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Already reserved. Complete an in-flight migration, else periodically
	// re-evaluate for a better provider (feature 7 — manual reservations only),
	// else keep the borrowed node(s) protected from CA scale-down. Release is
	// delete-driven; this branch never releases on its own except to migrate.
	if rr.Status.ReservationID != "" {
		if rr.Status.Phase == autoscalingv1alpha1.ResourceRequestMigrating {
			return r.completeMigration(ctx, &rr)
		}
		if res, migrated, err := r.maybeStartMigration(ctx, &rr); err != nil || migrated {
			return res, err
		}
		return r.holdReservation(ctx, &rr)
	}

	// Choose a provider (broker node groups are already policy-masked, same as CA
	// sees), size the request into whole chunks, and reserve.
	resp, err := r.Agent.GetNodeGroups(ctx)
	if err != nil {
		return r.setPending(ctx, &rr, "broker node groups unavailable: "+err.Error())
	}
	group, count, ok := pickProviderAndSize(resp.NodeGroups, rr.Spec.Resources)
	if !ok {
		return r.setPending(ctx, &rr, "no provider with enough free capacity yet")
	}

	resReq := &brokerapi.ReservationRequest{
		ProviderClusterID: group.ProviderClusterID,
		NodeGroupID:       group.ID,
		ChunkCount:        count,
		ChunkType:         group.Type,
	}
	if _, err := r.Agent.PostReservation(ctx, reservationID, resReq); err != nil {
		return r.setFailed(ctx, &rr, "reservation failed: "+err.Error())
	}
	log.V(1).Info("created manual reservation",
		"reservationId", reservationID, "provider", group.ProviderClusterID, "chunks", count)
	return r.setReserved(ctx, &rr, reservationID, group.ProviderClusterID, count)
}

// pickProviderAndSize chooses the growable node group with the most free
// capacity that can fully hold the request, and returns the chunk count to
// reserve there. ok=false means no provider currently fits (stay Pending).
func pickProviderAndSize(groups []brokerapi.NodeGroupView, req corev1.ResourceList) (*brokerapi.NodeGroupView, int32, bool) {
	wantGPU := gpuRequested(req)
	var best *brokerapi.NodeGroupView
	var bestCount, bestAvail int32
	for i := range groups {
		g := &groups[i]
		if wantGPU != (g.Type == brokerv1alpha1.ChunkTypeGPU) {
			continue
		}
		avail := g.MaxSize - g.CurrentReserved
		if avail <= 0 {
			continue
		}
		count := chunksFor(req, g.ChunkResources)
		if count <= 0 || count > avail {
			continue
		}
		if best == nil || avail > bestAvail {
			best, bestCount, bestAvail = g, count, avail
		}
	}
	if best == nil {
		return nil, 0, false
	}
	return best, bestCount, true
}

// chunksFor rounds a request UP to whole chunks: for each requested resource,
// ceil(requested / per-chunk), taking the max across resources. Returns 0 when a
// requested resource is absent from the chunk (that group cannot satisfy it).
func chunksFor(req, chunk corev1.ResourceList) int32 {
	var n int64
	for name, q := range req {
		rv := q.MilliValue()
		if rv <= 0 {
			continue
		}
		cq, ok := chunk[name]
		cv := cq.MilliValue()
		if !ok || cv <= 0 {
			return 0
		}
		if need := (rv + cv - 1) / cv; need > n {
			n = need
		}
	}
	if n < 1 {
		return 0
	}
	return int32(n)
}

// gpuRequested reports whether the request asks for at least one GPU.
func gpuRequested(req corev1.ResourceList) bool {
	q, ok := req[gpuResourceName]
	return ok && q.MilliValue() > 0
}

// -----------------------------------------------------------------------------
// status helpers
// -----------------------------------------------------------------------------

func (r *ResourceRequestReconciler) setPending(ctx context.Context, rr *autoscalingv1alpha1.ResourceRequest, msg string) (ctrl.Result, error) {
	if err := r.writePhase(ctx, rr, autoscalingv1alpha1.ResourceRequestPending, msg, "", "", 0); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: pendingRequeue}, nil
}

func (r *ResourceRequestReconciler) setFailed(ctx context.Context, rr *autoscalingv1alpha1.ResourceRequest, msg string) (ctrl.Result, error) {
	if err := r.writePhase(ctx, rr, autoscalingv1alpha1.ResourceRequestFailed, msg, "", "", 0); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: failedRequeue}, nil
}

func (r *ResourceRequestReconciler) setReserved(ctx context.Context, rr *autoscalingv1alpha1.ResourceRequest, resID, provider string, count int32) (ctrl.Result, error) {
	msg := fmt.Sprintf("reserved %d chunk(s) on %s", count, provider)
	if err := r.writePhase(ctx, rr, autoscalingv1alpha1.ResourceRequestReserved, msg, resID, provider, count); err != nil {
		return ctrl.Result{}, err
	}
	// Requeue so we can protect the node(s) once Liqo materialises them.
	return ctrl.Result{RequeueAfter: pendingRequeue}, nil
}

// -----------------------------------------------------------------------------
// re-evaluation + migration (feature 7)
// -----------------------------------------------------------------------------

// isStableMetricPolicy reports whether a placement policy migrates a placed
// reservation on a MEANINGFUL, stable metric change (cheaper / greener / lower
// measured RTT). Standard (most-free-capacity) and no-policy are excluded: their
// winner shifts as capacity fills, which would make a placed reservation wander.
func isStableMetricPolicy(p autoscalingv1alpha1.PlacementStrategy) bool {
	switch p {
	case autoscalingv1alpha1.PlacementStrategyPrice,
		autoscalingv1alpha1.PlacementStrategyEco,
		autoscalingv1alpha1.PlacementStrategyLatency:
		return true
	default:
		return false
	}
}

// migrationMetricMargin is the minimum RELATIVE improvement a candidate provider
// must show over the current one to justify migrating a placed reservation. It
// guards against equal-metric / floating-point ping-pong; genuine flap is
// additionally bounded by ReEvalInterval. Placement metrics are non-negative
// (per-chunk cost, weighted carbon, distance in km).
const migrationMetricMargin = 1e-6

// metricStrictlyBetter reports whether candidate is better (lower) than current by
// more than migrationMetricMargin. When current is 0 (e.g. co-located, distance 0)
// nothing can beat it, so a placed reservation there never migrates on that metric.
func metricStrictlyBetter(candidate, current float64) bool {
	return candidate < current-current*migrationMetricMargin
}

// findProviderView returns the view for the given provider + chunk type, or nil
// when that provider is no longer in the advertised (masked) list.
func findProviderView(groups []brokerapi.NodeGroupView, providerID string, t brokerv1alpha1.ChunkType) *brokerapi.NodeGroupView {
	for i := range groups {
		if groups[i].ProviderClusterID == providerID && groups[i].Type == t {
			return &groups[i]
		}
	}
	return nil
}

// nextReservationID returns the reservation id for the request's NEXT migration
// attempt: a FRESH id ("mr-<uid>-m<n>") so the new provider's broker reservation
// and consumer artifacts (vns-/rs-/kubeconfig-<id>) never collide with the old,
// still-terminating ones. The first (never-migrated) reservation keeps "mr-<uid>".
func (r *ResourceRequestReconciler) nextReservationID(rr *autoscalingv1alpha1.ResourceRequest) string {
	return fmt.Sprintf("mr-%s-m%d", rr.UID, rr.Status.MigrationCount+1)
}

// maybeStartMigration re-evaluates an active manual reservation against the
// current best provider and, when a strictly-different provider wins under a
// stable-metric policy (Price/Eco/Latency), begins a break-before-make migration:
// it releases the current reservation (unpeer old → pods evicted) and moves the
// request into the Migrating phase with a fresh reservation id. The next reconcile
// peers the new provider (completeMigration). Returns migrated=true when a
// migration was started (the caller returns immediately); migrated=false means
// "no change — keep holding".
//
// Re-eval is gated by ReEvalInterval, which doubles as the debounce / anti-flap
// floor: a reservation is re-evaluated (and can migrate) at most once per interval.
func (r *ResourceRequestReconciler) maybeStartMigration(ctx context.Context, rr *autoscalingv1alpha1.ResourceRequest) (ctrl.Result, bool, error) {
	log := ctrl.LoggerFrom(ctx)
	if r.ReEvalInterval <= 0 {
		return ctrl.Result{}, false, nil // re-eval disabled
	}
	if rr.Status.LastTransitionTime != nil && time.Since(rr.Status.LastTransitionTime.Time) < r.ReEvalInterval {
		return ctrl.Result{}, false, nil // debounced: too soon since the last transition
	}

	resp, err := r.Agent.GetNodeGroups(ctx)
	if err != nil {
		log.V(1).Info("re-eval: node groups unavailable; holding", "err", err.Error())
		return ctrl.Result{}, false, nil // transient — keep holding, try next tick
	}
	// Only migrate under a stable-metric policy — never Standard / no policy.
	if !isStableMetricPolicy(resp.AppliedPlacement) {
		return ctrl.Result{}, false, nil
	}
	// The masked, growable survivor within the request's chunk type is the current
	// best provider with head-room (for Latency this re-probed live via GetNodeGroups).
	group, _, ok := pickProviderAndSize(resp.NodeGroups, rr.Spec.Resources)
	if !ok || group.ProviderClusterID == rr.Status.ProviderClusterID {
		return ctrl.Result{}, false, nil // no growable alternative, or already the winner
	}

	// Guard against the self-occupancy confound. When THIS reservation fills its
	// (still-best) provider, that provider is masked as non-growable and the Broker
	// exposes the next-best one as the only survivor — so the growable "winner" above
	// is a spill target, not a genuine improvement. Migrating to it and back would
	// churn the peering every interval. Only migrate when the current provider has
	// left the advertised list, or the winner is STRICTLY better by the applied
	// policy's metric. The metric is occupancy-independent, so a merely-full current
	// provider (lower/equal metric) blocks the move.
	if cur := findProviderView(resp.NodeGroups, rr.Status.ProviderClusterID, group.Type); cur != nil {
		if !cur.HasMetric || !group.HasMetric || !metricStrictlyBetter(group.PlacementMetric, cur.PlacementMetric) {
			return ctrl.Result{}, false, nil // current provider still ≥ as good — it's just full
		}
	}

	// Genuinely better provider → break-before-make. Release the current reservation first.
	oldID := rr.Status.ReservationID
	if _, err := r.Agent.DeleteReservation(ctx, oldID); err != nil && !agentclient.IsNotFound(err) {
		log.Error(err, "re-eval: releasing old reservation failed; will retry", "reservationId", oldID)
		return ctrl.Result{}, false, err
	}
	newID := r.nextReservationID(rr)
	msg := fmt.Sprintf("migrating from %s to %s", rr.Status.ProviderClusterID, group.ProviderClusterID)
	log.Info("re-eval: migrating manual reservation",
		"from", rr.Status.ProviderClusterID, "to", group.ProviderClusterID, "newReservationId", newID)
	if err := r.setMigrating(ctx, rr, newID, msg); err != nil {
		return ctrl.Result{}, false, err
	}
	// Short drain gap before peering the new provider (break-before-make).
	return ctrl.Result{RequeueAfter: pendingRequeue}, true, nil
}

// completeMigration finishes a break-before-make migration. The old reservation
// was already released in maybeStartMigration, so it re-ranks fresh (self-
// correcting if the intended provider filled during the drain gap) and peers the
// best provider under the request's already-assigned fresh reservation id.
func (r *ResourceRequestReconciler) completeMigration(ctx context.Context, rr *autoscalingv1alpha1.ResourceRequest) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	resp, err := r.Agent.GetNodeGroups(ctx)
	if err != nil {
		return ctrl.Result{RequeueAfter: pendingRequeue}, nil // retry the peer
	}
	group, count, ok := pickProviderAndSize(resp.NodeGroups, rr.Spec.Resources)
	if !ok {
		// The old reservation is already gone; wait for a provider with capacity.
		if err := r.writePhase(ctx, rr, autoscalingv1alpha1.ResourceRequestMigrating,
			"migrating — waiting for a provider with capacity", "", "", 0); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: pendingRequeue}, nil
	}
	resReq := &brokerapi.ReservationRequest{
		ProviderClusterID: group.ProviderClusterID,
		NodeGroupID:       group.ID,
		ChunkCount:        count,
		ChunkType:         group.Type,
	}
	if _, err := r.Agent.PostReservation(ctx, rr.Status.ReservationID, resReq); err != nil {
		return r.setFailed(ctx, rr, "migration reservation failed: "+err.Error())
	}
	log.Info("re-eval: migration complete",
		"provider", group.ProviderClusterID, "reservationId", rr.Status.ReservationID, "chunks", count)
	return r.setReserved(ctx, rr, rr.Status.ReservationID, group.ProviderClusterID, count)
}

// setMigrating records the start of a migration: the fresh reservation id, the
// incremented MigrationCount, and the Migrating phase. Unlike writePhase it also
// bumps MigrationCount; the provider is left unchanged until completeMigration
// peers the new one.
func (r *ResourceRequestReconciler) setMigrating(ctx context.Context, rr *autoscalingv1alpha1.ResourceRequest, newID, msg string) error {
	now := metav1.Now()
	rr.Status.Phase = autoscalingv1alpha1.ResourceRequestMigrating
	rr.Status.Message = msg
	rr.Status.ReservationID = newID
	rr.Status.MigrationCount++
	rr.Status.LastTransitionTime = &now
	return r.Status().Update(ctx, rr)
}

// -----------------------------------------------------------------------------
// hold: protect the borrowed node(s) from Cluster Autoscaler scale-down
// -----------------------------------------------------------------------------

// holdReservation keeps a reserved manual request alive: it marks the borrowed
// node(s) so the Cluster Autoscaler will not scale them down (a manual
// reservation has no pods, so CA would otherwise reclaim the empty node), and
// reflects the result in the phase. It never releases anything — release is
// delete-driven.
func (r *ResourceRequestReconciler) holdReservation(ctx context.Context, rr *autoscalingv1alpha1.ResourceRequest) (ctrl.Result, error) {
	protected, err := r.protectReservationNodes(ctx, rr.Status.ReservationID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if protected == 0 {
		// The virtual node has not materialised yet; check again shortly.
		if err := r.writePhase(ctx, rr, autoscalingv1alpha1.ResourceRequestReserved,
			"reserved on "+rr.Status.ProviderClusterID+"; waiting for the virtual node", "", "", 0); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: pendingRequeue}, nil
	}
	msg := fmt.Sprintf("held on %s — %d node(s) protected from autoscaler scale-down", rr.Status.ProviderClusterID, protected)
	if err := r.writePhase(ctx, rr, autoscalingv1alpha1.ResourceRequestActive, msg, "", "", 0); err != nil {
		return ctrl.Result{}, err
	}
	// Re-assert periodically (cheap, idempotent) in case a node is recreated.
	return ctrl.Result{RequeueAfter: holdRequeue}, nil
}

// protectReservationNodes annotates every materialised node backing the given
// reservation with the CA scale-down-disabled annotation, and returns how many
// nodes are now protected. Nodes that have not appeared yet are skipped.
func (r *ResourceRequestReconciler) protectReservationNodes(ctx context.Context, reservationID string) (int, error) {
	var list autoscalingv1alpha1.VirtualNodeStateList
	if err := r.List(ctx, &list, client.MatchingLabels{virtualNodeReservationLabel: reservationID}); err != nil {
		return 0, err
	}
	protected := 0
	for i := range list.Items {
		nodeName := list.Items[i].Status.VirtualNodeName
		if nodeName == "" {
			continue
		}
		ok, err := r.disableScaleDown(ctx, nodeName)
		if err != nil {
			return protected, err
		}
		if ok {
			protected++
		}
	}
	return protected, nil
}

// disableScaleDown sets the CA scale-down-disabled annotation on the named node.
// Returns true when the node exists (already annotated or just annotated); false
// with no error when the node does not exist yet.
func (r *ResourceRequestReconciler) disableScaleDown(ctx context.Context, name string) (bool, error) {
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if node.Annotations[scaleDownDisabledAnnotation] == scaleDownDisabledValue {
		return true, nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[scaleDownDisabledAnnotation] = scaleDownDisabledValue
	if err := r.Patch(ctx, &node, patch); err != nil {
		return false, err
	}
	return true, nil
}

// writePhase updates the status subresource only when something changed,
// stamping LastTransitionTime on a phase change. Non-empty reservation fields
// are written; they are only ever set (on Reserved), never cleared.
func (r *ResourceRequestReconciler) writePhase(
	ctx context.Context, rr *autoscalingv1alpha1.ResourceRequest,
	phase autoscalingv1alpha1.ResourceRequestPhase, msg, resID, provider string, count int32,
) error {
	changed := rr.Status.Phase != phase || rr.Status.Message != msg ||
		(resID != "" && rr.Status.ReservationID != resID) ||
		(provider != "" && rr.Status.ProviderClusterID != provider) ||
		(count != 0 && rr.Status.ChunkCount != count)
	if !changed {
		return nil
	}
	if rr.Status.Phase != phase {
		now := metav1.Now()
		rr.Status.LastTransitionTime = &now
	}
	rr.Status.Phase = phase
	rr.Status.Message = msg
	if resID != "" {
		rr.Status.ReservationID = resID
	}
	if provider != "" {
		rr.Status.ProviderClusterID = provider
	}
	if count != 0 {
		rr.Status.ChunkCount = count
	}
	return r.Status().Update(ctx, rr)
}

// SetupWithManager registers the controller with the grpc-server manager.
func (r *ResourceRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.ResourceRequest{}).
		Named("autoscaling-resourcerequest").
		Complete(r)
}
