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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

var _ = Describe("ProviderInstruction Controller", func() {
	const (
		instructionName = "pi-test"
		reservationName = "resv-pi-test"
	)
	ctx := context.Background()
	piKey := types.NamespacedName{Name: instructionName, Namespace: "default"}
	resvKey := types.NamespacedName{Name: reservationName, Namespace: "default"}

	BeforeEach(func() {
		// Parent Reservation in a non-terminal phase, used by the expiry
		// path to verify the reconciler fails the right CR.
		resv := &brokerv1alpha1.Reservation{}
		if err := k8sClient.Get(ctx, resvKey, resv); err != nil && apierrors.IsNotFound(err) {
			Expect(k8sClient.Create(ctx, &brokerv1alpha1.Reservation{
				ObjectMeta: metav1.ObjectMeta{Name: reservationName, Namespace: "default"},
				Spec: brokerv1alpha1.ReservationSpec{
					ConsumerClusterID:     "consumer-test",
					ConsumerLiqoClusterID: "liqo-consumer-test",
					ProviderClusterID:     "provider-test",
					ProviderLiqoClusterID: "liqo-provider-test",
					ChunkCount:            1,
					ChunkType:             brokerv1alpha1.ChunkTypeStandard,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("2"),
						corev1.ResourceMemory: resource.MustParse("4Gi"),
					},
				},
			})).To(Succeed())
		}
	})

	AfterEach(func() {
		_ = k8sClient.DeleteAllOf(ctx, &autoscalingv1alpha1.ProviderInstruction{}, deleteAllInDefault())
		resv := &brokerv1alpha1.Reservation{}
		if err := k8sClient.Get(ctx, resvKey, resv); err == nil {
			Expect(k8sClient.Delete(ctx, resv)).To(Succeed())
		}
	})

	createInstruction := func(spec autoscalingv1alpha1.ProviderInstructionSpec) *autoscalingv1alpha1.ProviderInstruction {
		pi := &autoscalingv1alpha1.ProviderInstruction{
			ObjectMeta: metav1.ObjectMeta{Name: instructionName, Namespace: "default"},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, pi)).To(Succeed())
		return pi
	}

	It("stamps Status.IssuedAt on first reconcile", func() {
		createInstruction(autoscalingv1alpha1.ProviderInstructionSpec{
			ReservationID:   reservationName,
			Kind:            autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig,
			TargetClusterID: "provider-test",
		})

		r := &ProviderInstructionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: piKey})
		Expect(err).NotTo(HaveOccurred())

		got := &autoscalingv1alpha1.ProviderInstruction{}
		Expect(k8sClient.Get(ctx, piKey, got)).To(Succeed())
		Expect(got.Status.IssuedAt).NotTo(BeNil())
	})

	It("garbage-collects Enforced instructions older than the TTL", func() {
		pi := createInstruction(autoscalingv1alpha1.ProviderInstructionSpec{
			ReservationID:   reservationName,
			Kind:            autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig,
			TargetClusterID: "provider-test",
		})

		// Mark Enforced and stamp LastUpdateTime well before the TTL window.
		now := metav1.Now()
		old := metav1.NewTime(time.Now().Add(-time.Hour))
		pi.Status.Enforced = true
		pi.Status.IssuedAt = &now
		pi.Status.LastUpdateTime = &old
		Expect(k8sClient.Status().Update(ctx, pi)).To(Succeed())

		r := &ProviderInstructionReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			EnforcedTTL: time.Minute, // any positive value < 1h
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: piKey})
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, piKey, &autoscalingv1alpha1.ProviderInstruction{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("fails the parent Reservation and deletes the instruction when ExpiresAt elapses unenforced", func() {
		past := metav1.NewTime(time.Now().Add(-time.Minute))
		pi := createInstruction(autoscalingv1alpha1.ProviderInstructionSpec{
			ReservationID:   reservationName,
			Kind:            autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig,
			TargetClusterID: "provider-test",
			ExpiresAt:       &past,
		})

		// Pre-stamp IssuedAt so the reconciler skips the stamping branch
		// and goes straight to the expiry one.
		now := metav1.Now()
		pi.Status.IssuedAt = &now
		Expect(k8sClient.Status().Update(ctx, pi)).To(Succeed())

		r := &ProviderInstructionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: piKey})
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, piKey, &autoscalingv1alpha1.ProviderInstruction{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		updated := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, resvKey, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(brokerv1alpha1.ReservationPhaseFailed))
	})
})
