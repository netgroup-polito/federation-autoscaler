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
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/broker/chunk"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/agentclient"
)

// resourceRequestFinalizer keeps a user-created ResourceRequest around long
// enough for the controller to release its broker reservation (unpeer + free
// chunks) before the object is removed — the fix for the origin's delete-leak.
const resourceRequestFinalizer = "autoscaling.federation-autoscaler.io/release-reservation"

// gpuResourceName is the resource key that classifies a request (and a chunk)
// as GPU-typed.
const gpuResourceName = chunk.GPUResourceName

// pendingRequeue / failedRequeue / holdRequeue bound how often the controller
// retries a request that could not (yet) be reserved, and how often it re-asserts
// scale-down protection on a held reservation's node(s).
const (
	pendingRequeue = 15 * time.Second
	failedRequeue  = 30 * time.Second
	holdRequeue    = 60 * time.Second
	// waitingRequeue is the quick retry while the agent's AI is still choosing
	// (or the ConsumerChoice policy is still taking effect): the answer usually
	// comes within seconds.
	waitingRequeue = 3 * time.Second
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

// boundedNodeGroups is implemented by agent clients that can pass a
// per-request latency bound to the agent (*agentclient.Client does). It is
// optional so simple ReservationClient fakes keep working; they get the
// unbounded view.
type boundedNodeGroups interface {
	GetNodeGroupsWithin(
		ctx context.Context, bound *autoscalingv1alpha1.LatencyBound,
	) (*brokerapi.NodeGroupListResponse, error)
}

// queryNodeGroups is implemented by agent clients that can pass a whole
// per-request query (*agentclient.Client does): the latency bound, the
// request's own plain-language text and its size. Optional, like
// boundedNodeGroups.
type queryNodeGroups interface {
	GetNodeGroupsFor(ctx context.Context, q localapi.NodeGroupQuery) (*brokerapi.NodeGroupListResponse, error)
}

// nodeGroupsFor fetches the node groups for one ResourceRequest, applying its
// spec.latencyBound (which overrides the ConsumerPolicy bound in local mode),
// its spec.userPrompt (the local AI ranks the providers for that text) and its
// size (the choice skips providers that cannot hold it).
func (r *ResourceRequestReconciler) nodeGroupsFor(
	ctx context.Context, rr *autoscalingv1alpha1.ResourceRequest,
) (*brokerapi.NodeGroupListResponse, error) {
	if q, ok := r.Agent.(queryNodeGroups); ok {
		return q.GetNodeGroupsFor(ctx, localapi.NodeGroupQuery{
			Bound:  rr.Spec.LatencyBound,
			Prompt: rr.Spec.UserPrompt,
			Need:   rr.Spec.Resources,
		})
	}
	if rr.Spec.LatencyBound != nil {
		if b, ok := r.Agent.(boundedNodeGroups); ok {
			return b.GetNodeGroupsWithin(ctx, rr.Spec.LatencyBound)
		}
	}
	return r.Agent.GetNodeGroups(ctx)
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
			for _, id := range heldReservationIDs(&rr) {
				// A NotFound means the reservation is already gone (e.g. it was
				// released before this fix, or a race) — treat it as released so the
				// finalizer can be dropped instead of getting stuck retrying.
				if _, err := r.Agent.DeleteReservation(ctx, id); err != nil && !agentclient.IsNotFound(err) {
					log.Error(err, "releasing manual reservation failed; will retry", "reservationId", id)
					return ctrl.Result{}, err
				}
				log.V(1).Info("released manual reservation", "reservationId", id)
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
		if rr.Status.Phase == autoscalingv1alpha1.ResourceRequestExpired {
			return ctrl.Result{}, nil
		}
		if e := rr.Status.ExpiresAt; e != nil && !time.Now().Before(e.Time) {
			// The broker ends the booking at the same time (it got the same
			// duration); stop holding it instead of waiting for a node that
			// will not come back. The release is a no-op for a booking the
			// broker already ended (NotFound) and ends one that outlived the
			// request, e.g. after a migration.
			for _, id := range heldReservationIDs(&rr) {
				if _, err := r.Agent.DeleteReservation(ctx, id); err != nil && !agentclient.IsNotFound(err) {
					return ctrl.Result{}, err
				}
			}
			msg := fmt.Sprintf("booking ended after its duration (%s) on %s", rr.Spec.Duration, rr.Status.ProviderClusterID)
			return ctrl.Result{}, r.writePhase(ctx, &rr, autoscalingv1alpha1.ResourceRequestExpired, msg, "", "", 0)
		}
		if rr.Status.Phase == autoscalingv1alpha1.ResourceRequestMigrating {
			return r.completeMigration(ctx, &rr)
		}
		if res, migrated, err := r.maybeStartMigration(ctx, &rr); err != nil || migrated {
			return res, err
		}
		res, err := r.holdReservation(ctx, &rr)
		return untilExpiry(res, rr.Status.ExpiresAt), err
	}

	// Choose a provider (broker node groups are already policy-masked, same as CA
	// sees), size the request into whole chunks, and reserve.
	resp, err := r.nodeGroupsFor(ctx, &rr)
	if err != nil {
		return r.setPending(ctx, &rr, "broker node groups unavailable: "+err.Error())
	}
	group, count, ok := pickProviderAndSize(resp.NodeGroups, rr.Spec.Resources)
	if !ok {
		if resp.Waiting != "" {
			// Not "no capacity": the agent's AI is still deciding.
			return r.setWaiting(ctx, &rr, resp.Waiting)
		}
		if b := rr.Spec.LatencyBound; b != nil {
			return r.setPending(ctx, &rr, fmt.Sprintf("no provider with enough free capacity within %d ms yet", b.MaxLatencyMs))
		}
		return r.setPending(ctx, &rr, "no provider with enough free capacity yet")
	}
	if msg, tooBig := blocksOverLimit(count); tooBig {
		return r.setPending(ctx, &rr, msg)
	}

	ids := blockReservationIDs(reservationID, count)
	if err := r.reserveBlocks(ctx, ids, group, bookingTermsOf(&rr)); err != nil {
		return r.setFailed(ctx, &rr, "reservation failed: "+err.Error())
	}
	log.V(1).Info("created manual reservation",
		"reservationIds", ids, "provider", group.ProviderClusterID, "nodes", count)
	return r.setReserved(ctx, &rr, ids, group.ProviderClusterID, count)
}

// reserveBlocks makes one single-block reservation per id on group's provider,
// all or nothing: when one fails, the ones already made are released and the
// error is returned. The ids are deterministic, so a retry re-posts the same
// ones and the broker deduplicates them.
func (r *ResourceRequestReconciler) reserveBlocks(
	ctx context.Context, ids []string, group *brokerapi.NodeGroupView, terms bookingTerms,
) error {
	for i, id := range ids {
		req := &brokerapi.ReservationRequest{
			ProviderClusterID: group.ProviderClusterID,
			NodeGroupID:       group.ID,
			ChunkCount:        1, // one reservation = one block = one node
			ChunkType:         group.Type,
			DurationSeconds:   terms.durationSeconds,
			MaxLatencyMs:      terms.maxLatencyMs,
		}
		if _, err := r.Agent.PostReservation(ctx, id, req); err != nil {
			for _, done := range ids[:i] {
				if _, derr := r.Agent.DeleteReservation(ctx, done); derr != nil && !agentclient.IsNotFound(derr) {
					ctrl.LoggerFrom(ctx).Error(derr, "rolling back a partial request failed; a retry reuses it",
						"reservationId", done)
				}
			}
			return fmt.Errorf("node %d of %d: %w", i+1, len(ids), err)
		}
	}
	return nil
}

// blockReservationIDs returns the reservation ids for a request of count
// blocks: first, then first-c1, first-c2, ...
func blockReservationIDs(first string, count int32) []string {
	ids := []string{first}
	for i := int32(1); i < count; i++ {
		ids = append(ids, fmt.Sprintf("%s-c%d", first, i))
	}
	return ids
}

// heldReservationIDs returns every reservation the request holds: the
// multi-block list when there is one, else the single ReservationID (or none).
func heldReservationIDs(rr *autoscalingv1alpha1.ResourceRequest) []string {
	if len(rr.Status.ReservationIDs) > 0 {
		return rr.Status.ReservationIDs
	}
	if rr.Status.ReservationID != "" {
		return []string{rr.Status.ReservationID}
	}
	return nil
}

// maxBlocksPerRequest caps how many node-sized blocks one request may hold,
// so a single request cannot take a large share of the federation.
const maxBlocksPerRequest = 4

// blocksOverLimit reports whether a request sized to count blocks exceeds
// maxBlocksPerRequest, with the message to surface when it does.
func blocksOverLimit(count int32) (string, bool) {
	if count <= maxBlocksPerRequest {
		return "", false
	}
	return fmt.Sprintf(
		"request needs %d nodes; at most %d per request — lower spec.resources or split it into several requests",
		count, maxBlocksPerRequest), true
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

// chunksFor rounds a request UP to whole chunks (see chunk.CountFor).
func chunksFor(req, perChunk corev1.ResourceList) int32 {
	return chunk.CountFor(req, perChunk)
}

// gpuRequested reports whether the request asks for at least one GPU.
func gpuRequested(req corev1.ResourceList) bool {
	return chunk.WantsGPU(req)
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

// setWaiting keeps the request Pending with the agent's reason (the AI is
// choosing, or the ConsumerChoice policy is taking effect) and retries soon.
func (r *ResourceRequestReconciler) setWaiting(ctx context.Context, rr *autoscalingv1alpha1.ResourceRequest, reason string) (ctrl.Result, error) {
	if err := r.writePhase(ctx, rr, autoscalingv1alpha1.ResourceRequestPending, reason, "", "", 0); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: waitingRequeue}, nil
}

func (r *ResourceRequestReconciler) setFailed(ctx context.Context, rr *autoscalingv1alpha1.ResourceRequest, msg string) (ctrl.Result, error) {
	if err := r.writePhase(ctx, rr, autoscalingv1alpha1.ResourceRequestFailed, msg, "", "", 0); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: failedRequeue}, nil
}

func (r *ResourceRequestReconciler) setReserved(ctx context.Context, rr *autoscalingv1alpha1.ResourceRequest, ids []string, provider string, count int32) (ctrl.Result, error) {
	msg := fmt.Sprintf("reserved %d node(s) on %s", count, provider)
	if len(ids) > 1 {
		rr.Status.ReservationIDs = ids
	} else {
		rr.Status.ReservationIDs = nil
	}
	rr.Status.ExpiresAt = nil
	if d := bookingTermsOf(rr).durationSeconds; d > 0 {
		end := metav1.NewTime(time.Now().Add(time.Duration(d) * time.Second))
		rr.Status.ExpiresAt = &end
	}
	if err := r.writePhase(ctx, rr, autoscalingv1alpha1.ResourceRequestReserved, msg, ids[0], provider, count); err != nil {
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
	if rr.Spec.UserPrompt != "" {
		// Placed by the AI for its own text: there is no stable metric to
		// compare, and asking again would only re-run the model.
		return ctrl.Result{}, false, nil
	}
	if len(rr.Status.ReservationIDs) > 1 {
		return ctrl.Result{}, false, nil // multi-node requests stay where they are
	}
	if rr.Status.LastTransitionTime != nil && time.Since(rr.Status.LastTransitionTime.Time) < r.ReEvalInterval {
		return ctrl.Result{}, false, nil // debounced: too soon since the last transition
	}

	resp, err := r.nodeGroupsFor(ctx, rr)
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
	resp, err := r.nodeGroupsFor(ctx, rr)
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
	if count != 1 {
		// Only reachable if the winning provider's chunk shape shrank mid-flight.
		// The old reservation is already released, so stay Migrating and retry.
		if err := r.writePhase(ctx, rr, autoscalingv1alpha1.ResourceRequestMigrating,
			"migrating — the request no longer fits one node there", "", "", 0); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: pendingRequeue}, nil
	}
	terms := bookingTermsOf(rr)
	resReq := &brokerapi.ReservationRequest{
		ProviderClusterID: group.ProviderClusterID,
		NodeGroupID:       group.ID,
		ChunkCount:        count,
		ChunkType:         group.Type,
		DurationSeconds:   terms.durationSeconds,
		MaxLatencyMs:      terms.maxLatencyMs,
	}
	if _, err := r.Agent.PostReservation(ctx, rr.Status.ReservationID, resReq); err != nil {
		return r.setFailed(ctx, rr, "migration reservation failed: "+err.Error())
	}
	log.Info("re-eval: migration complete",
		"provider", group.ProviderClusterID, "reservationId", rr.Status.ReservationID, "chunks", count)
	return r.setReserved(ctx, rr, []string{rr.Status.ReservationID}, group.ProviderClusterID, count)
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
	ids := heldReservationIDs(rr)
	protected := 0
	for _, id := range ids {
		n, err := r.protectReservationNodes(ctx, id, rr.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		protected += n
	}
	if protected < len(ids) {
		// Not every virtual node has materialised yet; check again shortly.
		msg := "reserved on " + rr.Status.ProviderClusterID + "; waiting for the virtual node"
		if len(ids) > 1 {
			msg = fmt.Sprintf("reserved on %s; %d of %d virtual nodes ready", rr.Status.ProviderClusterID, protected, len(ids))
		}
		if err := r.writePhase(ctx, rr, autoscalingv1alpha1.ResourceRequestReserved, msg, "", "", 0); err != nil {
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
func (r *ResourceRequestReconciler) protectReservationNodes(ctx context.Context, reservationID, holder string) (int, error) {
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
		ok, err := r.disableScaleDown(ctx, nodeName, holder)
		if err != nil {
			return protected, err
		}
		if ok {
			protected++
		}
	}
	return protected, nil
}

// disableScaleDown sets the CA scale-down-disabled annotation on the named node,
// and the HeldByLabel with the holding request's name, so other workloads can
// avoid capacity held for this request. Returns true when the node exists
// (already marked or just marked); false with no error when the node does not
// exist yet.
func (r *ResourceRequestReconciler) disableScaleDown(ctx context.Context, name, holder string) (bool, error) {
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if node.Annotations[scaleDownDisabledAnnotation] == scaleDownDisabledValue &&
		node.Labels[autoscalingv1alpha1.HeldByLabel] == holder {
		return true, nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[scaleDownDisabledAnnotation] = scaleDownDisabledValue
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	node.Labels[autoscalingv1alpha1.HeldByLabel] = holder
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

// bookingTerms are the parts of a ResourceRequest a booking carries: how long
// to keep it, and its hard latency limit (checked by a region orchestrator; the
// consumer agent adds its own location).
type bookingTerms struct {
	durationSeconds int64
	maxLatencyMs    float64
}

// bookingTermsOf reads a ResourceRequest's duration and latency bound. A
// duration that does not parse is ignored (the broker's default applies).
func bookingTermsOf(rr *autoscalingv1alpha1.ResourceRequest) bookingTerms {
	var t bookingTerms
	if d, err := time.ParseDuration(rr.Spec.Duration); err == nil && d > 0 {
		t.durationSeconds = int64(d / time.Second)
	}
	if b := rr.Spec.LatencyBound; b != nil && b.MaxLatencyMs > 0 {
		t.maxLatencyMs = float64(b.MaxLatencyMs)
	}
	return t
}

// untilExpiry wakes the request up no later than its booking's end.
func untilExpiry(res ctrl.Result, expiresAt *metav1.Time) ctrl.Result {
	if expiresAt == nil {
		return res
	}
	left := time.Until(expiresAt.Time) + time.Second
	if left < time.Second {
		left = time.Second
	}
	if res.RequeueAfter == 0 || left < res.RequeueAfter {
		res.RequeueAfter = left
	}
	return res
}
