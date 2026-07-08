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

	// Already reserved — keep the borrowed node(s) protected from CA scale-down.
	// Release is delete-driven; this branch only holds the capacity.
	if rr.Status.ReservationID != "" {
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
