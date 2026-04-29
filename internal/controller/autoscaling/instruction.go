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

// This file holds the bookkeeping logic shared by ProviderInstruction and
// ReservationInstruction reconcilers (step 5d). Both reconcilers do the
// same three things — the instruction kind only differs in which CR they
// touch and how they fail the parent Reservation:
//
//  1. Stamp Status.IssuedAt on first observation.
//  2. Garbage-collect Enforced instructions after DefaultEnforcedTTL.
//  3. Fail the parent Reservation when Spec.ExpiresAt has elapsed before
//     enforcement, then delete the (now unactionable) instruction CR.
//
// The TTL and expiry are best-effort cleanup: missing a tick just delays
// the eventual delete, never compromises correctness — once Enforced is
// true the API instruction handler already filters the CR out of the
// agent polling response.

package autoscaling

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// DefaultEnforcedTTL is the grace period after which an instruction whose
// Status.Enforced is true is removed from the cluster. Five minutes is
// long enough for downstream operators to inspect the result, short
// enough that we don't bloat etcd.
const DefaultEnforcedTTL = 5 * time.Minute

// instructionStatus is the minimal projection of either instruction CR's
// status that the shared bookkeeping logic needs. Both
// ProviderInstructionStatus and ReservationInstructionStatus already
// expose these fields verbatim.
type instructionStatus struct {
	Enforced       bool
	IssuedAt       *metav1.Time
	LastUpdateTime *metav1.Time
}

// failParentReservationOnExpiry flips the owning Reservation to Failed
// when an instruction's Spec.ExpiresAt has elapsed before enforcement.
// Terminal-phase Reservations are left untouched. A missing parent is
// treated as success — there is nothing to fail.
func failParentReservationOnExpiry(
	ctx context.Context, c client.Client, namespace, reservationID, instructionName string,
) error {
	if reservationID == "" {
		return nil
	}
	var resv brokerv1alpha1.Reservation
	if err := c.Get(ctx,
		types.NamespacedName{Name: reservationID, Namespace: namespace},
		&resv); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	switch resv.Status.Phase {
	case brokerv1alpha1.ReservationPhaseReleased,
		brokerv1alpha1.ReservationPhaseExpired,
		brokerv1alpha1.ReservationPhaseFailed:
		return nil
	}
	patched := resv.DeepCopy()
	patched.Status.Phase = brokerv1alpha1.ReservationPhaseFailed
	patched.Status.Message = fmt.Sprintf("instruction %q expired before enforcement", instructionName)
	patched.Status.ObservedGeneration = resv.Generation
	return c.Status().Update(ctx, patched)
}

// instructionWakeup returns the duration the reconciler should wait
// before its next visit. The earliest of (ExpiresAt, IssuedAt+TTL) wins
// so we converge on either expiry or enforcement bookkeeping at exactly
// the right time. Returns zero (== Requeue immediately) when the
// reconciler must act now.
func instructionWakeup(
	now time.Time, expiresAt *metav1.Time, status instructionStatus, ttl time.Duration,
) time.Duration {
	var wake time.Time
	if status.Enforced && status.LastUpdateTime != nil {
		wake = status.LastUpdateTime.Add(ttl)
	}
	if !status.Enforced && expiresAt != nil {
		if wake.IsZero() || expiresAt.Before(&metav1.Time{Time: wake}) {
			wake = expiresAt.Time
		}
	}
	if wake.IsZero() {
		// Without expiry or enforcement we have nothing to wait for; the
		// next event will come from a watch on the CR.
		return 0
	}
	if !wake.After(now) {
		return 0
	}
	return wake.Sub(now)
}
