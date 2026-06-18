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
	const (
		resName      = "test-resource"
		providerName = "provider-test"
	)
	ctx := context.Background()
	key := types.NamespacedName{Name: resName, Namespace: "default"}
	cadvKey := types.NamespacedName{Name: providerName, Namespace: "default"}

	BeforeEach(func() {
		By("creating a matching available ClusterAdvertisement so the provider-availability guard passes")
		existingCA := &brokerv1alpha1.ClusterAdvertisement{}
		if err := k8sClient.Get(ctx, cadvKey, existingCA); err != nil && apierrors.IsNotFound(err) {
			cadv := &brokerv1alpha1.ClusterAdvertisement{
				ObjectMeta: metav1.ObjectMeta{Name: providerName, Namespace: "default"},
				Spec: brokerv1alpha1.ClusterAdvertisementSpec{
					ClusterID:     providerName,
					LiqoClusterID: "liqo-provider-test",
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
			now := metav1.Now()
			cadv.Status.Available = true
			cadv.Status.LastSeen = &now
			Expect(k8sClient.Status().Update(ctx, cadv)).To(Succeed())
		}

		By("creating a Reservation in Pending phase")
		existing := &brokerv1alpha1.Reservation{}
		if err := k8sClient.Get(ctx, key, existing); err != nil && apierrors.IsNotFound(err) {
			res := &brokerv1alpha1.Reservation{
				ObjectMeta: metav1.ObjectMeta{Name: resName, Namespace: "default"},
				Spec: brokerv1alpha1.ReservationSpec{
					ConsumerClusterID:     "consumer-test",
					ConsumerLiqoClusterID: "liqo-consumer-test",
					ProviderClusterID:     providerName,
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
		By("cleaning up Reservation + emitted instructions + ClusterAdvertisement")
		// Delete every Reservation in the namespace (some specs create
		// sibling reservations for bug #5). ReservationFinalizer holds
		// each object until a reconcile credits chunks back and removes
		// it; envtest has no controller running, so drive the reconcile
		// here until all are gone. Keeps specs isolated.
		var resvs brokerv1alpha1.ReservationList
		_ = k8sClient.List(ctx, &resvs, client.InNamespace("default"))
		for i := range resvs.Items {
			_ = k8sClient.Delete(ctx, &resvs.Items[i])
		}
		Eventually(func() bool {
			rr := &ReservationReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			var remaining brokerv1alpha1.ReservationList
			if err := k8sClient.List(ctx, &remaining, client.InNamespace("default")); err != nil {
				return false
			}
			for i := range remaining.Items {
				_, _ = rr.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name: remaining.Items[i].Name, Namespace: "default",
					},
				})
			}
			return len(remaining.Items) == 0
		}).Should(BeTrue())

		// Delete emitted instructions AFTER the reservation drain — the
		// deletion path itself emits an un-owned consumer Cleanup
		// instruction, so clearing instructions first would leave that one
		// behind to pollute the next spec. envtest runs no GC, so we
		// remove them explicitly here.
		_ = k8sClient.DeleteAllOf(ctx, &autoscalingv1alpha1.ProviderInstruction{},
			client.InNamespace("default"))
		_ = k8sClient.DeleteAllOf(ctx, &autoscalingv1alpha1.ReservationInstruction{},
			client.InNamespace("default"))

		// Staging kubeconfig Secrets are shared (consumer, provider) and
		// not owned by any Reservation; remove them explicitly.
		_ = k8sClient.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace("default"))

		cadv := &brokerv1alpha1.ClusterAdvertisement{}
		if err := k8sClient.Get(ctx, cadvKey, cadv); err == nil {
			Expect(k8sClient.Delete(ctx, cadv)).To(Succeed())
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
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: providerInstructionGKName("consumer-test", providerName), Namespace: "default",
		}, got)).To(Succeed())
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
		Expect(got.Spec.KubeconfigRef).To(Equal(kubeconfigSecretName("consumer-test", providerName)))

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

	It("fails a Pending Reservation when its ClusterAdvertisement is missing (no Cleanup needed)", func() {
		By("removing the advertisement before reconcile")
		cadv := &brokerv1alpha1.ClusterAdvertisement{}
		Expect(k8sClient.Get(ctx, cadvKey, cadv)).To(Succeed())
		Expect(k8sClient.Delete(ctx, cadv)).To(Succeed())

		reconcileOnce()

		updated := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(brokerv1alpha1.ReservationPhaseFailed))
		Expect(updated.Status.Message).To(ContainSubstring("no longer exists"))

		// No instructions of any kind: Pending hasn't peered, no Cleanup needed.
		piList := &autoscalingv1alpha1.ProviderInstructionList{}
		Expect(k8sClient.List(ctx, piList, client.InNamespace("default"))).To(Succeed())
		Expect(piList.Items).To(BeEmpty())
		riList := &autoscalingv1alpha1.ReservationInstructionList{}
		Expect(k8sClient.List(ctx, riList, client.InNamespace("default"))).To(Succeed())
		Expect(riList.Items).To(BeEmpty())
	})

	It("fails a Peered Reservation and queues a Cleanup instruction when its provider goes unavailable", func() {
		By("flipping the advertisement to Available=false")
		cadv := &brokerv1alpha1.ClusterAdvertisement{}
		Expect(k8sClient.Get(ctx, cadvKey, cadv)).To(Succeed())
		cadv.Status.Available = false
		Expect(k8sClient.Status().Update(ctx, cadv)).To(Succeed())

		setPhase(ctx, key, brokerv1alpha1.ReservationPhasePeered)
		reconcileOnce()

		updated := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(brokerv1alpha1.ReservationPhaseFailed))
		Expect(updated.Status.Message).To(ContainSubstring("no longer available"))

		got := &autoscalingv1alpha1.ReservationInstruction{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "cleanup-" + resName, Namespace: "default",
		}, got)).To(Succeed())
		Expect(got.Spec.Kind).To(Equal(autoscalingv1alpha1.ReservationInstructionCleanup))
		Expect(got.Spec.TargetClusterID).To(Equal("consumer-test"))
		Expect(got.Spec.LastChunk).To(BeTrue())
	})

	It("does not interfere with a reservation already in Unpeering when the provider becomes unavailable", func() {
		By("flipping the advertisement to Available=false")
		cadv := &brokerv1alpha1.ClusterAdvertisement{}
		Expect(k8sClient.Get(ctx, cadvKey, cadv)).To(Succeed())
		cadv.Status.Available = false
		Expect(k8sClient.Status().Update(ctx, cadv)).To(Succeed())

		setPhase(ctx, key, brokerv1alpha1.ReservationPhaseUnpeering)
		reconcileOnce()

		updated := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		// Phase MUST stay Unpeering — the existing Unpeer instruction is in
		// flight and the instruction-result handler will move it to Released
		// or Failed once the consumer reports back.
		Expect(updated.Status.Phase).To(Equal(brokerv1alpha1.ReservationPhaseUnpeering))

		got := &autoscalingv1alpha1.ReservationInstruction{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "unpeer-" + resName, Namespace: "default",
		}, got)).To(Succeed())
		Expect(got.Spec.Kind).To(Equal(autoscalingv1alpha1.ReservationInstructionUnpeer))

		// And no Cleanup instruction should have been emitted alongside it.
		cleanupKey := types.NamespacedName{Name: "cleanup-" + resName, Namespace: "default"}
		Expect(apierrors.IsNotFound(
			k8sClient.Get(ctx, cleanupKey, &autoscalingv1alpha1.ReservationInstruction{}),
		)).To(BeTrue())
	})

	It("Released → emits a ProviderInstruction{Cleanup} and removes the staging Secret when the credential exists", func() {
		By("first running the Pending reconcile so the GenerateKubeconfig instruction exists")
		reconcileOnce()
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: providerInstructionGKName("consumer-test", providerName), Namespace: "default",
		}, &autoscalingv1alpha1.ProviderInstruction{})).To(Succeed())

		By("seeding the shared (consumer, provider) staging kubeconfig Secret")
		// In production the API GenerateKubeconfig-result handler creates
		// this; the controller test stands in for it. Its presence is the
		// signal handleTerminal uses to decide provider work happened.
		secretName := kubeconfigSecretName("consumer-test", providerName)
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"},
			Data:       map[string][]byte{"kubeconfig": []byte("dummy")},
		})).To(Succeed())

		setPhase(ctx, key, brokerv1alpha1.ReservationPhaseReleased)
		reconcileOnce()

		got := &autoscalingv1alpha1.ProviderInstruction{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: providerInstructionCleanupName("consumer-test", providerName), Namespace: "default",
		}, got)).To(Succeed())
		Expect(got.Spec.Kind).To(Equal(autoscalingv1alpha1.ProviderInstructionCleanup))
		Expect(got.Spec.TargetClusterID).To(Equal(providerName))
		Expect(got.Spec.ConsumerClusterID).To(Equal("consumer-test"))

		By("the staging Secret was deleted (last reservation for the pair)")
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
				Name: secretName, Namespace: "default",
			}, &corev1.Secret{}))
		}).Should(BeTrue())

		By("re-reconciling does not duplicate the cleanup instruction")
		reconcileOnce()
		list := &autoscalingv1alpha1.ProviderInstructionList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace("default"))).To(Succeed())
		// Two ProviderInstructions total: gk + pcleanup. No duplicates.
		Expect(list.Items).To(HaveLen(2))
	})

	It("stamps TerminatedAt on the first terminal reconcile and does not GC yet (finding #1)", func() {
		setPhase(ctx, key, brokerv1alpha1.ReservationPhaseReleased)
		reconcileOnce()

		got := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.TerminatedAt).NotTo(BeNil())
		// Default TTL is 15m, so a freshly-stamped reservation is NOT yet
		// collected — it must linger long enough for the cleanup
		// instructions to be processed.
		Expect(got.DeletionTimestamp.IsZero()).To(BeTrue())
	})

	It("garbage-collects a terminal reservation once it is older than the TTL (finding #1)", func() {
		By("marking the reservation terminal with a TerminatedAt well past the default TTL")
		res := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, res)).To(Succeed())
		res.Status.Phase = brokerv1alpha1.ReservationPhaseReleased
		stale := metav1.NewTime(time.Now().Add(-2 * DefaultTerminalReservationTTL))
		res.Status.TerminatedAt = &stale
		Expect(k8sClient.Status().Update(ctx, res)).To(Succeed())

		By("the reconciler issues a Delete (GC) and the finalizer is cleared so the CR disappears")
		Eventually(func() bool {
			r := &ReservationReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &brokerv1alpha1.Reservation{}))
		}).Should(BeTrue())
	})

	It("Expired (peered) → emits a consumer Cleanup so the ResourceSlice/VNS don't leak (bug #7)", func() {
		By("running Pending so GenerateKubeconfig exists, then seeding the staging Secret (provider work happened)")
		reconcileOnce()
		secretName := kubeconfigSecretName("consumer-test", providerName)
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"},
			Data:       map[string][]byte{"kubeconfig": []byte("dummy")},
		})).To(Succeed())

		By("the reservation reaches Expired from a peered state (TTL lapsed, NOT the Unpeering path)")
		setPhase(ctx, key, brokerv1alpha1.ReservationPhaseExpired)
		reconcileOnce()

		By("a consumer Cleanup instruction is emitted (un-owned) so the agent tears down its Liqo state")
		cleanup := &autoscalingv1alpha1.ReservationInstruction{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "cleanup-" + resName, Namespace: "default",
		}, cleanup)).To(Succeed())
		Expect(cleanup.Spec.Kind).To(Equal(autoscalingv1alpha1.ReservationInstructionCleanup))
		Expect(cleanup.Spec.TargetClusterID).To(Equal("consumer-test"))
		Expect(cleanup.OwnerReferences).To(BeEmpty())
	})

	It("does not emit provider Cleanup when the reservation never reached GeneratingKubeconfig", func() {
		By("flipping straight from Pending to Failed without ever reconciling Pending")
		setPhase(ctx, key, brokerv1alpha1.ReservationPhaseFailed)
		reconcileOnce()

		piList := &autoscalingv1alpha1.ProviderInstructionList{}
		Expect(k8sClient.List(ctx, piList, client.InNamespace("default"))).To(Succeed())
		Expect(piList.Items).To(BeEmpty())
		// No consumer Cleanup either — never peered, so nothing to tear down.
		riList := &autoscalingv1alpha1.ReservationInstructionList{}
		Expect(k8sClient.List(ctx, riList, client.InNamespace("default"))).To(Succeed())
		Expect(riList.Items).To(BeEmpty())
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

	It("backfills a nil ExpiresAt from creationTimestamp + timeout so a reservation always has a deadline (lost create-time stamp)", func() {
		// Precondition: the BeforeEach created the reservation without an
		// ExpiresAt — exactly the state left behind when the API handler's
		// best-effort status stamp loses its race with the finalizer write.
		before := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, before)).To(Succeed())
		Expect(before.Status.ExpiresAt).To(BeNil())

		By("reconciling with an explicit, non-default 3h timeout")
		r := &ReservationReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), ReservationTimeout: 3 * time.Hour,
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		updated := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.CreatedAt).NotTo(BeNil())
		Expect(updated.Status.ExpiresAt).NotTo(BeNil())

		// ExpiresAt == creationTimestamp + the CONFIGURED timeout (proves the
		// 3h value is used, not the 24h default), so it can actually expire.
		want := updated.CreationTimestamp.Add(3 * time.Hour)
		Expect(updated.Status.ExpiresAt.Time).To(BeTemporally("~", want, 2*time.Second))

		By("a second reconcile leaves the stamped deadline unchanged (idempotent)")
		stamped := *updated.Status.ExpiresAt
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		again := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, again)).To(Succeed())
		Expect(again.Status.ExpiresAt.Time).To(BeTemporally("==", stamped.Time))
	})

	It("hard delete (kubectl delete reservation) credits reserved chunks back via the finalizer", func() {
		By("seeding the provider's ReservedChunks budget")
		cadv := &brokerv1alpha1.ClusterAdvertisement{}
		Expect(k8sClient.Get(ctx, cadvKey, cadv)).To(Succeed())
		cadv.Status.TotalChunks = 5
		cadv.Status.ReservedChunks = 5
		cadv.Status.AvailableChunks = 0
		Expect(k8sClient.Status().Update(ctx, cadv)).To(Succeed())

		By("reconciling once so the finalizer is attached")
		reconcileOnce()
		res := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, res)).To(Succeed())
		Expect(res.Finalizers).To(ContainElement(ReservationFinalizer))

		By("hard-deleting the Reservation, then reconciling the deletion")
		Expect(k8sClient.Delete(ctx, res)).To(Succeed())
		reconcileOnce()

		By("the Reservation is gone (finalizer removed)")
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &brokerv1alpha1.Reservation{}))
		}).Should(BeTrue())

		By("ReservedChunks was credited back by resv.Spec.ChunkCount (5 - 2 = 3)")
		updatedCA := &brokerv1alpha1.ClusterAdvertisement{}
		Expect(k8sClient.Get(ctx, cadvKey, updatedCA)).To(Succeed())
		Expect(updatedCA.Status.ReservedChunks).To(Equal(int32(3)))
		Expect(updatedCA.Status.AvailableChunks).To(Equal(int32(2)))

		By("a consumer Cleanup instruction was emitted (un-owned) so the agent tears down its Liqo state")
		cleanup := &autoscalingv1alpha1.ReservationInstruction{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "cleanup-" + resName, Namespace: "default",
		}, cleanup)).To(Succeed())
		Expect(cleanup.Spec.Kind).To(Equal(autoscalingv1alpha1.ReservationInstructionCleanup))
		Expect(cleanup.Spec.TargetClusterID).To(Equal("consumer-test"))
		// Un-owned so it survives the Reservation's deletion.
		Expect(cleanup.OwnerReferences).To(BeEmpty())
	})

	It("hard delete does not double-release when chunks were already released", func() {
		By("seeding ReservedChunks and attaching the finalizer")
		cadv := &brokerv1alpha1.ClusterAdvertisement{}
		Expect(k8sClient.Get(ctx, cadvKey, cadv)).To(Succeed())
		cadv.Status.TotalChunks = 5
		cadv.Status.ReservedChunks = 5
		cadv.Status.AvailableChunks = 0
		Expect(k8sClient.Status().Update(ctx, cadv)).To(Succeed())

		reconcileOnce()
		res := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, res)).To(Succeed())

		By("stamping ChunksReleased before delete (simulates the normal API release path)")
		brokerv1alpha1.MarkChunksReleased(res)
		Expect(k8sClient.Update(ctx, res)).To(Succeed())

		Expect(k8sClient.Delete(ctx, res)).To(Succeed())
		reconcileOnce()

		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &brokerv1alpha1.Reservation{}))
		}).Should(BeTrue())

		By("ReservedChunks is unchanged — no second decrement")
		updatedCA := &brokerv1alpha1.ClusterAdvertisement{}
		Expect(k8sClient.Get(ctx, cadvKey, updatedCA)).To(Succeed())
		Expect(updatedCA.Status.ReservedChunks).To(Equal(int32(5)))
	})

	// ---------------------------------------------------------------------
	// bug #5 — (consumer, provider)-scoped GenerateKubeconfig de-duplication
	// ---------------------------------------------------------------------

	It("Pending fast-paths to KubeconfigReady when the shared kubeconfig Secret already exists", func() {
		By("seeding the shared (consumer, provider) staging Secret as if a sibling already generated it")
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: kubeconfigSecretName("consumer-test", providerName), Namespace: "default",
			},
			Data: map[string][]byte{"kubeconfig": []byte("dummy")},
		})).To(Succeed())

		reconcileOnce()

		By("no GenerateKubeconfig instruction was emitted")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
			Name: providerInstructionGKName("consumer-test", providerName), Namespace: "default",
		}, &autoscalingv1alpha1.ProviderInstruction{}))).To(BeTrue())

		By("the Reservation jumped straight to KubeconfigReady")
		updated := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(brokerv1alpha1.ReservationPhaseKubeconfigReady))
	})

	It("two concurrent Pendings to the same provider issue exactly one GenerateKubeconfig", func() {
		By("creating a sibling Reservation for the same (consumer, provider)")
		sibling := newReservation("test-resource-2", "consumer-test", providerName)
		Expect(k8sClient.Create(ctx, sibling)).To(Succeed())
		setPhase(ctx, types.NamespacedName{Name: "test-resource-2", Namespace: "default"},
			brokerv1alpha1.ReservationPhasePending)

		By("reconciling both")
		reconcileOnce() // test-resource
		rr := &ReservationReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := rr.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: "test-resource-2", Namespace: "default",
		}})
		Expect(err).NotTo(HaveOccurred())

		By("exactly one shared GenerateKubeconfig instruction exists")
		list := &autoscalingv1alpha1.ProviderInstructionList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace("default"))).To(Succeed())
		gkCount := 0
		for i := range list.Items {
			if list.Items[i].Spec.Kind == autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig {
				gkCount++
				Expect(list.Items[i].Name).To(Equal(providerInstructionGKName("consumer-test", providerName)))
			}
		}
		Expect(gkCount).To(Equal(1))
	})

	It("defers provider Cleanup until the last reservation for a (consumer, provider) terminates", func() {
		By("seeding the shared staging Secret and a sibling reservation")
		secretName := kubeconfigSecretName("consumer-test", providerName)
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"},
			Data:       map[string][]byte{"kubeconfig": []byte("dummy")},
		})).To(Succeed())
		sibling := newReservation("test-resource-2", "consumer-test", providerName)
		Expect(k8sClient.Create(ctx, sibling)).To(Succeed())
		setPhase(ctx, types.NamespacedName{Name: "test-resource-2", Namespace: "default"},
			brokerv1alpha1.ReservationPhasePeered)

		By("releasing the first reservation while the sibling is still active")
		setPhase(ctx, key, brokerv1alpha1.ReservationPhaseReleased)
		reconcileOnce()

		By("no provider Cleanup yet, Secret still present")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
			Name: providerInstructionCleanupName("consumer-test", providerName), Namespace: "default",
		}, &autoscalingv1alpha1.ProviderInstruction{}))).To(BeTrue())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: "default"},
			&corev1.Secret{})).To(Succeed())

		By("releasing the sibling — now the last one")
		setPhase(ctx, types.NamespacedName{Name: "test-resource-2", Namespace: "default"},
			brokerv1alpha1.ReservationPhaseReleased)
		_, err := (&ReservationReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}).Reconcile(
			ctx, reconcile.Request{NamespacedName: types.NamespacedName{
				Name: "test-resource-2", Namespace: "default",
			}})
		Expect(err).NotTo(HaveOccurred())

		By("provider Cleanup is emitted and the Secret is removed")
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: providerInstructionCleanupName("consumer-test", providerName), Namespace: "default",
		}, &autoscalingv1alpha1.ProviderInstruction{})).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
				Name: secretName, Namespace: "default",
			}, &corev1.Secret{}))
		}).Should(BeTrue())
	})

	// A release that follows a prior (consumer, provider) cycle must RE-RUN the
	// provider Cleanup even though a same-named, already-Enforced instruction
	// from that prior cycle still lingers (before the DefaultEnforcedTTL GC
	// reclaims it). Without the re-arm, ensureSharedInstruction no-ops on
	// AlreadyExists, the peering-user is never deleted, and the next
	// GenerateKubeconfig fails with "CSR already exists".
	It("re-arms a stale, already-enforced provider Cleanup on release instead of suppressing it", func() {
		cleanupName := providerInstructionCleanupName("consumer-test", providerName)

		By("seeding the shared staging Secret — a peering-user exists for this (consumer, provider)")
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: kubeconfigSecretName("consumer-test", providerName), Namespace: "default",
			},
			Data: map[string][]byte{"kubeconfig": []byte("dummy")},
		})).To(Succeed())

		By("pre-creating an already-ENFORCED provider Cleanup from a previous cycle (TTL GC hasn't run)")
		stale := &autoscalingv1alpha1.ProviderInstruction{
			ObjectMeta: metav1.ObjectMeta{Name: cleanupName, Namespace: "default"},
			Spec: autoscalingv1alpha1.ProviderInstructionSpec{
				ReservationID:     "old-reservation",
				Kind:              autoscalingv1alpha1.ProviderInstructionCleanup,
				TargetClusterID:   providerName,
				ConsumerClusterID: "consumer-test",
			},
		}
		Expect(k8sClient.Create(ctx, stale)).To(Succeed())
		now := metav1.Now()
		stale.Status.Enforced = true
		stale.Status.LastUpdateTime = &now
		Expect(k8sClient.Status().Update(ctx, stale)).To(Succeed())

		By("releasing the (last) reservation for this pair")
		setPhase(ctx, key, brokerv1alpha1.ReservationPhaseReleased)
		reconcileOnce()

		By("the shared Cleanup was re-armed: Enforced reset and re-bound to this reservation")
		got := &autoscalingv1alpha1.ProviderInstruction{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: cleanupName, Namespace: "default"}, got)).To(Succeed())
		Expect(got.Status.Enforced).To(BeFalse())
		Expect(got.Spec.ReservationID).To(Equal(resName))

		By("the staging Secret was removed (broker no longer believes a peering-user exists)")
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
				Name: kubeconfigSecretName("consumer-test", providerName), Namespace: "default",
			}, &corev1.Secret{}))
		}).Should(BeTrue())
	})
})

// newReservation builds a Pending-ready Reservation for a (consumer,
// provider) pair. Status.Phase is set separately by the caller via
// setPhase (status is a subresource).
func newReservation(name, consumer, provider string) *brokerv1alpha1.Reservation {
	return &brokerv1alpha1.Reservation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: brokerv1alpha1.ReservationSpec{
			ConsumerClusterID:     consumer,
			ConsumerLiqoClusterID: "liqo-" + consumer,
			ProviderClusterID:     provider,
			ProviderLiqoClusterID: "liqo-" + provider,
			ChunkCount:            1,
			ChunkType:             brokerv1alpha1.ChunkTypeStandard,
			Resources: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
	}
}

// setPhase patches the Reservation to a specific phase. Used by the spec
// runners to land the CR in the start state each transition expects.
func setPhase(ctx context.Context, key types.NamespacedName, phase brokerv1alpha1.ReservationPhase) {
	res := &brokerv1alpha1.Reservation{}
	Expect(k8sClient.Get(ctx, key, res)).To(Succeed())
	res.Status.Phase = phase
	Expect(k8sClient.Status().Update(ctx, res)).To(Succeed())
}
