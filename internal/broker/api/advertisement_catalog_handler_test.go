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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/catalog"
)

const catTestNS = "federation-autoscaler-system"

// catFailingWriter is a catalog.Writer that always fails.
type catFailingWriter struct{}

func (catFailingWriter) UpsertOffer(context.Context, catalog.Offer) error {
	return errors.New("redis is down")
}

func (catFailingWriter) DeleteOffer(context.Context, string, string) error {
	return errors.New("redis is down")
}

// catServer builds a Server on a fake client (no envtest). The status
// subresource is registered so Status().Update behaves like a real cluster.
func catServer(t *testing.T, w catalog.Writer, objs ...client.Object) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := brokerv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add broker scheme: %v", err)
	}
	if err := autoscalingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add autoscaling scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&brokerv1alpha1.ClusterAdvertisement{}).
		Build()
	return NewServer(ServerOptions{Client: c, Namespace: catTestNS, Catalog: w})
}

// catPost sends one advertisement to the handler as catProvider.
func catPost(t *testing.T, s *Server, req *AdvertisementRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/advertisements", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(NewContextWithClusterID(r.Context(), catProvider))
	w := httptest.NewRecorder()
	s.handleAdvertisementUpsert(w, r)
	return w
}

func TestAdvertisementHandler_WritesOneOffer(t *testing.T) {
	mem := catalog.NewMemoryCatalog()
	s := catServer(t, mem)
	ctx := context.Background()

	w := catPost(t, s, catReq())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	got, err := mem.GetOffer(ctx, catProvider, "default")
	if err != nil {
		t.Fatalf("GetOffer: %v", err)
	}
	if got.ProviderID != catProvider || got.SiteID != "default" {
		t.Errorf("offer ids = %q/%q", got.ProviderID, got.SiteID)
	}
	catWantQty(t, got.Available, corev1.ResourceCPU, "8")
	catWantQty(t, got.Available, corev1.ResourceMemory, "16Gi")

	all, err := mem.ListOffers(ctx)
	if err != nil {
		t.Fatalf("ListOffers: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("offers = %d, want exactly 1", len(all))
	}
}

func TestAdvertisementHandler_SecondAdvertisementOverwrites(t *testing.T) {
	mem := catalog.NewMemoryCatalog()
	s := catServer(t, mem)
	ctx := context.Background()

	if w := catPost(t, s, catReq()); w.Code != http.StatusOK {
		t.Fatalf("first status = %d", w.Code)
	}
	second := catReq()
	second.Resources = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("6"),
		corev1.ResourceMemory: resource.MustParse("16Gi"),
	}
	if w := catPost(t, s, second); w.Code != http.StatusOK {
		t.Fatalf("second status = %d", w.Code)
	}

	got, err := mem.GetOffer(ctx, catProvider, "default")
	if err != nil {
		t.Fatalf("GetOffer: %v", err)
	}
	catWantQty(t, got.Available, corev1.ResourceCPU, "6")

	all, err := mem.ListOffers(ctx)
	if err != nil {
		t.Fatalf("ListOffers: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("offers = %d, want 1 (overwritten, not duplicated)", len(all))
	}
}

func TestAdvertisementHandler_OfferAvailableSubtractsReservedChunks(t *testing.T) {
	mem := catalog.NewMemoryCatalog()
	existing := &brokerv1alpha1.ClusterAdvertisement{
		ObjectMeta: metav1.ObjectMeta{Name: catProvider, Namespace: catTestNS},
		Status:     brokerv1alpha1.ClusterAdvertisementStatus{ReservedChunks: 2},
	}
	s := catServer(t, mem, existing)

	if w := catPost(t, s, catReq()); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	got, err := mem.GetOffer(context.Background(), catProvider, "default")
	if err != nil {
		t.Fatalf("GetOffer: %v", err)
	}
	// 8 cpu / 16Gi advertised, 2 chunks (2 cpu / 4Gi each) reserved.
	catWantQty(t, got.Available, corev1.ResourceCPU, "4")
	catWantQty(t, got.Available, corev1.ResourceMemory, "8Gi")
}

func TestAdvertisementHandler_FailingCatalogStillReturns200(t *testing.T) {
	s := catServer(t, catFailingWriter{})

	w := catPost(t, s, catReq())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (catalog is best effort), body = %s", w.Code, w.Body.String())
	}
}

func TestAdvertisementHandler_NilCatalogDoesNothing(t *testing.T) {
	s := catServer(t, nil)

	w := catPost(t, s, catReq())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", w.Code, w.Body.String())
	}
}
