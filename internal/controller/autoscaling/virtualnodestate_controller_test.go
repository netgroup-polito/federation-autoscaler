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
)

var _ = Describe("VirtualNodeState Controller", func() {
	const namespace = "default"
	// The reconciler correlates by the cluster-scoped v1.Node named after
	// the provider's Liqo cluster ID — this is what newVNS sets and what
	// Liqo names the node in a real deployment.
	const providerLiqoID = "liqo-provider-test"

	ctx := context.Background()

	reconcileOnce := func(name string) {
		controllerReconciler := &VirtualNodeStateReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	newVNS := func(name, reservationID string) *autoscalingv1alpha1.VirtualNodeState {
		return &autoscalingv1alpha1.VirtualNodeState{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: autoscalingv1alpha1.VirtualNodeStateSpec{
				ProviderClusterID:     "provider-test",
				ProviderLiqoClusterID: providerLiqoID,
				NodeGroupID:           "provider-test-standard",
				ChunkIndex:            0,
				ReservationID:         reservationID,
				Resources: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
			},
		}
	}

	// newVirtualNode builds the cluster-scoped v1.Node Liqo would create
	// for the peering (named after the provider's Liqo cluster ID), with
	// the given Ready status and allocatable.
	newVirtualNode := func(name string, ready bool, alloc corev1.ResourceList) *corev1.Node {
		st := corev1.ConditionFalse
		if ready {
			st = corev1.ConditionTrue
		}
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: map[string]string{"liqo.io/type": "virtual-node"},
			},
			Status: corev1.NodeStatus{
				Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}},
				Allocatable: alloc,
			},
		}
	}

	Context("when no Liqo virtual node exists yet", func() {
		const name, resv = "vns-empty", "resv-empty"

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, newVNS(name, resv))).To(Succeed())
		})
		AfterEach(func() {
			vns := &autoscalingv1alpha1.VirtualNodeState{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, vns))).To(Succeed())
		})

		It("reports Phase=Creating and a NotReady condition", func() {
			reconcileOnce(name)

			got := &autoscalingv1alpha1.VirtualNodeState{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(autoscalingv1alpha1.VirtualNodeStatePhaseCreating))
			Expect(got.Status.VirtualNodeName).To(BeEmpty())
			Expect(got.Status.Allocatable).To(BeEmpty())
			Expect(got.Status.Conditions).To(ContainElement(SatisfyAll(
				HaveField("Type", autoscalingv1alpha1.VirtualNodeStateConditionReady),
				HaveField("Status", metav1.ConditionFalse),
			)))
		})
	})

	Context("when the Liqo v1.Node is Ready with allocatable", func() {
		const name, resv = "vns-running", "resv-running"

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, newVNS(name, resv))).To(Succeed())
			node := newVirtualNode(providerLiqoID, false, nil)
			// Liqo stamps a `liqo://…` providerID on the virtual node; CA
			// matches instances to registered nodes by this, not by name.
			node.Spec.ProviderID = "liqo://" + providerLiqoID
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			// Status is a subresource — set it separately.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: providerLiqoID}, node)).To(Succeed())
			node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
			node.Status.Allocatable = corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			}
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
		})
		AfterEach(func() {
			vns := &autoscalingv1alpha1.VirtualNodeState{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, vns))).To(Succeed())
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: providerLiqoID}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
		})

		It("projects Phase=Running, VirtualNodeName, ProviderID, and Allocatable", func() {
			reconcileOnce(name)

			got := &autoscalingv1alpha1.VirtualNodeState{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(autoscalingv1alpha1.VirtualNodeStatePhaseRunning))
			Expect(got.Status.VirtualNodeName).To(Equal(providerLiqoID))
			Expect(got.Status.ProviderID).To(Equal("liqo://" + providerLiqoID))
			Expect(got.Status.Allocatable).To(HaveKeyWithValue(corev1.ResourceCPU, resource.MustParse("4")))
			Expect(got.Status.Allocatable).To(HaveKeyWithValue(corev1.ResourceMemory, resource.MustParse("8Gi")))
			Expect(got.Status.Conditions).To(ContainElement(SatisfyAll(
				HaveField("Type", autoscalingv1alpha1.VirtualNodeStateConditionReady),
				HaveField("Status", metav1.ConditionTrue),
			)))
		})
	})

	Context("when the v1.Node exists but is not Ready", func() {
		const name, resv = "vns-notready", "resv-notready"

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, newVNS(name, resv))).To(Succeed())
			node := newVirtualNode(providerLiqoID, false, nil)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
		})
		AfterEach(func() {
			vns := &autoscalingv1alpha1.VirtualNodeState{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, vns))).To(Succeed())
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: providerLiqoID}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
		})

		It("reports Phase=Creating with the node name recorded and no allocatable", func() {
			reconcileOnce(name)

			got := &autoscalingv1alpha1.VirtualNodeState{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(autoscalingv1alpha1.VirtualNodeStatePhaseCreating))
			Expect(got.Status.VirtualNodeName).To(Equal(providerLiqoID))
			Expect(got.Status.ProviderID).To(BeEmpty())
			Expect(got.Status.Allocatable).To(BeEmpty())
		})
	})

	Context("requestsForNode (v1.Node watch map-func)", func() {
		It("enqueues a VNS whose ProviderLiqoClusterID matches the node name", func() {
			Expect(k8sClient.Create(ctx, newVNS("vns-watch-1", "resv-watch-1"))).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &autoscalingv1alpha1.VirtualNodeState{
					ObjectMeta: metav1.ObjectMeta{Name: "vns-watch-1", Namespace: namespace}})
			})

			r := &VirtualNodeStateReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			node := newVirtualNode(providerLiqoID, true, nil)
			reqs := r.requestsForNode(ctx, node)
			Expect(reqs).To(ContainElement(reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "vns-watch-1", Namespace: namespace},
			}))
		})

		It("returns no requests when no VNS targets that node", func() {
			Expect(k8sClient.Create(ctx, newVNS("vns-watch-2", "resv-watch-2"))).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &autoscalingv1alpha1.VirtualNodeState{
					ObjectMeta: metav1.ObjectMeta{Name: "vns-watch-2", Namespace: namespace}})
			})

			r := &VirtualNodeStateReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			node := newVirtualNode("some-unrelated-node", true, nil)
			Expect(r.requestsForNode(ctx, node)).NotTo(ContainElement(reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "vns-watch-2", Namespace: namespace},
			}))
		})
	})

	Context("when the VirtualNodeState is being deleted", func() {
		const name, resv = "vns-terminating", "resv-terminating"

		BeforeEach(func() {
			vns := newVNS(name, resv)
			// Add a finalizer so the apiserver keeps the object visible
			// after Delete — letting us observe Reconcile's no-op branch.
			vns.Finalizers = []string{"test.federation-autoscaler.io/keep"}
			Expect(k8sClient.Create(ctx, vns)).To(Succeed())
			Expect(k8sClient.Delete(ctx, vns)).To(Succeed())
		})
		AfterEach(func() {
			vns := &autoscalingv1alpha1.VirtualNodeState{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, vns)
			if apierrors.IsNotFound(err) {
				return
			}
			Expect(err).NotTo(HaveOccurred())
			vns.Finalizers = nil
			_ = k8sClient.Update(ctx, vns)
		})

		It("returns without touching status", func() {
			reconcileOnce(name)

			got := &autoscalingv1alpha1.VirtualNodeState{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Status.Phase).To(BeEmpty())
		})
	})
})
