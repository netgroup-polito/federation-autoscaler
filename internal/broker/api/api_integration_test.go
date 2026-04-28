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

// Integration test driving every Broker REST endpoint (steps 4f / 4g / 4h)
// against a real envtest API server. Each spec resets authClusterID so the
// fake-auth middleware in suite_test.go injects the right caller.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

const (
	providerCluster = "provider-a"
	consumerCluster = "consumer-a"
)

var _ = Describe("Broker REST API", func() {
	BeforeEach(func() {
		authClusterID = ""
	})

	Describe("POST /api/v1/advertisements", func() {
		It("creates a ClusterAdvertisement and computes chunks", func() {
			authClusterID = providerCluster

			resp := doJSON(http.MethodPost, "/api/v1/advertisements", AdvertisementRequest{
				ClusterID:     providerCluster,
				LiqoClusterID: "liqo-" + providerCluster,
				Resources: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("8"),
					corev1.ResourceMemory: resource.MustParse("16Gi"),
				},
			})
			Expect(resp.Status).To(Equal(http.StatusOK), resp.Describe())

			var body AdvertisementResponse
			Expect(resp.DecodeInto(&body)).To(Succeed())
			Expect(body.Accepted).To(BeTrue())
			Expect(body.ChunkCount).To(Equal(int32(4))) // 8cpu / 16Gi → 4 standard

			// CR was actually persisted with a spec/status sourced from the request.
			cadv := &brokerv1alpha1.ClusterAdvertisement{}
			Expect(k8sClient.Get(suiteCtx,
				types.NamespacedName{Name: providerCluster, Namespace: testNamespace},
				cadv)).To(Succeed())
			Expect(cadv.Spec.LiqoClusterID).To(Equal("liqo-" + providerCluster))
			Expect(cadv.Status.TotalChunks).To(Equal(int32(4)))
			Expect(cadv.Status.Available).To(BeTrue())
		})

		It("rejects a CN/body cluster-ID mismatch with 403", func() {
			authClusterID = "intruder"
			resp := doJSON(http.MethodPost, "/api/v1/advertisements", AdvertisementRequest{
				ClusterID:     providerCluster,
				LiqoClusterID: "liqo-x",
				Resources:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			})
			Expect(resp.Status).To(Equal(http.StatusForbidden), resp.Describe())
		})
	})

	Describe("GET /api/v1/advertisements/{clusterId}", func() {
		It("returns 404 for an unknown advertisement", func() {
			authClusterID = "ghost-cluster"
			resp := doJSON(http.MethodGet, "/api/v1/advertisements/ghost-cluster", nil)
			Expect(resp.Status).To(Equal(http.StatusNotFound), resp.Describe())
		})

		It("returns the advertisement the upsert created", func() {
			authClusterID = providerCluster
			resp := doJSON(http.MethodGet, "/api/v1/advertisements/"+providerCluster, nil)
			Expect(resp.Status).To(Equal(http.StatusOK), resp.Describe())

			var snap AdvertisementSnapshot
			Expect(resp.DecodeInto(&snap)).To(Succeed())
			Expect(snap.ClusterID).To(Equal(providerCluster))
			Expect(snap.ChunkCount).To(Equal(int32(4)))
		})
	})

	Describe("POST /api/v1/heartbeat", func() {
		It("populates the consumer registry", func() {
			authClusterID = consumerCluster
			resp := doJSON(http.MethodPost, "/api/v1/heartbeat", HeartbeatRequest{
				ClusterID:     consumerCluster,
				LiqoClusterID: "liqo-" + consumerCluster,
			})
			Expect(resp.Status).To(Equal(http.StatusOK), resp.Describe())
		})
	})

	Describe("POST /api/v1/reservations", func() {
		It("returns 412 when the consumer hasn't heartbeated", func() {
			authClusterID = "consumer-b"
			resp := doJSON(http.MethodPost, "/api/v1/reservations", ReservationRequest{
				ProviderClusterID: providerCluster,
				ChunkCount:        1,
				ChunkType:         brokerv1alpha1.ChunkTypeStandard,
			})
			Expect(resp.Status).To(Equal(http.StatusPreconditionFailed), resp.Describe())
		})

		It("creates a Reservation and decrements available chunks", func() {
			authClusterID = consumerCluster
			resp := doJSON(http.MethodPost, "/api/v1/reservations", ReservationRequest{
				ProviderClusterID: providerCluster,
				ChunkCount:        2,
				ChunkType:         brokerv1alpha1.ChunkTypeStandard,
			})
			Expect(resp.Status).To(Equal(http.StatusCreated), resp.Describe())

			var body ReservationResponse
			Expect(resp.DecodeInto(&body)).To(Succeed())
			Expect(body.ReservationID).NotTo(BeEmpty())
			Expect(body.Status).To(Equal(brokerv1alpha1.ReservationPhasePending))

			// Reserved-chunk accounting flowed through to the advertisement.
			cadv := &brokerv1alpha1.ClusterAdvertisement{}
			Expect(k8sClient.Get(suiteCtx,
				types.NamespacedName{Name: providerCluster, Namespace: testNamespace},
				cadv)).To(Succeed())
			Expect(cadv.Status.ReservedChunks).To(Equal(int32(2)))
			Expect(cadv.Status.AvailableChunks).To(Equal(int32(2)))

			By("GET /api/v1/reservations/{id} returns it")
			authClusterID = consumerCluster
			getResp := doJSON(http.MethodGet, "/api/v1/reservations/"+body.ReservationID, nil)
			Expect(getResp.Status).To(Equal(http.StatusOK), getResp.Describe())
		})
	})

	Describe("GET /api/v1/instructions", func() {
		It("returns an empty list when there are no instructions", func() {
			authClusterID = providerCluster
			resp := doJSON(http.MethodGet, "/api/v1/instructions", nil)
			Expect(resp.Status).To(Equal(http.StatusOK), resp.Describe())

			var body InstructionsResponse
			Expect(resp.DecodeInto(&body)).To(Succeed())
			Expect(body.Instructions).To(BeEmpty())
		})
	})

	Describe("/healthz", func() {
		It("returns 200 OK without authentication", func() {
			resp := doJSON(http.MethodGet, "/healthz", nil)
			Expect(resp.Status).To(Equal(http.StatusOK))
		})
	})
})

// -----------------------------------------------------------------------------
// HTTP helpers
// -----------------------------------------------------------------------------

// recordedResponse is the test-side view of an HTTP response. The body is
// drained eagerly so we can both decode it and render it as part of an
// assertion failure message (Gomega evaluates the failure description
// strictly, which would race with json.Decoder otherwise).
type recordedResponse struct {
	Status int
	Body   []byte
}

// DecodeInto unmarshals the captured body into out.
func (r recordedResponse) DecodeInto(out any) error {
	return json.Unmarshal(r.Body, out)
}

// Describe renders a human-readable summary used as the failure message
// argument of Gomega assertions.
func (r recordedResponse) Describe() string {
	return "status=" + http.StatusText(r.Status) + " body=" + string(r.Body)
}

// doJSON sends a JSON request to the in-process test server and returns
// the response with its body already buffered.
func doJSON(method, path string, body any) recordedResponse {
	var buf bytes.Buffer
	if body != nil {
		Expect(json.NewEncoder(&buf).Encode(body)).To(Succeed())
	}
	req, err := http.NewRequestWithContext(context.Background(),
		method, httpServer.URL+path, &buf)
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Content-Type", ContentTypeJSON)

	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return recordedResponse{Status: resp.StatusCode, Body: raw}
}
