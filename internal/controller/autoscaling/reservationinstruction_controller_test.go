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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// deleteAllInDefault is shared between the two instruction-reconciler test
// files; defined here so both compilation units have access to it.
func deleteAllInDefault() client.DeleteAllOfOption {
	return client.InNamespace("default")
}

var _ = Describe("ReservationInstruction Controller", func() {
	const (
		instructionName = "ri-test"
		reservationName = "resv-ri-test"
	)
	ctx := context.Background()
	riKey := types.NamespacedName{Name: instructionName, Namespace: "default"}
	resvKey := types.NamespacedName{Name: reservationName, Namespace: "default"}

	BeforeEach(func() {
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
		_ = k8sClient.DeleteAllOf(ctx, &autoscalingv1alpha1.ReservationInstruction{}, deleteAllInDefault())
		resv := &brokerv1alpha1.Reservation{}
		if err := k8sClient.Get(ctx, resvKey, resv); err == nil {
			Expect(k8sClient.Delete(ctx, resv)).To(Succeed())
		}
	})

	createInstruction := func(spec autoscalingv1alpha1.ReservationInstructionSpec) *autoscalingv1alpha1.ReservationInstruction {
		ri := &autoscalingv1alpha1.ReservationInstruction{
			ObjectMeta: metav1.ObjectMeta{Name: instructionName, Namespace: "default"},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, ri)).To(Succeed())
		return ri
	}

	It("stamps Status.IssuedAt on first reconcile", func() {
		createInstruction(autoscalingv1alpha1.ReservationInstructionSpec{
			ReservationID:   reservationName,
			Kind:            autoscalingv1alpha1.ReservationInstructionPeer,
			TargetClusterID: "consumer-test",
		})

		r := &ReservationInstructionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: riKey})
		Expect(err).NotTo(HaveOccurred())

		got := &autoscalingv1alpha1.ReservationInstruction{}
		Expect(k8sClient.Get(ctx, riKey, got)).To(Succeed())
		Expect(got.Status.IssuedAt).NotTo(BeNil())
	})

	It("garbage-collects Enforced instructions older than the TTL", func() {
		ri := createInstruction(autoscalingv1alpha1.ReservationInstructionSpec{
			ReservationID:   reservationName,
			Kind:            autoscalingv1alpha1.ReservationInstructionPeer,
			TargetClusterID: "consumer-test",
		})

		now := metav1.Now()
		old := metav1.NewTime(time.Now().Add(-time.Hour))
		ri.Status.Enforced = true
		ri.Status.IssuedAt = &now
		ri.Status.LastUpdateTime = &old
		Expect(k8sClient.Status().Update(ctx, ri)).To(Succeed())

		r := &ReservationInstructionReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			EnforcedTTL: time.Minute,
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: riKey})
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, riKey, &autoscalingv1alpha1.ReservationInstruction{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("fails the parent Reservation and deletes the instruction when ExpiresAt elapses unenforced", func() {
		past := metav1.NewTime(time.Now().Add(-time.Minute))
		ri := createInstruction(autoscalingv1alpha1.ReservationInstructionSpec{
			ReservationID:   reservationName,
			Kind:            autoscalingv1alpha1.ReservationInstructionPeer,
			TargetClusterID: "consumer-test",
			ExpiresAt:       &past,
		})

		now := metav1.Now()
		ri.Status.IssuedAt = &now
		Expect(k8sClient.Status().Update(ctx, ri)).To(Succeed())

		r := &ReservationInstructionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: riKey})
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, riKey, &autoscalingv1alpha1.ReservationInstruction{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		updated := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, resvKey, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(brokerv1alpha1.ReservationPhaseFailed))
	})
})
