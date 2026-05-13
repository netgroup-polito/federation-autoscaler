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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
)

// DefaultVirtualNodeStateRequeue is the polling cadence the reconciler
// falls back on when no Liqo VirtualNode event has fired. The watch
// wired in SetupWithManager makes the common case event-driven; this
// timer is a safety net for races where the watch hasn't yet seen the
// VirtualNode (or where, in dev clusters without Liqo, no VirtualNode
// will ever appear).
const DefaultVirtualNodeStateRequeue = 30 * time.Second

// ReservationLabel is the label key the Consumer Agent stamps on every
// Liqo CR it creates per reservation (ResourceSlice, NamespaceOffloading
// — see internal/agent/consumer/instructions/liqo.go). Liqo propagates
// this label to the VirtualNode it materialises, which gives the
// reconciler a robust way to discover the VirtualNode without depending
// on Liqo's internal naming conventions.
const ReservationLabel = "federation-autoscaler.io/reservation"

// LiqoVirtualNodeGVK is the GVK of the Liqo CR the reconciler observes.
// We talk to it via unstructured.Unstructured so federation-autoscaler
// doesn't have to vendor the entire liqo Go module (which would drag in
// apps/v1 and several internal helper packages just for this Status
// projection).
var LiqoVirtualNodeGVK = schema.GroupVersionKind{
	Group:   "offloading.liqo.io",
	Version: "v1beta1",
	Kind:    "VirtualNode",
}

// VirtualNodeStateReconciler reconciles a VirtualNodeState by projecting
// the corresponding Liqo VirtualNode's status onto it. See docs/design.md
// §5.1: the gRPC server consumes VirtualNodeState as the canonical view
// of consumer-side virtual nodes when answering Cluster Autoscaler.
type VirtualNodeStateReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// RequeueAfter overrides DefaultVirtualNodeStateRequeue (mainly a
	// test hook).
	RequeueAfter time.Duration
}

// +kubebuilder:rbac:groups=autoscaling.federation-autoscaler.io,resources=virtualnodestates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.federation-autoscaler.io,resources=virtualnodestates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.federation-autoscaler.io,resources=virtualnodestates/finalizers,verbs=update
// +kubebuilder:rbac:groups=offloading.liqo.io,resources=virtualnodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile maps Liqo state onto VirtualNodeState. Strategy:
//  1. Resolve the Liqo VirtualNode name (cached on Status, otherwise
//     discovered via the reservation label).
//  2. If the VirtualNode is not (yet) present, mark Phase=Creating.
//  3. Otherwise derive Phase from the VirtualNode's Node condition,
//     and — only when Phase=Running — copy the corresponding v1.Node's
//     allocatable to surface a faithful view to the gRPC server.
//  4. Refresh Ready/Failed Conditions and Status.Update when changed.
func (r *VirtualNodeStateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("virtualnodestate", req.NamespacedName)

	var vns autoscalingv1alpha1.VirtualNodeState
	if err := r.Get(ctx, req.NamespacedName, &vns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// CRs in the process of deletion are owned by the Consumer Agent's
	// Unpeer flow: it deletes the ResourceSlice, Liqo deletes the
	// VirtualNode, and we just stop projecting status until the CR
	// disappears.
	if vns.GetDeletionTimestamp() != nil {
		return ctrl.Result{}, nil
	}

	vnName, err := r.resolveVirtualNodeName(ctx, &vns)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve Liqo VirtualNode: %w", err)
	}

	desired := vns.DeepCopy()
	desired.Status.ObservedGeneration = vns.Generation

	if vnName == "" {
		// No VirtualNode discovered yet — Liqo has not honoured the
		// ResourceSlice (or the consumer agent hasn't created it).
		applyPhase(&desired.Status, autoscalingv1alpha1.VirtualNodeStatePhaseCreating,
			"waiting for Liqo VirtualNode")
		desired.Status.Allocatable = nil
	} else if err := r.projectFromVirtualNode(ctx, &vns, desired, vnName); err != nil {
		return ctrl.Result{}, err
	}

	updateConditions(&desired.Status)

	if !statusEqual(&vns.Status, &desired.Status) {
		if err := r.Status().Update(ctx, desired); err != nil {
			return ctrl.Result{}, fmt.Errorf("update VirtualNodeState status: %w", err)
		}
		log.V(1).Info("updated VirtualNodeState status",
			"phase", desired.Status.Phase,
			"virtualNode", desired.Status.VirtualNodeName)
	}

	return ctrl.Result{RequeueAfter: r.requeueAfter()}, nil
}

func (r *VirtualNodeStateReconciler) requeueAfter() time.Duration {
	if r.RequeueAfter > 0 {
		return r.RequeueAfter
	}
	return DefaultVirtualNodeStateRequeue
}

// projectFromVirtualNode resolves the Liqo VirtualNode by name, then
// mutates desired.Status to reflect what it found. The function only
// returns an error for unexpected I/O failures; NoKindMatch (Liqo CRD
// missing) and NotFound (VirtualNode deleted) are handled inline so
// the reconciler can keep advancing the CR's status.
func (r *VirtualNodeStateReconciler) projectFromVirtualNode(
	ctx context.Context,
	vns *autoscalingv1alpha1.VirtualNodeState,
	desired *autoscalingv1alpha1.VirtualNodeState,
	vnName string,
) error {
	vn, err := r.getVirtualNode(ctx, vns.Namespace, vnName)
	switch {
	case meta.IsNoMatchError(err):
		// Liqo CRD not installed (e.g. dev cluster without Liqo). Keep
		// Phase=Creating; the user must install Liqo before scaling —
		// at runtime the VirtualNode would never materialise.
		applyPhase(&desired.Status, autoscalingv1alpha1.VirtualNodeStatePhaseCreating,
			"Liqo VirtualNode CRD not installed")
		desired.Status.Allocatable = nil
		return nil
	case apierrors.IsNotFound(err):
		// Name was recorded on a previous reconcile but the VirtualNode
		// is gone — likely torn down mid-flight. Flip to Deleting so the
		// gRPC server stops scheduling on it.
		desired.Status.VirtualNodeName = vnName
		applyPhase(&desired.Status, autoscalingv1alpha1.VirtualNodeStatePhaseDeleting,
			"Liqo VirtualNode not found")
		desired.Status.Allocatable = nil
		return nil
	case err != nil:
		return fmt.Errorf("get Liqo VirtualNode %q: %w", vnName, err)
	}

	desired.Status.VirtualNodeName = vnName
	phase, message := derivePhase(vn)
	applyPhase(&desired.Status, phase, message)
	if phase != autoscalingv1alpha1.VirtualNodeStatePhaseRunning {
		desired.Status.Allocatable = nil
		return nil
	}
	alloc, err := r.fetchNodeAllocatable(ctx, vnName)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get v1.Node %q: %w", vnName, err)
	}
	desired.Status.Allocatable = alloc
	return nil
}

// resolveVirtualNodeName returns the Liqo VirtualNode name to project
// from. First preference: a name already cached in Status (set on a
// previous reconcile). Fallback: list VirtualNodes in the CR namespace
// carrying the same reservation label and take the first match. A
// missing Liqo CRD (NoKindMatchError) is treated as "no VirtualNode
// yet" — see the IsNoMatchError branch in Reconcile.
func (r *VirtualNodeStateReconciler) resolveVirtualNodeName(ctx context.Context, vns *autoscalingv1alpha1.VirtualNodeState) (string, error) {
	if vns.Status.VirtualNodeName != "" {
		return vns.Status.VirtualNodeName, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   LiqoVirtualNodeGVK.Group,
		Version: LiqoVirtualNodeGVK.Version,
		Kind:    LiqoVirtualNodeGVK.Kind + "List",
	})
	err := r.List(ctx, list,
		client.InNamespace(vns.Namespace),
		client.MatchingLabels{ReservationLabel: vns.Spec.ReservationID},
	)
	switch {
	case meta.IsNoMatchError(err):
		return "", nil
	case err != nil:
		return "", err
	}
	if len(list.Items) == 0 {
		return "", nil
	}
	return list.Items[0].GetName(), nil
}

// getVirtualNode fetches a single Liqo VirtualNode via the unstructured
// client. Errors are returned verbatim so the caller can branch on
// NoMatch (CRD absent) vs NotFound (deleted) vs other.
func (r *VirtualNodeStateReconciler) getVirtualNode(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	vn := &unstructured.Unstructured{}
	vn.SetGroupVersionKind(LiqoVirtualNodeGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, vn); err != nil {
		return nil, err
	}
	return vn, nil
}

// fetchNodeAllocatable returns a defensive copy of the v1.Node's
// Status.Allocatable. Liqo creates the v1.Node with the same name as
// the VirtualNode CR (CreateNode=true is the default), so a single Get
// is enough.
func (r *VirtualNodeStateReconciler) fetchNodeAllocatable(ctx context.Context, name string) (corev1.ResourceList, error) {
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		return nil, err
	}
	out := make(corev1.ResourceList, len(node.Status.Allocatable))
	for k, v := range node.Status.Allocatable {
		out[k] = v.DeepCopy()
	}
	return out, nil
}

// derivePhase maps a Liqo VirtualNode's Node-condition status onto our
// VirtualNodeStatePhase. The "Node" condition is authoritative; "None"
// or absent statuses map to Creating (Liqo emits a None placeholder
// before any kubelet has reported in).
func derivePhase(vn *unstructured.Unstructured) (autoscalingv1alpha1.VirtualNodeStatePhase, string) {
	if vn.GetDeletionTimestamp() != nil {
		return autoscalingv1alpha1.VirtualNodeStatePhaseDeleting, "Liqo VirtualNode is terminating"
	}
	conds, found, err := unstructured.NestedSlice(vn.Object, "status", "conditions")
	if err != nil || !found {
		return autoscalingv1alpha1.VirtualNodeStatePhaseCreating, "Liqo VirtualNode has no status yet"
	}
	var nodeStatus string
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(m, "type"); t == "Node" {
			nodeStatus, _, _ = unstructured.NestedString(m, "status")
			break
		}
	}
	switch nodeStatus {
	case "Running":
		return autoscalingv1alpha1.VirtualNodeStatePhaseRunning, "Liqo VirtualNode is ready"
	case "Creating", "None", "":
		return autoscalingv1alpha1.VirtualNodeStatePhaseCreating, "Liqo VirtualNode is being created"
	case "Draining", "Deleting":
		return autoscalingv1alpha1.VirtualNodeStatePhaseDeleting, "Liqo VirtualNode is shutting down"
	default:
		return autoscalingv1alpha1.VirtualNodeStatePhaseFailed,
			"unexpected Node condition status: " + nodeStatus
	}
}

// applyPhase sets Phase + Message and bumps LastTransitionTime only on
// an actual phase change. Callers must invoke updateConditions next so
// the derived Ready/Failed Conditions stay aligned.
func applyPhase(status *autoscalingv1alpha1.VirtualNodeStateStatus, phase autoscalingv1alpha1.VirtualNodeStatePhase, message string) {
	if status.Phase != phase {
		now := metav1.Now()
		status.LastTransitionTime = &now
	}
	status.Phase = phase
	status.Message = message
}

// updateConditions keeps the Ready / Failed conditions aligned with
// Phase. metav1's helper is intentionally avoided because we want the
// LastTransitionTime to advance only on a Status flip — the helper also
// advances it on Reason changes, which would churn the CR every tick.
func updateConditions(status *autoscalingv1alpha1.VirtualNodeStateStatus) {
	setCondition(&status.Conditions,
		autoscalingv1alpha1.VirtualNodeStateConditionReady,
		status.Phase == autoscalingv1alpha1.VirtualNodeStatePhaseRunning,
		readyReason(status.Phase), status.Message)
	setCondition(&status.Conditions,
		autoscalingv1alpha1.VirtualNodeStateConditionFailed,
		status.Phase == autoscalingv1alpha1.VirtualNodeStatePhaseFailed,
		failedReason(status.Phase), status.Message)
}

func readyReason(phase autoscalingv1alpha1.VirtualNodeStatePhase) string {
	if phase == autoscalingv1alpha1.VirtualNodeStatePhaseRunning {
		return "VirtualNodeReady"
	}
	return "VirtualNodeNotReady"
}

func failedReason(phase autoscalingv1alpha1.VirtualNodeStatePhase) string {
	if phase == autoscalingv1alpha1.VirtualNodeStatePhaseFailed {
		return "VirtualNodeFailed"
	}
	return "VirtualNodeNotFailed"
}

// setCondition is a minimal upsert that touches LastTransitionTime only
// when Status flips. Reason / Message updates without a Status change
// don't bump the timestamp, keeping the CR quiet on every tick.
func setCondition(conds *[]metav1.Condition, condType string, ok bool, reason, msg string) {
	cs := metav1.ConditionFalse
	if ok {
		cs = metav1.ConditionTrue
	}
	for i, c := range *conds {
		if c.Type != condType {
			continue
		}
		if c.Status != cs {
			(*conds)[i].LastTransitionTime = metav1.Now()
		}
		(*conds)[i].Status = cs
		(*conds)[i].Reason = reason
		(*conds)[i].Message = msg
		return
	}
	*conds = append(*conds, metav1.Condition{
		Type:               condType,
		Status:             cs,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	})
}

// statusEqual is a value-based comparator covering every field the
// reconciler may touch. Used to skip no-op Status().Update calls so the
// CR's resourceVersion (and any cached informer downstream) stays
// stable when nothing has changed.
func statusEqual(a, b *autoscalingv1alpha1.VirtualNodeStateStatus) bool {
	if a.Phase != b.Phase ||
		a.VirtualNodeName != b.VirtualNodeName ||
		a.ObservedGeneration != b.ObservedGeneration ||
		a.Message != b.Message {
		return false
	}
	if !resourceListEqual(a.Allocatable, b.Allocatable) {
		return false
	}
	return conditionsEqual(a.Conditions, b.Conditions)
}

func resourceListEqual(a, b corev1.ResourceList) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || va.Cmp(vb) != 0 {
			return false
		}
	}
	return true
}

func conditionsEqual(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	idx := make(map[string]metav1.Condition, len(a))
	for _, c := range a {
		idx[c.Type] = c
	}
	for _, c := range b {
		ac, ok := idx[c.Type]
		if !ok ||
			ac.Status != c.Status ||
			ac.Reason != c.Reason ||
			ac.Message != c.Message {
			return false
		}
	}
	return true
}

// SetupWithManager wires the reconciler with a watch on Liqo
// VirtualNodes so that the common case — Liqo emits an event when the
// VirtualNode's Node condition flips — re-reconciles the owning
// VirtualNodeState immediately, instead of waiting for the timer-based
// RequeueAfter to fire.
//
// The Liqo VirtualNode CRD must be installed on the cluster before the
// manager starts; otherwise the cache informer for the GVK will fail
// to list and the manager will not become ready. This matches our
// deployment expectation: on a consumer cluster, Liqo is always
// installed alongside federation-autoscaler.
func (r *VirtualNodeStateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	vn := &unstructured.Unstructured{}
	vn.SetGroupVersionKind(LiqoVirtualNodeGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.VirtualNodeState{}).
		Watches(vn, handler.EnqueueRequestsFromMapFunc(r.requestsForVirtualNode)).
		Named("autoscaling-virtualnodestate").
		Complete(r)
}

// requestsForVirtualNode is the map-func feeding the Liqo VirtualNode
// watch. It returns the reconcile.Request for the owning
// VirtualNodeState — discovered through two routes so we don't depend
// on Liqo propagating any one label or naming convention:
//
//  1. The reservation label (federation-autoscaler.io/reservation)
//     when Liqo propagated it from the ResourceSlice the Consumer
//     Agent created.
//  2. Status.VirtualNodeName when a prior reconcile already cached the
//     VirtualNode name on the owning CR.
//
// If neither route matches, the timer-based requeue eventually finds
// the relationship the next time it fires (which keeps the watch
// purely an optimisation, never a correctness anchor).
func (r *VirtualNodeStateReconciler) requestsForVirtualNode(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	var list autoscalingv1alpha1.VirtualNodeStateList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "list VirtualNodeStates from VirtualNode watch",
			"virtualNode", obj.GetName())
		return nil
	}
	resID := obj.GetLabels()[ReservationLabel]
	vnName := obj.GetName()
	out := make([]reconcile.Request, 0, len(list.Items))
	seen := make(map[string]struct{}, len(list.Items))
	for i := range list.Items {
		v := &list.Items[i]
		match := (resID != "" && v.Spec.ReservationID == resID) ||
			(v.Status.VirtualNodeName != "" && v.Status.VirtualNodeName == vnName)
		if !match {
			continue
		}
		key := v.Namespace + "/" + v.Name
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: v.Name, Namespace: v.Namespace},
		})
	}
	return out
}
