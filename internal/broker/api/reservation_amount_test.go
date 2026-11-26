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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// Amount reservations: any CPU and memory amount as one virtual node, sharing
// the provider's capacity with fixed-size chunks. A provider and a consumer of
// their own keep these specs apart from the rest of the suite.
var _ = Describe("Amount reservations", Ordered, func() {
	const (
		amtProvider = "provider-amt"
		amtConsumer = "consumer-amt"
	)
	var amountID string

	amount := func(cpu, mem string) corev1.ResourceList {
		return corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		}
	}
	cadvStatus := func() brokerv1alpha1.ClusterAdvertisementStatus {
		cadv := &brokerv1alpha1.ClusterAdvertisement{}
		Expect(k8sClient.Get(suiteCtx,
			types.NamespacedName{Name: amtProvider, Namespace: testNamespace}, cadv)).To(Succeed())
		return cadv.Status
	}
	reserve := func(id string, req ReservationRequest) recordedResponse {
		authClusterID = amtConsumer
		return doJSONWithReservationID(http.MethodPost, "/api/v1/reservations", id, req)
	}

	BeforeAll(func() {
		authClusterID = amtProvider
		resp := doJSON(http.MethodPost, "/api/v1/advertisements", AdvertisementRequest{
			ClusterID:     amtProvider,
			LiqoClusterID: "liqo-" + amtProvider,
			Resources:     amount("8", "16Gi"), // 4 standard chunks
		})
		Expect(resp.Status).To(Equal(http.StatusOK), resp.Describe())

		authClusterID = amtConsumer
		resp = doJSON(http.MethodPost, "/api/v1/heartbeat", HeartbeatRequest{
			ClusterID:     amtConsumer,
			LiqoClusterID: "liqo-" + amtConsumer,
		})
		Expect(resp.Status).To(Equal(http.StatusOK), resp.Describe())
	})

	AfterAll(func() {
		authClusterID = ""
		var list brokerv1alpha1.ReservationList
		Expect(k8sClient.List(suiteCtx, &list, client.InNamespace(testNamespace))).To(Succeed())
		for i := range list.Items {
			if list.Items[i].Spec.ConsumerClusterID == amtConsumer {
				Expect(client.IgnoreNotFound(k8sClient.Delete(suiteCtx, &list.Items[i]))).To(Succeed())
			}
		}
		Expect(k8sClient.Delete(suiteCtx, &brokerv1alpha1.ClusterAdvertisement{
			ObjectMeta: metav1.ObjectMeta{Name: amtProvider, Namespace: testNamespace},
		})).To(Succeed())
	})

	It("reserves an amount as one node and leaves fewer chunks", func() {
		resp := reserve("amt-1", ReservationRequest{
			ProviderClusterID: amtProvider,
			Resources:         amount("3", "6Gi"),
		})
		Expect(resp.Status).To(Equal(http.StatusCreated), resp.Describe())

		var body ReservationResponse
		Expect(resp.DecodeInto(&body)).To(Succeed())
		amountID = body.ReservationID
		Expect(body.Amount).To(BeTrue())
		Expect(body.ChunkCount).To(Equal(int32(1)))
		Expect(body.ChunkType).To(Equal(brokerv1alpha1.ChunkTypeStandard))
		Expect(body.Resources.Cpu().String()).To(Equal("3"))
		Expect(body.Resources.Memory().String()).To(Equal("6Gi"))

		st := cadvStatus()
		Expect(st.ReservedResources.Cpu().String()).To(Equal("3"))
		Expect(st.ReservedChunks).To(Equal(int32(0)))
		Expect(st.TotalChunks).To(Equal(int32(2))) // 5 cpu / 10Gi left → 2 chunks
		Expect(st.AvailableChunks).To(Equal(int32(2)))
	})

	It("lets a fixed-size chunk use only what the amount left", func() {
		resp := reserve("amt-chunk", ReservationRequest{
			ProviderClusterID: amtProvider,
			ChunkCount:        1,
			ChunkType:         brokerv1alpha1.ChunkTypeStandard,
		})
		Expect(resp.Status).To(Equal(http.StatusCreated), resp.Describe())

		st := cadvStatus()
		Expect(st.ReservedChunks).To(Equal(int32(1)))
		Expect(st.AvailableChunks).To(Equal(int32(1)))
	})

	It("refuses an amount larger than what is free, and changes nothing", func() {
		// Free: 8 cpu - 2 (chunk) - 3 (amount) = 3 cpu.
		resp := reserve("amt-too-big", ReservationRequest{
			ProviderClusterID: amtProvider,
			Resources:         amount("4", "4Gi"),
		})
		Expect(resp.Status).To(Equal(http.StatusConflict), resp.Describe())
		Expect(string(resp.Body)).To(ContainSubstring(string(ErrCodeInsufficientCapacity)))

		st := cadvStatus()
		Expect(st.ReservedResources.Cpu().String()).To(Equal("3"))
		Expect(st.ReservedChunks).To(Equal(int32(1)))
	})

	DescribeTable("rejects a malformed amount with 400",
		func(req ReservationRequest) {
			req.ProviderClusterID = amtProvider
			resp := reserve("", req)
			Expect(resp.Status).To(Equal(http.StatusBadRequest), resp.Describe())
		},
		Entry("memory missing", ReservationRequest{
			Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
		}),
		Entry("negative cpu", ReservationRequest{Resources: amount("-1", "1Gi")}),
		Entry("unsupported resource", ReservationRequest{Resources: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("1"),
			corev1.ResourceMemory:           resource.MustParse("1Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
		}}),
		Entry("a node group", ReservationRequest{
			Resources: amount("1", "1Gi"), NodeGroupID: "ng-" + amtProvider + "-standard",
		}),
		Entry("two nodes", ReservationRequest{Resources: amount("1", "1Gi"), ChunkCount: 2}),
	)

	It("answers a retry with the same id, and refuses the same id with another amount", func() {
		resp := reserve(amountID, ReservationRequest{
			ProviderClusterID: amtProvider,
			Resources:         amount("3", "6Gi"),
		})
		Expect(resp.Status).To(Equal(http.StatusOK), resp.Describe())

		resp = reserve(amountID, ReservationRequest{
			ProviderClusterID: amtProvider,
			Resources:         amount("2", "6Gi"),
		})
		Expect(resp.Status).To(Equal(http.StatusConflict), resp.Describe())
	})

	It("only accepts or rejects: an unavailable provider refuses with 503 and nothing changes", func() {
		cadv := &brokerv1alpha1.ClusterAdvertisement{}
		key := types.NamespacedName{Name: amtProvider, Namespace: testNamespace}
		Expect(k8sClient.Get(suiteCtx, key, cadv)).To(Succeed())
		cadv.Status.Available = false
		Expect(k8sClient.Status().Update(suiteCtx, cadv)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Get(suiteCtx, key, cadv)).To(Succeed())
			cadv.Status.Available = true
			Expect(k8sClient.Status().Update(suiteCtx, cadv)).To(Succeed())
		})

		before := cadvStatus()
		resp := reserve("amt-unavailable", ReservationRequest{
			ProviderClusterID: amtProvider,
			Resources:         amount("1", "1Gi"),
		})
		Expect(resp.Status).To(Equal(http.StatusServiceUnavailable), resp.Describe())

		after := cadvStatus()
		Expect(after.ReservedResources).To(Equal(before.ReservedResources))
		Expect(after.ReservedChunks).To(Equal(before.ReservedChunks))
		Expect(apierrors.IsNotFound(k8sClient.Get(suiteCtx,
			types.NamespacedName{Name: "amt-unavailable", Namespace: testNamespace},
			&brokerv1alpha1.Reservation{}))).To(BeTrue())
	})

	It("keeps a booking for its own duration, and refuses a negative one", func() {
		resp := reserve("amt-duration", ReservationRequest{
			ProviderClusterID: amtProvider, Resources: amount("1", "1Gi"), DurationSeconds: 3600,
		})
		Expect(resp.Status).To(Equal(http.StatusCreated), resp.Describe())
		var body ReservationResponse
		Expect(resp.DecodeInto(&body)).To(Succeed())
		Expect(body.ExpiresAt).NotTo(BeNil())
		Expect(body.ExpiresAt.Sub(body.CreatedAt.Time)).To(Equal(time.Hour))
		authClusterID = amtConsumer
		Expect(doJSON(http.MethodDelete, "/api/v1/reservations/amt-duration", nil).Status).To(Equal(http.StatusAccepted))

		resp = reserve("amt-negative", ReservationRequest{
			ProviderClusterID: amtProvider, Resources: amount("1", "1Gi"), DurationSeconds: -1,
		})
		Expect(resp.Status).To(Equal(http.StatusBadRequest), resp.Describe())
	})

	It("keeps a booking without a duration forever when the broker has no default", func() {
		saved := testServer.reservationTimeout
		testServer.reservationTimeout = 0 // --reservation-timeout=0 (region orchestrator)
		DeferCleanup(func() { testServer.reservationTimeout = saved })

		resp := reserve("amt-forever", ReservationRequest{
			ProviderClusterID: amtProvider, Resources: amount("1", "1Gi"),
		})
		Expect(resp.Status).To(Equal(http.StatusCreated), resp.Describe())
		var body ReservationResponse
		Expect(resp.DecodeInto(&body)).To(Succeed())
		Expect(body.ExpiresAt).To(BeNil())
		authClusterID = amtConsumer
		Expect(doJSON(http.MethodDelete, "/api/v1/reservations/amt-forever", nil).Status).To(Equal(http.StatusAccepted))
	})

	It("refuses a new booking on an expired offer, but still answers a retry of an existing one", func() {
		past := metav1.NewTime(time.Now().Add(-time.Minute))
		future := metav1.NewTime(time.Now().Add(time.Minute))

		By("an expired offer: OfferExpired, nothing reserved")
		before := cadvStatus()
		resp := reserve("amt-stale", ReservationRequest{
			ProviderClusterID: amtProvider, Resources: amount("1", "1Gi"), OfferExpiresAt: &past,
		})
		Expect(resp.Status).To(Equal(http.StatusConflict), resp.Describe())
		Expect(string(resp.Body)).To(ContainSubstring(string(ErrCodeOfferExpired)))
		Expect(cadvStatus().ReservedResources).To(Equal(before.ReservedResources))

		By("a fresh offer: accepted")
		resp = reserve("amt-fresh", ReservationRequest{
			ProviderClusterID: amtProvider, Resources: amount("1", "1Gi"), OfferExpiresAt: &future,
		})
		Expect(resp.Status).To(Equal(http.StatusCreated), resp.Describe())

		By("the same booking retried after its offer expired: the existing booking, not a refusal")
		resp = reserve("amt-fresh", ReservationRequest{
			ProviderClusterID: amtProvider, Resources: amount("1", "1Gi"), OfferExpiresAt: &past,
		})
		Expect(resp.Status).To(Equal(http.StatusOK), resp.Describe())

		authClusterID = amtConsumer
		Expect(doJSON(http.MethodDelete, "/api/v1/reservations/amt-fresh", nil).Status).To(Equal(http.StatusAccepted))
	})

	It("keeps an amount out of the consumer's node groups", func() {
		authClusterID = amtConsumer
		resp := doJSON(http.MethodGet, "/api/v1/nodegroups", nil)
		Expect(resp.Status).To(Equal(http.StatusOK), resp.Describe())
		var body NodeGroupListResponse
		Expect(resp.DecodeInto(&body)).To(Succeed())
		for _, ng := range body.NodeGroups {
			if ng.ProviderClusterID == amtProvider {
				Expect(ng.CurrentReserved).To(Equal(int32(1)), "only the fixed-size chunk")
			}
		}
	})

	It("gives the amount back on release", func() {
		authClusterID = amtConsumer
		resp := doJSON(http.MethodDelete, "/api/v1/reservations/"+amountID, nil)
		Expect(resp.Status).To(Equal(http.StatusAccepted), resp.Describe())

		st := cadvStatus()
		Expect(st.ReservedResources).To(BeEmpty())
		Expect(st.TotalChunks).To(Equal(int32(4)))
		Expect(st.ReservedChunks).To(Equal(int32(1)))
		Expect(st.AvailableChunks).To(Equal(int32(3)))

		resv := &brokerv1alpha1.Reservation{}
		Expect(k8sClient.Get(suiteCtx,
			types.NamespacedName{Name: amountID, Namespace: testNamespace}, resv)).To(Succeed())
		Expect(brokerv1alpha1.IsChunksReleased(resv)).To(BeTrue())
	})

	It("counts amounts when a new advertisement refreshes the chunk total", func() {
		resp := reserve("amt-2", ReservationRequest{
			ProviderClusterID: amtProvider,
			Resources:         amount("2", "4Gi"),
		})
		Expect(resp.Status).To(Equal(http.StatusCreated), resp.Describe())

		authClusterID = amtProvider
		resp = doJSON(http.MethodPost, "/api/v1/advertisements", AdvertisementRequest{
			ClusterID:     amtProvider,
			LiqoClusterID: "liqo-" + amtProvider,
			Resources:     amount("8", "16Gi"),
		})
		Expect(resp.Status).To(Equal(http.StatusOK), resp.Describe())

		st := cadvStatus()
		Expect(st.TotalChunks).To(Equal(int32(3))) // 6 cpu / 12Gi left → 3 chunks
		Expect(st.AvailableChunks).To(Equal(int32(2)))
	})
})

// doJSONWithReservationID is doJSON with the X-Reservation-Id idempotency
// header set (left out when id is empty).
func doJSONWithReservationID(method, path, id string, body any) recordedResponse {
	var buf bytes.Buffer
	Expect(json.NewEncoder(&buf).Encode(body)).To(Succeed())
	req, err := http.NewRequestWithContext(context.Background(), method, httpServer.URL+path, &buf)
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Content-Type", ContentTypeJSON)
	if id != "" {
		req.Header.Set(HeaderReservationID, id)
	}
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return recordedResponse{Status: resp.StatusCode, Body: raw}
}
