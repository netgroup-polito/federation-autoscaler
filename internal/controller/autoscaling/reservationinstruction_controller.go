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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
)

// ReservationInstructionReconciler is the consumer-side counterpart to
// ProviderInstructionReconciler. The control flow and semantics match:
//
//  1. Stamp Status.IssuedAt on first observation.
//  2. Delete Enforced instructions after EnforcedTTL.
//  3. Fail the parent Reservation and delete the instruction when
//     Spec.ExpiresAt is reached before enforcement.
//
// See instruction.go for the shared helpers.
type ReservationInstructionReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// EnforcedTTL is how long an Enforced instruction survives before
	// being deleted. Zero means use DefaultEnforcedTTL.
	EnforcedTTL time.Duration

	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=autoscaling.federation-autoscaler.io,resources=reservationinstructions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.federation-autoscaler.io,resources=reservationinstructions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.federation-autoscaler.io,resources=reservationinstructions/finalizers,verbs=update
// +kubebuilder:rbac:groups=broker.federation-autoscaler.io,resources=reservations,verbs=get;list;watch
// +kubebuilder:rbac:groups=broker.federation-autoscaler.io,resources=reservations/status,verbs=get;update;patch

// Reconcile drives the housekeeping state machine documented on the type.
func (r *ReservationInstructionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("reservationInstruction", req.String())

	var ri autoscalingv1alpha1.ReservationInstruction
	if err := r.Get(ctx, req.NamespacedName, &ri); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if ri.Status.IssuedAt == nil {
		now := metav1.NewTime(r.now())
		patched := ri.DeepCopy()
		patched.Status.IssuedAt = &now
		patched.Status.ObservedGeneration = ri.Generation
		if err := r.Status().Update(ctx, patched); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if ri.Status.Enforced {
		if r.enforcedExpired(&ri) {
			log.Info("garbage-collecting enforced ReservationInstruction")
			return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, &ri))
		}
	} else if ri.Spec.ExpiresAt != nil && !ri.Spec.ExpiresAt.After(r.now()) {
		log.Info("ReservationInstruction expired without enforcement; failing parent Reservation",
			"reservationId", ri.Spec.ReservationID)
		if err := failParentReservationOnExpiry(ctx, r.Client, ri.Namespace,
			ri.Spec.ReservationID, ri.Name); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, &ri))
	}

	wake := instructionWakeup(r.now(), ri.Spec.ExpiresAt, instructionStatus{
		Enforced:       ri.Status.Enforced,
		IssuedAt:       ri.Status.IssuedAt,
		LastUpdateTime: ri.Status.LastUpdateTime,
	}, r.ttl())
	if wake == 0 {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: wake}, nil
}

func (r *ReservationInstructionReconciler) enforcedExpired(ri *autoscalingv1alpha1.ReservationInstruction) bool {
	if ri.Status.LastUpdateTime == nil {
		return false
	}
	return r.now().Sub(ri.Status.LastUpdateTime.Time) >= r.ttl()
}

func (r *ReservationInstructionReconciler) ttl() time.Duration {
	if r.EnforcedTTL > 0 {
		return r.EnforcedTTL
	}
	return DefaultEnforcedTTL
}

func (r *ReservationInstructionReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager sets up the controller with the Manager.
func (r *ReservationInstructionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.ReservationInstruction{}).
		Named("autoscaling-reservationinstruction").
		Complete(r)
}
