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

package integration

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

const (
	timeout  = 10 * time.Second
	interval = 200 * time.Millisecond
)

var _ = Describe("Reservation lifecycle: Pending → Peered → Released", func() {
	const (
		ns           = "default"
		resName      = "scaleup-resv"
		providerName = "provider-int"
	)
	resvKey := types.NamespacedName{Name: resName, Namespace: ns}
	// GenerateKubeconfig, the staging Secret, and provider Cleanup are
	// keyed by (consumer, provider) — shared across reservations — not by
	// reservation name (bug #5). Peer/Unpeer stay per-reservation.
	credKey := "consumer-int-provider-int"
	gkKey := types.NamespacedName{Name: "gk-" + credKey, Namespace: ns}
	kubeconfigSecretKey := types.NamespacedName{Name: "kubeconfig-" + credKey, Namespace: ns}
	peerKey := types.NamespacedName{Name: "peer-" + resName, Namespace: ns}
	unpeerKey := types.NamespacedName{Name: "unpeer-" + resName, Namespace: ns}
	pcleanupKey := types.NamespacedName{Name: "pcleanup-" + credKey, Namespace: ns}
	cadvKey := types.NamespacedName{Name: providerName, Namespace: ns}

	AfterEach(func() {
		// Best-effort cleanup. envtest does not run the garbage collector,
		// so we explicitly delete instruction CRs before the parent.
		// DeleteAllOf keeps the AfterEach robust to lifecycle additions.
		_ = k8sClient.DeleteAllOf(ctx, &autoscalingv1alpha1.ProviderInstruction{}, client.InNamespace(ns))
		_ = k8sClient.DeleteAllOf(ctx, &autoscalingv1alpha1.ReservationInstruction{}, client.InNamespace(ns))
		_ = k8sClient.Delete(ctx, &brokerv1alpha1.Reservation{
			ObjectMeta: metav1.ObjectMeta{Name: resName, Namespace: ns},
		})
		_ = k8sClient.Delete(ctx, &brokerv1alpha1.ClusterAdvertisement{
			ObjectMeta: metav1.ObjectMeta{Name: providerName, Namespace: ns},
		})
	})

	It("walks the Reservation through every phase as the agents report results", func() {
		By("creating an available ClusterAdvertisement for the provider")
		cadv := &brokerv1alpha1.ClusterAdvertisement{
			ObjectMeta: metav1.ObjectMeta{Name: providerName, Namespace: ns},
			Spec: brokerv1alpha1.ClusterAdvertisementSpec{
				ClusterID:     providerName,
				LiqoClusterID: "liqo-provider-int",
				ClusterType:   brokerv1alpha1.ChunkTypeStandard,
				Resources: brokerv1alpha1.AdvertisedResources{
					Allocatable: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("8"),
						corev1.ResourceMemory: resource.MustParse("16Gi"),
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cadv)).To(Succeed())
		// Mark Available=true; otherwise the ClusterAdvertisement reconciler
		// (which is wired into the integration manager) will leave Available
		// as the default false until an HTTP advertisement lands, and the
		// Reservation provider-availability guard would fail the reservation.
		Eventually(func() error {
			c := &brokerv1alpha1.ClusterAdvertisement{}
			if err := k8sClient.Get(ctx, cadvKey, c); err != nil {
				return err
			}
			now := metav1.Now()
			c.Status.Available = true
			c.Status.LastSeen = &now
			return k8sClient.Status().Update(ctx, c)
		}, timeout, interval).Should(Succeed())

		By("creating the Reservation in Pending")
		resv := &brokerv1alpha1.Reservation{
			ObjectMeta: metav1.ObjectMeta{Name: resName, Namespace: ns},
			Spec: brokerv1alpha1.ReservationSpec{
				ConsumerClusterID:     "consumer-int",
				ConsumerLiqoClusterID: "liqo-consumer-int",
				ProviderClusterID:     "provider-int",
				ProviderLiqoClusterID: "liqo-provider-int",
				ChunkCount:            1,
				ChunkType:             brokerv1alpha1.ChunkTypeStandard,
				Resources: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
			},
		}
		Expect(k8sClient.Create(ctx, resv)).To(Succeed())
		updateStatus(resvKey, func(r *brokerv1alpha1.Reservation) {
			r.Status.Phase = brokerv1alpha1.ReservationPhasePending
		})

		By("waiting for the Reservation reconciler to emit GenerateKubeconfig and advance to GeneratingKubeconfig")
		Eventually(func(g Gomega) {
			pi := &autoscalingv1alpha1.ProviderInstruction{}
			g.Expect(k8sClient.Get(ctx, gkKey, pi)).To(Succeed())
			g.Expect(pi.Spec.Kind).To(Equal(autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig))
			g.Expect(pi.Spec.TargetClusterID).To(Equal("provider-int"))

			r := &brokerv1alpha1.Reservation{}
			g.Expect(k8sClient.Get(ctx, resvKey, r)).To(Succeed())
			g.Expect(r.Status.Phase).To(Equal(brokerv1alpha1.ReservationPhaseGeneratingKubeconfig))
		}, timeout, interval).Should(Succeed())

		By("simulating the provider agent reporting a kubeconfig success")
		simulateInstructionEnforced(gkKey, true)
		// The API GenerateKubeconfig-result handler stages the peering-user
		// kubeconfig in a (consumer, provider)-keyed Secret; stand in for it
		// here. handleTerminal later gates provider Cleanup on this Secret's
		// existence (it is the durable "provider work happened" marker).
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: kubeconfigSecretKey.Name, Namespace: ns},
			Data:       map[string][]byte{"kubeconfig": []byte("dummy")},
		})).To(Succeed())
		updateStatus(resvKey, func(r *brokerv1alpha1.Reservation) {
			r.Status.Phase = brokerv1alpha1.ReservationPhaseKubeconfigReady
			r.Status.Message = "kubeconfig delivered"
		})

		By("waiting for the Reservation reconciler to emit Peer and advance to Peering")
		Eventually(func(g Gomega) {
			ri := &autoscalingv1alpha1.ReservationInstruction{}
			g.Expect(k8sClient.Get(ctx, peerKey, ri)).To(Succeed())
			g.Expect(ri.Spec.Kind).To(Equal(autoscalingv1alpha1.ReservationInstructionPeer))
			g.Expect(ri.Spec.TargetClusterID).To(Equal("consumer-int"))
			g.Expect(ri.Spec.KubeconfigRef).To(Equal(kubeconfigSecretKey.Name))

			r := &brokerv1alpha1.Reservation{}
			g.Expect(k8sClient.Get(ctx, resvKey, r)).To(Succeed())
			g.Expect(r.Status.Phase).To(Equal(brokerv1alpha1.ReservationPhasePeering))
		}, timeout, interval).Should(Succeed())

		By("simulating the consumer agent reporting a peer success")
		simulateInstructionEnforced(peerKey, false)
		updateStatus(resvKey, func(r *brokerv1alpha1.Reservation) {
			r.Status.Phase = brokerv1alpha1.ReservationPhasePeered
			r.Status.Message = "peering completed"
			r.Status.VirtualNodeNames = []string{"liqo-provider-int"}
		})

		By("the Reservation has reached its terminal happy-path phase")
		Eventually(func(g Gomega) {
			r := &brokerv1alpha1.Reservation{}
			g.Expect(k8sClient.Get(ctx, resvKey, r)).To(Succeed())
			g.Expect(r.Status.Phase).To(Equal(brokerv1alpha1.ReservationPhasePeered))
			g.Expect(r.Status.VirtualNodeNames).To(ContainElement("liqo-provider-int"))
		}, timeout, interval).Should(Succeed())

		By("the autoscaling reconcilers have stamped IssuedAt on both instructions")
		Eventually(func(g Gomega) {
			pi := &autoscalingv1alpha1.ProviderInstruction{}
			g.Expect(k8sClient.Get(ctx, gkKey, pi)).To(Succeed())
			g.Expect(pi.Status.IssuedAt).NotTo(BeNil())

			ri := &autoscalingv1alpha1.ReservationInstruction{}
			g.Expect(k8sClient.Get(ctx, peerKey, ri)).To(Succeed())
			g.Expect(ri.Status.IssuedAt).NotTo(BeNil())
		}, timeout, interval).Should(Succeed())

		By("simulating a tear-down request — phase moves to Unpeering")
		updateStatus(resvKey, func(r *brokerv1alpha1.Reservation) {
			r.Status.Phase = brokerv1alpha1.ReservationPhaseUnpeering
			r.Status.Message = "tear-down requested"
		})

		By("waiting for the Reservation reconciler to emit Unpeer and stay in Unpeering")
		Eventually(func(g Gomega) {
			ri := &autoscalingv1alpha1.ReservationInstruction{}
			g.Expect(k8sClient.Get(ctx, unpeerKey, ri)).To(Succeed())
			g.Expect(ri.Spec.Kind).To(Equal(autoscalingv1alpha1.ReservationInstructionUnpeer))
			g.Expect(ri.Spec.LastChunk).To(BeTrue())

			r := &brokerv1alpha1.Reservation{}
			g.Expect(k8sClient.Get(ctx, resvKey, r)).To(Succeed())
			g.Expect(r.Status.Phase).To(Equal(brokerv1alpha1.ReservationPhaseUnpeering))
		}, timeout, interval).Should(Succeed())

		By("simulating the consumer agent reporting an unpeer success — phase moves to Released")
		simulateInstructionEnforced(unpeerKey, false)
		updateStatus(resvKey, func(r *brokerv1alpha1.Reservation) {
			r.Status.Phase = brokerv1alpha1.ReservationPhaseReleased
			r.Status.Message = "all chunks released"
		})

		By("the Reservation reconciler emits a ProviderInstruction{Cleanup} on Released")
		Eventually(func(g Gomega) {
			pi := &autoscalingv1alpha1.ProviderInstruction{}
			g.Expect(k8sClient.Get(ctx, pcleanupKey, pi)).To(Succeed())
			g.Expect(pi.Spec.Kind).To(Equal(autoscalingv1alpha1.ProviderInstructionCleanup))
			g.Expect(pi.Spec.TargetClusterID).To(Equal("provider-int"))
			g.Expect(pi.Spec.ConsumerClusterID).To(Equal("consumer-int"))
		}, timeout, interval).Should(Succeed())
	})
})

// updateStatus reads the Reservation, applies mutate, and writes status
// back. Retries on conflict to absorb the race against the reconcilers
// that are concurrently advancing phase.
func updateStatus(key types.NamespacedName, mutate func(*brokerv1alpha1.Reservation)) {
	Eventually(func() error {
		r := &brokerv1alpha1.Reservation{}
		if err := k8sClient.Get(ctx, key, r); err != nil {
			return err
		}
		mutate(r)
		return k8sClient.Status().Update(ctx, r)
	}, timeout, interval).Should(Succeed())
}

// simulateInstructionEnforced flips status.enforced=true with a fresh
// LastUpdateTime, mirroring exactly what internal/broker/api/instructions.go
// does on receipt of a successful POST /api/v1/instructions/{id}/result.
// The provider flag picks the instruction kind (true → ProviderInstruction,
// false → ReservationInstruction).
func simulateInstructionEnforced(key types.NamespacedName, provider bool) {
	Eventually(func() error {
		now := metav1.Now()
		if provider {
			pi := &autoscalingv1alpha1.ProviderInstruction{}
			if err := k8sClient.Get(ctx, key, pi); err != nil {
				if apierrors.IsNotFound(err) {
					return err
				}
				return err
			}
			pi.Status.Enforced = true
			pi.Status.Attempts++
			pi.Status.LastUpdateTime = &now
			pi.Status.Message = "Succeeded"
			return k8sClient.Status().Update(ctx, pi)
		}
		ri := &autoscalingv1alpha1.ReservationInstruction{}
		if err := k8sClient.Get(ctx, key, ri); err != nil {
			return err
		}
		ri.Status.Enforced = true
		ri.Status.Attempts++
		ri.Status.LastUpdateTime = &now
		ri.Status.Message = "Succeeded"
		return k8sClient.Status().Update(ctx, ri)
	}, timeout, interval).Should(Succeed())
}
