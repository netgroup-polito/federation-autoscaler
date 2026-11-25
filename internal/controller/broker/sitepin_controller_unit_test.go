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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

const pinLabel = "example.org/site"

func pinSlice(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ResourceSliceGVK)
	u.SetName(name)
	u.SetNamespace("liqo-tenant-consumer-1")
	return u
}

func pinClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := brokerv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objs...).WithStatusSubresource(pinSlice("")).Build()
}

func pinRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "liqo-tenant-consumer-1"}}
}

func nodeSelectorOf(t *testing.T, c client.Client, name string) map[string]string {
	t.Helper()
	u := pinSlice(name)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(u), u); err != nil {
		t.Fatal(err)
	}
	sel, _, _ := unstructured.NestedStringMap(u.Object, "status", "nodeSelector")
	return sel
}

func TestSitePinSetsTheBookingsSite(t *testing.T) {
	resv := &brokerv1alpha1.Reservation{
		ObjectMeta: metav1.ObjectMeta{Name: "res-1", Namespace: "fa"},
		Spec:       brokerv1alpha1.ReservationSpec{Site: "cuneo"},
	}
	c := pinClient(t, resv, pinSlice("rs-res-1"))
	r := &SitePinReconciler{Client: c, Label: pinLabel, Namespace: "fa"}

	if _, err := r.Reconcile(context.Background(), pinRequest("rs-res-1")); err != nil {
		t.Fatal(err)
	}
	if got := nodeSelectorOf(t, c, "rs-res-1")[pinLabel]; got != "cuneo" {
		t.Fatalf("nodeSelector[%s] = %q; want cuneo", pinLabel, got)
	}
	// A second pass changes nothing and does not fail.
	if _, err := r.Reconcile(context.Background(), pinRequest("rs-res-1")); err != nil {
		t.Fatal(err)
	}
}

func TestSitePinLeavesOtherSlicesAlone(t *testing.T) {
	noSite := &brokerv1alpha1.Reservation{
		ObjectMeta: metav1.ObjectMeta{Name: "res-2", Namespace: "fa"},
	}
	c := pinClient(t, noSite, pinSlice("rs-res-2"), pinSlice("rs-unknown"), pinSlice("provider-1"))
	r := &SitePinReconciler{Client: c, Label: pinLabel, Namespace: "fa"}

	for _, name := range []string{"rs-res-2", "rs-unknown", "provider-1", "rs-missing"} {
		if _, err := r.Reconcile(context.Background(), pinRequest(name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"rs-res-2", "rs-unknown", "provider-1"} {
		if sel := nodeSelectorOf(t, c, name); len(sel) != 0 {
			t.Fatalf("%s: nodeSelector = %v; want none", name, sel)
		}
	}
}
