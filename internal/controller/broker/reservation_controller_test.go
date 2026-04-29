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

package broker

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

var _ = Describe("Reservation Controller", func() {
	const resName = "test-resource"
	ctx := context.Background()
	key := types.NamespacedName{Name: resName, Namespace: "default"}

	BeforeEach(func() {
		By("creating a Reservation in Pending phase")
		existing := &brokerv1alpha1.Reservation{}
		if err := k8sClient.Get(ctx, key, existing); err != nil && apierrors.IsNotFound(err) {
			res := &brokerv1alpha1.Reservation{
				ObjectMeta: metav1.ObjectMeta{Name: resName, Namespace: "default"},
				Spec: brokerv1alpha1.ReservationSpec{
					ConsumerClusterID:     "consumer-test",
					ConsumerLiqoClusterID: "liqo-consumer-test",
					ProviderClusterID:     "provider-test",
					ProviderLiqoClusterID: "liqo-provider-test",
					ChunkCount:            2,
					ChunkType:             brokerv1alpha1.ChunkTypeStandard,
					Resources: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("2"),
						corev1.ResourceMemory: resource.MustParse("4Gi"),
					},
				},
			}
			Expect(k8sClient.Create(ctx, res)).To(Succeed())

			res.Status.Phase = brokerv1alpha1.ReservationPhasePending
			Expect(k8sClient.Status().Update(ctx, res)).To(Succeed())
		}
	})

	AfterEach(func() {
		By("cleaning up Reservation + emitted instructions")
		// Delete the parent first; OwnerReferences should garbage-collect
		// children, but envtest does not run the garbage collector, so we
		// remove them explicitly to keep specs isolated.
		_ = k8sClient.DeleteAllOf(ctx, &autoscalingv1alpha1.ProviderInstruction{},
			client.InNamespace("default"))
		_ = k8sClient.DeleteAllOf(ctx, &autoscalingv1alpha1.ReservationInstruction{},
			client.InNamespace("default"))

		res := &brokerv1alpha1.Reservation{}
		if err := k8sClient.Get(ctx, key, res); err == nil {
			Expect(k8sClient.Delete(ctx, res)).To(Succeed())
		}
	})

	reconcileOnce := func() {
		r := &ReservationReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}

	It("Pending → emits a GenerateKubeconfig ProviderInstruction and advances to GeneratingKubeconfig", func() {
		reconcileOnce()

		got := &autoscalingv1alpha1.ProviderInstruction{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "gk-" + resName, Namespace: "default"}, got)).To(Succeed())
		Expect(got.Spec.Kind).To(Equal(autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig))
		Expect(got.Spec.TargetClusterID).To(Equal("provider-test"))
		Expect(got.Spec.ConsumerClusterID).To(Equal("consumer-test"))

		updated := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(brokerv1alpha1.ReservationPhaseGeneratingKubeconfig))

		By("re-reconciling does not duplicate the instruction")
		reconcileOnce()
		list := &autoscalingv1alpha1.ProviderInstructionList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace("default"))).To(Succeed())
		Expect(list.Items).To(HaveLen(1))
	})

	It("KubeconfigReady → emits a Peer ReservationInstruction and advances to Peering", func() {
		setPhase(ctx, key, brokerv1alpha1.ReservationPhaseKubeconfigReady)
		reconcileOnce()

		got := &autoscalingv1alpha1.ReservationInstruction{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "peer-" + resName, Namespace: "default"}, got)).To(Succeed())
		Expect(got.Spec.Kind).To(Equal(autoscalingv1alpha1.ReservationInstructionPeer))
		Expect(got.Spec.TargetClusterID).To(Equal("consumer-test"))
		Expect(got.Spec.KubeconfigRef).To(Equal("kubeconfig-" + resName))

		updated := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(brokerv1alpha1.ReservationPhasePeering))
	})

	It("Unpeering → emits an Unpeer ReservationInstruction with LastChunk=true and leaves phase as Unpeering", func() {
		setPhase(ctx, key, brokerv1alpha1.ReservationPhaseUnpeering)
		reconcileOnce()

		got := &autoscalingv1alpha1.ReservationInstruction{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "unpeer-" + resName, Namespace: "default"}, got)).To(Succeed())
		Expect(got.Spec.Kind).To(Equal(autoscalingv1alpha1.ReservationInstructionUnpeer))
		Expect(got.Spec.LastChunk).To(BeTrue())

		updated := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(brokerv1alpha1.ReservationPhaseUnpeering))
	})

	It("flips a non-terminal Reservation past ExpiresAt to Expired without emitting any instruction", func() {
		expired := metav1.NewTime(time.Now().Add(-time.Minute))
		res := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, res)).To(Succeed())
		res.Status.Phase = brokerv1alpha1.ReservationPhasePending
		res.Status.ExpiresAt = &expired
		Expect(k8sClient.Status().Update(ctx, res)).To(Succeed())

		reconcileOnce()

		updated := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(brokerv1alpha1.ReservationPhaseExpired))

		// No ProviderInstruction was emitted by the expiry path.
		list := &autoscalingv1alpha1.ProviderInstructionList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace("default"))).To(Succeed())
		Expect(list.Items).To(BeEmpty())
	})
})

// setPhase patches the Reservation to a specific phase. Used by the spec
// runners to land the CR in the start state each transition expects.
func setPhase(ctx context.Context, key types.NamespacedName, phase brokerv1alpha1.ReservationPhase) {
	res := &brokerv1alpha1.Reservation{}
	Expect(k8sClient.Get(ctx, key, res)).To(Succeed())
	res.Status.Phase = phase
	Expect(k8sClient.Status().Update(ctx, res)).To(Succeed())
}
