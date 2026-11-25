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

package api

import (
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/broker/sites"
)

// Region orchestrator with edge sites: every booking is placed on one site
// whose free room fits it, the site is stored on the Reservation and never
// returned to the consumer.
var _ = Describe("Edge-site placement", Ordered, func() {
	const (
		siteProvider = "provider-site"
		siteConsumer = "consumer-site"
		siteLabel    = "example.org/site"
	)
	nodes := []string{"edge-cuneo", "edge-asti"}

	rl := func(cpu, mem string) corev1.ResourceList {
		return corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		}
	}
	reserve := func(id string, req ReservationRequest) recordedResponse {
		authClusterID = siteConsumer
		return doJSONWithReservationID(http.MethodPost, "/api/v1/reservations", id, req)
	}
	siteOf := func(id string) string {
		resv := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(suiteCtx,
			types.NamespacedName{Name: id, Namespace: testNamespace}, resv)).To(Succeed())
		return resv.Spec.Site
	}

	BeforeAll(func() {
		for i, site := range []string{"cuneo", "asti"} {
			n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: nodes[i], Labels: map[string]string{siteLabel: site},
			}}
			Expect(k8sClient.Create(suiteCtx, n)).To(Succeed())
			n.Status.Allocatable = rl("4", "8Gi")
			n.Status.Capacity = rl("4", "8Gi")
			n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
			Expect(k8sClient.Status().Update(suiteCtx, n)).To(Succeed())
		}
		testServer.sites = &sites.Picker{Reader: k8sClient, Label: siteLabel, Namespace: testNamespace}

		authClusterID = siteProvider
		resp := doJSON(http.MethodPost, "/api/v1/advertisements", AdvertisementRequest{
			ClusterID: siteProvider, LiqoClusterID: "liqo-" + siteProvider,
			Resources: rl("8", "16Gi"), // the two sites together
		})
		Expect(resp.Status).To(Equal(http.StatusOK), resp.Describe())
		authClusterID = siteConsumer
		resp = doJSON(http.MethodPost, "/api/v1/heartbeat", HeartbeatRequest{
			ClusterID: siteConsumer, LiqoClusterID: "liqo-" + siteConsumer,
		})
		Expect(resp.Status).To(Equal(http.StatusOK), resp.Describe())
	})

	AfterAll(func() {
		testServer.sites = nil
		authClusterID = ""
		var list brokerv1alpha1.ReservationList
		Expect(k8sClient.List(suiteCtx, &list, client.InNamespace(testNamespace))).To(Succeed())
		for i := range list.Items {
			if list.Items[i].Spec.ConsumerClusterID == siteConsumer {
				Expect(client.IgnoreNotFound(k8sClient.Delete(suiteCtx, &list.Items[i]))).To(Succeed())
			}
		}
		Expect(k8sClient.Delete(suiteCtx, &brokerv1alpha1.ClusterAdvertisement{
			ObjectMeta: metav1.ObjectMeta{Name: siteProvider, Namespace: testNamespace},
		})).To(Succeed())
		for _, name := range nodes {
			Expect(k8sClient.Delete(suiteCtx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
		}
	})

	It("places a booking on a site and keeps the site out of the answer", func() {
		resp := reserve("site-1", ReservationRequest{ProviderClusterID: siteProvider, Resources: rl("3", "2Gi")})
		Expect(resp.Status).To(Equal(http.StatusCreated), resp.Describe())
		Expect(string(resp.Body)).NotTo(ContainSubstring("asti"))
		Expect(string(resp.Body)).NotTo(ContainSubstring("cuneo"))
		Expect(siteOf("site-1")).To(Equal("asti")) // tie on free CPU: first by name
	})

	It("puts the next booking on the site with the most free room", func() {
		resp := reserve("site-2", ReservationRequest{ProviderClusterID: siteProvider, Resources: rl("3", "2Gi")})
		Expect(resp.Status).To(Equal(http.StatusCreated), resp.Describe())
		Expect(siteOf("site-2")).To(Equal("cuneo"))
	})

	It("refuses a booking no single site can hold, though the region could", func() {
		// 1 cpu left in each site, 2 in the region.
		resp := reserve("site-3", ReservationRequest{ProviderClusterID: siteProvider, Resources: rl("2", "1Gi")})
		Expect(resp.Status).To(Equal(http.StatusConflict), resp.Describe())
		Expect(string(resp.Body)).To(ContainSubstring(string(ErrCodeInsufficientCapacity)))
	})

	It("gives the site's room back on release", func() {
		authClusterID = siteConsumer
		resp := doJSON(http.MethodDelete, "/api/v1/reservations/site-2", nil)
		Expect(resp.Status).To(Equal(http.StatusAccepted), resp.Describe())
		// The release marks it Unpeering; it frees the site once terminal.
		resv := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(suiteCtx, types.NamespacedName{Name: "site-2", Namespace: testNamespace}, resv)).To(Succeed())
		resv.Status.Phase = brokerv1alpha1.ReservationPhaseReleased
		Expect(k8sClient.Status().Update(suiteCtx, resv)).To(Succeed())

		resp = reserve("site-4", ReservationRequest{ProviderClusterID: siteProvider, Resources: rl("2", "1Gi")})
		Expect(resp.Status).To(Equal(http.StatusCreated), resp.Describe())
		Expect(siteOf("site-4")).To(Equal("cuneo"))
	})
})
