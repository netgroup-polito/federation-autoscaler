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

// ProviderInstructionReconciler keeps ProviderInstruction CRs tidy:
//   - stamps Status.IssuedAt the first time it sees a brand new instruction;
//   - garbage-collects Enforced instructions after EnforcedTTL has elapsed;
//   - fails the parent Reservation and deletes the instruction when
//     Spec.ExpiresAt is reached before enforcement.
//
// The reconciler never speaks to a provider agent directly — agents pull
// instructions through the Broker REST API. This is purely housekeeping.
type ProviderInstructionReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// EnforcedTTL is how long an Enforced instruction survives before
	// being deleted. Zero means use DefaultEnforcedTTL.
	EnforcedTTL time.Duration

	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=autoscaling.federation-autoscaler.io,resources=providerinstructions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.federation-autoscaler.io,resources=providerinstructions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.federation-autoscaler.io,resources=providerinstructions/finalizers,verbs=update
// +kubebuilder:rbac:groups=broker.federation-autoscaler.io,resources=reservations,verbs=get;list;watch
// +kubebuilder:rbac:groups=broker.federation-autoscaler.io,resources=reservations/status,verbs=get;update;patch

// Reconcile drives the housekeeping state machine documented on the type.
func (r *ProviderInstructionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("providerInstruction", req.String())

	var pi autoscalingv1alpha1.ProviderInstruction
	if err := r.Get(ctx, req.NamespacedName, &pi); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 1. Stamp IssuedAt on first observation. Re-queue immediately so the
	//    next pass sees the freshly-stamped status.
	if pi.Status.IssuedAt == nil {
		now := metav1.NewTime(r.now())
		patched := pi.DeepCopy()
		patched.Status.IssuedAt = &now
		patched.Status.ObservedGeneration = pi.Generation
		if err := r.Status().Update(ctx, patched); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 2. Enforced TTL: delete after grace period.
	if pi.Status.Enforced {
		if r.enforcedExpired(&pi) {
			log.Info("garbage-collecting enforced ProviderInstruction")
			return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, &pi))
		}
	} else if pi.Spec.ExpiresAt != nil && !pi.Spec.ExpiresAt.After(r.now()) {
		// 3. Expiry: instruction was never enforced before its deadline.
		log.Info("ProviderInstruction expired without enforcement; failing parent Reservation",
			"reservationId", pi.Spec.ReservationID)
		if err := failParentReservationOnExpiry(ctx, r.Client, pi.Namespace,
			pi.Spec.ReservationID, pi.Name); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, &pi))
	}

	// 4. Schedule the next wake-up at whichever deadline (ExpiresAt or
	//    LastUpdateTime+TTL) is closer.
	wake := instructionWakeup(r.now(), pi.Spec.ExpiresAt, instructionStatus{
		Enforced:       pi.Status.Enforced,
		IssuedAt:       pi.Status.IssuedAt,
		LastUpdateTime: pi.Status.LastUpdateTime,
	}, r.ttl())
	if wake == 0 {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: wake}, nil
}

func (r *ProviderInstructionReconciler) enforcedExpired(pi *autoscalingv1alpha1.ProviderInstruction) bool {
	if pi.Status.LastUpdateTime == nil {
		return false
	}
	return r.now().Sub(pi.Status.LastUpdateTime.Time) >= r.ttl()
}

func (r *ProviderInstructionReconciler) ttl() time.Duration {
	if r.EnforcedTTL > 0 {
		return r.EnforcedTTL
	}
	return DefaultEnforcedTTL
}

func (r *ProviderInstructionReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager sets up the controller with the Manager.
func (r *ProviderInstructionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.ProviderInstruction{}).
		Named("autoscaling-providerinstruction").
		Complete(r)
}
