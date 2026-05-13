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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
)

var _ = Describe("VirtualNodeState Controller", func() {
	const namespace = "default"

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
				ProviderLiqoClusterID: "liqo-provider-test",
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

	newLiqoVirtualNode := func(name, reservationID string, nodeStatus string) *unstructured.Unstructured {
		vn := &unstructured.Unstructured{}
		vn.SetGroupVersionKind(LiqoVirtualNodeGVK)
		vn.SetName(name)
		vn.SetNamespace(namespace)
		vn.SetLabels(map[string]string{ReservationLabel: reservationID})
		_ = unstructured.SetNestedField(vn.Object, map[string]interface{}{
			"clusterID": "liqo-provider-test",
		}, "spec")
		if nodeStatus != "" {
			conds := []interface{}{
				map[string]interface{}{"type": "Node", "status": nodeStatus},
			}
			_ = unstructured.SetNestedSlice(vn.Object, conds, "status", "conditions")
		}
		return vn
	}

	Context("when no Liqo VirtualNode exists yet", func() {
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

	Context("when a Liqo VirtualNode is Running and the v1.Node has allocatable", func() {
		const name, resv = "vns-running", "resv-running"
		const liqoVNName = "rs-resv-running"

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, newVNS(name, resv))).To(Succeed())
			Expect(k8sClient.Create(ctx, newLiqoVirtualNode(liqoVNName, resv, "Running"))).To(Succeed())
			// Patch status separately — envtest enforces the status subresource.
			vn := newLiqoVirtualNode(liqoVNName, resv, "Running")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: liqoVNName, Namespace: namespace}, vn)).To(Succeed())
			_ = unstructured.SetNestedSlice(vn.Object, []interface{}{
				map[string]interface{}{"type": "Node", "status": "Running"},
			}, "status", "conditions")
			Expect(k8sClient.Status().Update(ctx, vn)).To(Succeed())

			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: liqoVNName},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			node.Status.Allocatable = corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			}
			Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
		})
		AfterEach(func() {
			vns := &autoscalingv1alpha1.VirtualNodeState{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, vns))).To(Succeed())
			vn := &unstructured.Unstructured{}
			vn.SetGroupVersionKind(LiqoVirtualNodeGVK)
			vn.SetName(liqoVNName)
			vn.SetNamespace(namespace)
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, vn))).To(Succeed())
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: liqoVNName}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
		})

		It("projects Phase=Running, VirtualNodeName, and Allocatable", func() {
			reconcileOnce(name)

			got := &autoscalingv1alpha1.VirtualNodeState{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(autoscalingv1alpha1.VirtualNodeStatePhaseRunning))
			Expect(got.Status.VirtualNodeName).To(Equal(liqoVNName))
			Expect(got.Status.Allocatable).To(HaveKeyWithValue(corev1.ResourceCPU, resource.MustParse("4")))
			Expect(got.Status.Allocatable).To(HaveKeyWithValue(corev1.ResourceMemory, resource.MustParse("8Gi")))
			Expect(got.Status.Conditions).To(ContainElement(SatisfyAll(
				HaveField("Type", autoscalingv1alpha1.VirtualNodeStateConditionReady),
				HaveField("Status", metav1.ConditionTrue),
			)))
		})
	})

	Context("when a Liqo VirtualNode reports an unexpected Node condition", func() {
		const name, resv = "vns-fail", "resv-fail"
		const liqoVNName = "rs-resv-fail"

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, newVNS(name, resv))).To(Succeed())
			Expect(k8sClient.Create(ctx, newLiqoVirtualNode(liqoVNName, resv, "Bogus"))).To(Succeed())
			vn := newLiqoVirtualNode(liqoVNName, resv, "Bogus")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: liqoVNName, Namespace: namespace}, vn)).To(Succeed())
			_ = unstructured.SetNestedSlice(vn.Object, []interface{}{
				map[string]interface{}{"type": "Node", "status": "Bogus"},
			}, "status", "conditions")
			Expect(k8sClient.Status().Update(ctx, vn)).To(Succeed())
		})
		AfterEach(func() {
			vns := &autoscalingv1alpha1.VirtualNodeState{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, vns))).To(Succeed())
			vn := &unstructured.Unstructured{}
			vn.SetGroupVersionKind(LiqoVirtualNodeGVK)
			vn.SetName(liqoVNName)
			vn.SetNamespace(namespace)
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, vn))).To(Succeed())
		})

		It("flags Phase=Failed and a Failed condition", func() {
			reconcileOnce(name)

			got := &autoscalingv1alpha1.VirtualNodeState{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(autoscalingv1alpha1.VirtualNodeStatePhaseFailed))
			Expect(got.Status.Allocatable).To(BeEmpty())
			Expect(got.Status.Conditions).To(ContainElement(SatisfyAll(
				HaveField("Type", autoscalingv1alpha1.VirtualNodeStateConditionFailed),
				HaveField("Status", metav1.ConditionTrue),
			)))
		})
	})

	Context("when the VirtualNodeName was cached but the VirtualNode has been deleted", func() {
		const name, resv = "vns-gone", "resv-gone"
		const liqoVNName = "rs-resv-gone"

		BeforeEach(func() {
			vns := newVNS(name, resv)
			Expect(k8sClient.Create(ctx, vns)).To(Succeed())
			// Pre-populate the status pointer so the reconciler skips
			// label-discovery and goes straight to a missing Get.
			vns.Status.VirtualNodeName = liqoVNName
			Expect(k8sClient.Status().Update(ctx, vns)).To(Succeed())
		})
		AfterEach(func() {
			vns := &autoscalingv1alpha1.VirtualNodeState{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, vns))).To(Succeed())
		})

		It("transitions to Phase=Deleting", func() {
			reconcileOnce(name)

			got := &autoscalingv1alpha1.VirtualNodeState{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Status.Phase).To(Equal(autoscalingv1alpha1.VirtualNodeStatePhaseDeleting))
		})
	})

	Context("requestsForVirtualNode (Liqo VN watch map-func)", func() {
		const namespace = "default"

		It("enqueues a VNS by reservation label match", func() {
			Expect(k8sClient.Create(ctx, newVNS("vns-watch-1", "resv-watch-1"))).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &autoscalingv1alpha1.VirtualNodeState{
					ObjectMeta: metav1.ObjectMeta{Name: "vns-watch-1", Namespace: namespace}})
			})

			r := &VirtualNodeStateReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			vn := newLiqoVirtualNode("rs-resv-watch-1", "resv-watch-1", "")
			reqs := r.requestsForVirtualNode(ctx, vn)
			Expect(reqs).To(ConsistOf(reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "vns-watch-1", Namespace: namespace},
			}))
		})

		It("enqueues a VNS by cached Status.VirtualNodeName when no label is present", func() {
			vns := newVNS("vns-watch-2", "resv-watch-2")
			Expect(k8sClient.Create(ctx, vns)).To(Succeed())
			vns.Status.VirtualNodeName = "stamped-name"
			Expect(k8sClient.Status().Update(ctx, vns)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &autoscalingv1alpha1.VirtualNodeState{
					ObjectMeta: metav1.ObjectMeta{Name: "vns-watch-2", Namespace: namespace}})
			})

			r := &VirtualNodeStateReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			vn := &unstructured.Unstructured{}
			vn.SetGroupVersionKind(LiqoVirtualNodeGVK)
			vn.SetName("stamped-name")
			vn.SetNamespace(namespace)
			// No reservation label — name-match must rescue.
			reqs := r.requestsForVirtualNode(ctx, vn)
			Expect(reqs).To(ConsistOf(reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "vns-watch-2", Namespace: namespace},
			}))
		})

		It("returns no requests when neither route matches", func() {
			Expect(k8sClient.Create(ctx, newVNS("vns-watch-3", "resv-watch-3"))).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &autoscalingv1alpha1.VirtualNodeState{
					ObjectMeta: metav1.ObjectMeta{Name: "vns-watch-3", Namespace: namespace}})
			})

			r := &VirtualNodeStateReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			vn := newLiqoVirtualNode("rs-orphan", "resv-unrelated", "")
			Expect(r.requestsForVirtualNode(ctx, vn)).To(BeEmpty())
		})

		It("deduplicates when label and cached name both point at the same VNS", func() {
			vns := newVNS("vns-watch-4", "resv-watch-4")
			Expect(k8sClient.Create(ctx, vns)).To(Succeed())
			vns.Status.VirtualNodeName = "rs-resv-watch-4"
			Expect(k8sClient.Status().Update(ctx, vns)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, &autoscalingv1alpha1.VirtualNodeState{
					ObjectMeta: metav1.ObjectMeta{Name: "vns-watch-4", Namespace: namespace}})
			})

			r := &VirtualNodeStateReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			vn := newLiqoVirtualNode("rs-resv-watch-4", "resv-watch-4", "")
			reqs := r.requestsForVirtualNode(ctx, vn)
			Expect(reqs).To(HaveLen(1))
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
