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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
)

// DefaultVirtualNodeStateRequeue is the polling cadence the reconciler
// falls back on when no v1.Node event has fired. The watch wired in
// SetupWithManager makes the common case event-driven; this timer is a
// safety net for races where the watch hasn't yet seen the node (or
// where, on a dev cluster without Liqo, no virtual node will ever
// appear).
const DefaultVirtualNodeStateRequeue = 30 * time.Second

// VirtualNodeStateReconciler reconciles a VirtualNodeState by projecting
// the status of the Liqo-materialised consumer-side node onto it. See
// docs/design.md §5.1: the gRPC server consumes VirtualNodeState as the
// canonical view of consumer-side virtual nodes when answering Cluster
// Autoscaler.
//
// Correlation is by the cluster-scoped v1.Node Liqo creates for the
// peering, whose name equals the provider's Liqo cluster ID
// (Spec.ProviderLiqoClusterID). This is deliberately NOT the namespaced
// Liqo VirtualNode CR: in a real deployment Liqo puts that CR in a
// per-provider tenant namespace (liqo-tenant-<provider>) and does not
// carry our reservation label onto it, so discovering it by
// label-in-our-namespace fails. The v1.Node, by contrast, is
// cluster-scoped, deterministically named, and is exactly the object
// Cluster Autoscaler schedules onto — making it the right anchor.
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
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile maps the consumer-side v1.Node's state onto VirtualNodeState.
// Strategy:
//  1. The node name is Spec.ProviderLiqoClusterID (Liqo names the virtual
//     node after the provider's Liqo cluster ID).
//  2. If the node is not (yet) present, mark Phase=Creating.
//  3. Otherwise derive Phase from the node's Ready condition, and — only
//     when Ready — copy the node's allocatable so the gRPC server sees a
//     faithful view.
//  4. Refresh Ready/Failed Conditions and Status.Update when changed.
func (r *VirtualNodeStateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("virtualnodestate", req.NamespacedName)

	var vns autoscalingv1alpha1.VirtualNodeState
	if err := r.Get(ctx, req.NamespacedName, &vns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// CRs in the process of deletion are owned by the Consumer Agent's
	// Unpeer flow: it deletes the ResourceSlice, Liqo deletes the
	// virtual node, and we just stop projecting status until the CR
	// disappears.
	if vns.GetDeletionTimestamp() != nil {
		return ctrl.Result{}, nil
	}

	desired := vns.DeepCopy()
	desired.Status.ObservedGeneration = vns.Generation

	nodeName := vns.Spec.ProviderLiqoClusterID
	if nodeName == "" {
		// Misconfigured CR — nothing to correlate against.
		applyPhase(&desired.Status, autoscalingv1alpha1.VirtualNodeStatePhaseCreating,
			"VirtualNodeState missing providerLiqoClusterId")
		desired.Status.VirtualNodeName = ""
		desired.Status.ProviderID = ""
		desired.Status.Allocatable = nil
	} else if err := r.projectFromNode(ctx, desired, nodeName); err != nil {
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

// projectFromNode resolves the cluster-scoped v1.Node by name and mutates
// desired.Status to reflect what it found. Only unexpected I/O failures
// are returned; NotFound is handled inline so the reconciler keeps
// advancing the CR's status while Liqo is still bringing the node up.
func (r *VirtualNodeStateReconciler) projectFromNode(
	ctx context.Context,
	desired *autoscalingv1alpha1.VirtualNodeState,
	nodeName string,
) error {
	var node corev1.Node
	err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node)
	switch {
	case apierrors.IsNotFound(err):
		// Liqo hasn't materialised the node yet (or it has been torn
		// down). Either way there is nothing to schedule on.
		applyPhase(&desired.Status, autoscalingv1alpha1.VirtualNodeStatePhaseCreating,
			"waiting for Liqo to materialise node "+nodeName)
		desired.Status.VirtualNodeName = ""
		desired.Status.ProviderID = ""
		desired.Status.Allocatable = nil
		return nil
	case err != nil:
		return fmt.Errorf("get v1.Node %q: %w", nodeName, err)
	}

	desired.Status.VirtualNodeName = nodeName

	if node.DeletionTimestamp != nil {
		applyPhase(&desired.Status, autoscalingv1alpha1.VirtualNodeStatePhaseDeleting,
			"Liqo virtual node is terminating")
		desired.Status.ProviderID = ""
		desired.Status.Allocatable = nil
		return nil
	}

	if !nodeReady(&node) {
		applyPhase(&desired.Status, autoscalingv1alpha1.VirtualNodeStatePhaseCreating,
			"Liqo virtual node registered, not Ready yet")
		desired.Status.ProviderID = ""
		desired.Status.Allocatable = nil
		return nil
	}

	applyPhase(&desired.Status, autoscalingv1alpha1.VirtualNodeStatePhaseRunning,
		"Liqo virtual node is ready")
	// Capture the node's providerID so the gRPC server can report it as the
	// NodeGroupNodes Instance Id — CA matches instances to registered nodes
	// by providerID, not name (see VirtualNodeStateStatus.ProviderID).
	desired.Status.ProviderID = node.Spec.ProviderID
	desired.Status.Allocatable = copyResourceList(node.Status.Allocatable)
	return nil
}

// nodeReady reports whether the node's Ready condition is True.
func nodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// copyResourceList returns a defensive deep copy of a ResourceList.
func copyResourceList(in corev1.ResourceList) corev1.ResourceList {
	if len(in) == 0 {
		return nil
	}
	out := make(corev1.ResourceList, len(in))
	for k, v := range in {
		out[k] = v.DeepCopy()
	}
	return out
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
		a.ProviderID != b.ProviderID ||
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

// SetupWithManager wires the reconciler with a watch on v1.Nodes so that
// the common case — Liqo registers (or drops) the virtual node — re-
// reconciles the owning VirtualNodeState immediately, instead of waiting
// for the timer-based RequeueAfter to fire.
func (r *VirtualNodeStateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.VirtualNodeState{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.requestsForNode)).
		Named("autoscaling-virtualnodestate").
		Complete(r)
}

// requestsForNode is the map-func feeding the v1.Node watch. It enqueues
// every VirtualNodeState whose Spec.ProviderLiqoClusterID equals the
// node's name — i.e. the CR(s) that project from this node. Listed
// cluster-wide so the lookup is independent of which namespace the CRs
// live in.
func (r *VirtualNodeStateReconciler) requestsForNode(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	var list autoscalingv1alpha1.VirtualNodeStateList
	if err := r.List(ctx, &list); err != nil {
		logf.FromContext(ctx).Error(err, "list VirtualNodeStates from node watch",
			"node", obj.GetName())
		return nil
	}
	nodeName := obj.GetName()
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		v := &list.Items[i]
		if v.Spec.ProviderLiqoClusterID != nodeName {
			continue
		}
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: v.Name, Namespace: v.Namespace},
		})
	}
	return out
}
