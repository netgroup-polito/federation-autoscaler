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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

var _ = Describe("ClusterAdvertisement Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"
		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating the custom resource for the Kind ClusterAdvertisement")
			existing := &brokerv1alpha1.ClusterAdvertisement{}
			if err := k8sClient.Get(ctx, typeNamespacedName, existing); err != nil && errors.IsNotFound(err) {
				res := &brokerv1alpha1.ClusterAdvertisement{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: brokerv1alpha1.ClusterAdvertisementSpec{
						ClusterID:     "provider-test",
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
				Expect(k8sClient.Create(ctx, res)).To(Succeed())
			}
		})

		AfterEach(func() {
			res := &brokerv1alpha1.ClusterAdvertisement{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, res)).To(Succeed())
			By("Cleanup the specific resource instance ClusterAdvertisement")
			Expect(k8sClient.Delete(ctx, res)).To(Succeed())
		})

		// reconcileOnce constructs the reconciler with a 1-second freshness
		// window and runs a single Reconcile pass against the fixture.
		reconcileOnce := func() reconcile.Result {
			r := &ClusterAdvertisementReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				StaleAfter: time.Second,
			}
			res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			return res
		}

		It("flips Available=false when LastSeen is missing", func() {
			reconcileOnce()

			updated := &brokerv1alpha1.ClusterAdvertisement{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Available).To(BeFalse())
		})

		It("keeps Available=true when LastSeen is fresh", func() {
			res := &brokerv1alpha1.ClusterAdvertisement{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, res)).To(Succeed())
			now := metav1.Now()
			res.Status.LastSeen = &now
			res.Status.Available = true
			res.Status.TotalChunks = 4
			res.Status.AvailableChunks = 4
			Expect(k8sClient.Status().Update(ctx, res)).To(Succeed())

			reconcileOnce()

			updated := &brokerv1alpha1.ClusterAdvertisement{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Available).To(BeTrue())
			Expect(updated.Status.AvailableChunks).To(Equal(int32(4)))
		})

		It("flips Available=false when LastSeen is older than StaleAfter and zeroes AvailableChunks", func() {
			res := &brokerv1alpha1.ClusterAdvertisement{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, res)).To(Succeed())
			old := metav1.NewTime(time.Now().Add(-30 * time.Second))
			res.Status.LastSeen = &old
			res.Status.Available = true
			res.Status.TotalChunks = 4
			res.Status.AvailableChunks = 4
			Expect(k8sClient.Status().Update(ctx, res)).To(Succeed())

			result := reconcileOnce()
			Expect(result.RequeueAfter).To(BeNumerically(">", time.Duration(0)))

			updated := &brokerv1alpha1.ClusterAdvertisement{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Available).To(BeFalse())
			Expect(updated.Status.AvailableChunks).To(Equal(int32(0)))
		})
	})
})
