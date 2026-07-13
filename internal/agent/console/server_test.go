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

package console

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
)

const (
	testNS        = "federation-autoscaler-system"
	testClusterID = "consumer-1"
	testLiqoID    = "7f3a9c2e-0000-0000-0000-0000000000b1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := autoscalingv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add autoscaling scheme: %v", err)
	}
	return s
}

// newTestServer builds a console Server (role) backed by a fake client seeded
// with objs, plus an httptest.Server mounted on its handler.
func newTestServer(t *testing.T, role string, objs ...ctrlclient.Object) (ctrlclient.Client, *httptest.Server) {
	t.Helper()
	scheme := testScheme(t)
	fc := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	s, err := New(Options{
		Role: role, BindAddress: ":0", LocalClient: fc, Namespace: testNS,
		ClusterID: testClusterID, LiqoClusterID: testLiqoID,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return fc, ts
}

// post issues a JSON POST and returns the status code, closing the body.
func post(t *testing.T, ts *httptest.Server, path, body string) int {
	t.Helper()
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func wantStatus(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

// --- ConsumerPolicy (create / update / delete) -------------------------------

func TestPolicyCreateUpdateDelete(t *testing.T) {
	fc, ts := newTestServer(t, RoleConsumer)
	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNS, Name: consumerPolicyName}

	// Create.
	wantStatus(t, post(t, ts, "/api/policy", `{"type":"Price"}`), http.StatusOK)
	var cp autoscalingv1alpha1.ConsumerPolicy
	if err := fc.Get(ctx, key, &cp); err != nil {
		t.Fatalf("get after create: %v", err)
	}
	if cp.Spec.Placement.Type != autoscalingv1alpha1.PlacementStrategyPrice {
		t.Fatalf("type = %q, want Price", cp.Spec.Placement.Type)
	}

	// Update.
	wantStatus(t, post(t, ts, "/api/policy", `{"type":"Eco"}`), http.StatusOK)
	if err := fc.Get(ctx, key, &cp); err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if cp.Spec.Placement.Type != autoscalingv1alpha1.PlacementStrategyEco {
		t.Fatalf("type = %q, want Eco", cp.Spec.Placement.Type)
	}

	// Delete (empty = no policy).
	wantStatus(t, post(t, ts, "/api/policy", `{"type":""}`), http.StatusOK)
	if err := fc.Get(ctx, key, &cp); !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound after delete, got %v", err)
	}
}

func TestPolicyDeleteWhenAbsentIsOK(t *testing.T) {
	_, ts := newTestServer(t, RoleConsumer)
	wantStatus(t, post(t, ts, "/api/policy", `{"type":"None"}`), http.StatusOK)
}

func TestPolicyRejectsUnknownType(t *testing.T) {
	_, ts := newTestServer(t, RoleConsumer)
	wantStatus(t, post(t, ts, "/api/policy", `{"type":"Cheapest"}`), http.StatusBadRequest)
}

// --- Prices / Capacity (provider) --------------------------------------------

func TestPricesUpsertAndValidation(t *testing.T) {
	fc, ts := newTestServer(t, RoleProvider)

	// Invalid quantity → 400, nothing written.
	wantStatus(t, post(t, ts, "/api/prices", `{"cpu":"abc"}`), http.StatusBadRequest)

	wantStatus(t, post(t, ts, "/api/prices", `{"cpu":"0.020","memory":"0.003"}`), http.StatusOK)
	var cm corev1.ConfigMap
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: pricesConfigMap}, &cm); err != nil {
		t.Fatalf("get agent-prices: %v", err)
	}
	got := cm.Data[pricesKey]
	if !strings.Contains(got, `cpu: "0.020"`) || !strings.Contains(got, `memory: "0.003"`) {
		t.Errorf("prices.yaml = %q", got)
	}
}

func TestCapacityUpsertClamps(t *testing.T) {
	fc, ts := newTestServer(t, RoleProvider)
	// Percentages: "150" clamps to 100, "50" passes through. Values arrive as
	// strings (the UI sends the slider value or a fixed quantity).
	wantStatus(t, post(t, ts, "/api/capacity", `{"cpu":"150","memory":"50"}`), http.StatusOK)
	var cm corev1.ConfigMap
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: capacityConfigMap}, &cm); err != nil {
		t.Fatalf("get agent-capacity: %v", err)
	}
	got := cm.Data[capacityKey]
	if !strings.Contains(got, "cpu: 100") { // 150 clamped to 100
		t.Errorf("cpu not clamped to 100: %q", got)
	}
	if !strings.Contains(got, "memory: 50") {
		t.Errorf("memory missing: %q", got)
	}
}

func TestCapacityUpsertFixed(t *testing.T) {
	fc, ts := newTestServer(t, RoleProvider)
	// A CPU percentage plus a fixed memory quantity, mixed in one request.
	wantStatus(t, post(t, ts, "/api/capacity", `{"cpu":"80%","memory":"8Gi"}`), http.StatusOK)
	var cm corev1.ConfigMap
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: capacityConfigMap}, &cm); err != nil {
		t.Fatalf("get agent-capacity: %v", err)
	}
	got := cm.Data[capacityKey]
	if !strings.Contains(got, "cpu: 80") {
		t.Errorf("cpu percent missing: %q", got)
	}
	// Fixed quantities are written quoted so YAML keeps them as strings.
	if !strings.Contains(got, `memory: "8Gi"`) {
		t.Errorf("fixed memory not written as a quoted quantity: %q", got)
	}
	// A value that is neither a percentage nor a quantity is rejected.
	wantStatus(t, post(t, ts, "/api/capacity", `{"memory":"notaqty!"}`), http.StatusBadRequest)
}

// --- Workload (apply / delete, idempotent) -----------------------------------

func TestWorkloadApplyDelete(t *testing.T) {
	fc, ts := newTestServer(t, RoleConsumer)
	ctx := context.Background()
	key := types.NamespacedName{Namespace: workloadNamespace, Name: workloadName}

	wantStatus(t, post(t, ts, "/api/workload", `{"action":"apply"}`), http.StatusOK)
	var dep appsv1.Deployment
	if err := fc.Get(ctx, key, &dep); err != nil {
		t.Fatalf("get after apply: %v", err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 5 {
		t.Errorf("replicas = %v, want 5", dep.Spec.Replicas)
	}

	// Apply again → idempotent.
	wantStatus(t, post(t, ts, "/api/workload", `{"action":"apply"}`), http.StatusOK)

	wantStatus(t, post(t, ts, "/api/workload", `{"action":"delete"}`), http.StatusOK)
	if err := fc.Get(ctx, key, &dep); !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound after delete, got %v", err)
	}

	// Delete again → idempotent.
	wantStatus(t, post(t, ts, "/api/workload", `{"action":"delete"}`), http.StatusOK)
}

// --- Manual reservation (apply / delete) -------------------------------------

func TestReservationApplyDelete(t *testing.T) {
	fc, ts := newTestServer(t, RoleConsumer)
	ctx := context.Background()
	listReservations := func() []autoscalingv1alpha1.ResourceRequest {
		var l autoscalingv1alpha1.ResourceRequestList
		if err := fc.List(ctx, &l, ctrlclient.InNamespace(testNS)); err != nil {
			t.Fatalf("list: %v", err)
		}
		return l.Items
	}

	// apply creates a NEW, labelled ResourceRequest with the requested resources.
	wantStatus(t, post(t, ts, "/api/reservation", `{"action":"apply","cpu":"4","memory":"8Gi"}`), http.StatusOK)
	items := listReservations()
	if len(items) != 1 {
		t.Fatalf("want 1 reservation after apply, got %d", len(items))
	}
	rr := items[0]
	if rr.Labels[consoleManagedLabel] != consoleManagedValue {
		t.Errorf("missing console label: %v", rr.Labels)
	}
	if q := rr.Spec.Resources[corev1.ResourceCPU]; q.String() != "4" {
		t.Errorf("cpu = %s, want 4", q.String())
	}
	if q := rr.Spec.Resources[corev1.ResourceMemory]; q.String() != "8Gi" {
		t.Errorf("memory = %s, want 8Gi", q.String())
	}
	firstName := rr.Name

	// a second apply creates a SECOND reservation (not an update-in-place).
	wantStatus(t, post(t, ts, "/api/reservation", `{"action":"apply","cpu":"2"}`), http.StatusOK)
	if n := len(listReservations()); n != 2 {
		t.Fatalf("want 2 reservations after second apply, got %d", n)
	}

	// invalid quantity and empty request are rejected.
	wantStatus(t, post(t, ts, "/api/reservation", `{"action":"apply","cpu":"bad!"}`), http.StatusBadRequest)
	wantStatus(t, post(t, ts, "/api/reservation", `{"action":"apply"}`), http.StatusBadRequest)

	// delete requires a name; releasing by name removes exactly that one.
	wantStatus(t, post(t, ts, "/api/reservation", `{"action":"delete"}`), http.StatusBadRequest)
	wantStatus(t, post(t, ts, "/api/reservation", `{"action":"delete","name":"`+firstName+`"}`), http.StatusOK)
	items = listReservations()
	if len(items) != 1 {
		t.Fatalf("want 1 reservation after release, got %d", len(items))
	}
	if items[0].Name == firstName {
		t.Errorf("released the wrong reservation: %s still present", firstName)
	}

	// the state endpoint lists the remaining reservation.
	var st consumerState
	getState(t, ts, &st)
	if len(st.ManualReservations) != 1 {
		t.Errorf("state manualReservations = %d, want 1", len(st.ManualReservations))
	}

	// provider role has no manual-reservation endpoint.
	_, tsP := newTestServer(t, RoleProvider)
	wantStatus(t, post(t, tsP, "/api/reservation", `{"action":"apply","cpu":"1"}`), http.StatusMethodNotAllowed)
}

// --- GET /api/state ----------------------------------------------------------

func TestStateConsumer(t *testing.T) {
	cp := &autoscalingv1alpha1.ConsumerPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: consumerPolicyName, Namespace: testNS},
		Spec:       autoscalingv1alpha1.ConsumerPolicySpec{Placement: autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyLatency}},
	}
	_, ts := newTestServer(t, RoleConsumer, cp)

	var st consumerState
	getState(t, ts, &st)
	if st.Role != RoleConsumer || st.Policy != "Latency" {
		t.Errorf("state = %+v", st)
	}
	// No NodeName / MockGeoURL configured on the test server ⇒ location discovery
	// is off and the state carries no location.
	if st.Location != nil {
		t.Errorf("location = %+v, want nil (discovery disabled)", st.Location)
	}
	if st.ClusterID != testClusterID || st.LiqoClusterID != testLiqoID {
		t.Errorf("identity = %q / %q, want %q / %q", st.ClusterID, st.LiqoClusterID, testClusterID, testLiqoID)
	}
	if st.Workload.Present {
		t.Errorf("workload should be absent")
	}
	if st.Workload.Replicas != 5 {
		t.Errorf("display replicas = %d, want 5 (from embedded manifest)", st.Workload.Replicas)
	}
	if st.Workload.Requests["cpu"] == "" || st.Workload.Requests["memory"] == "" {
		t.Errorf("requests not surfaced: %+v", st.Workload.Requests)
	}
}

func TestStateProvider(t *testing.T) {
	prices := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: pricesConfigMap, Namespace: testNS},
		Data:       map[string]string{pricesKey: "cpu: \"0.050\"\nmemory: \"0.006\"\n"},
	}
	capacity := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: capacityConfigMap, Namespace: testNS},
		Data:       map[string]string{capacityKey: "cpu: 80\nmemory: 8Gi\n"}, // bare-int percent + fixed quantity
	}
	_, ts := newTestServer(t, RoleProvider, prices, capacity)

	var st providerState
	getState(t, ts, &st)
	if st.Role != RoleProvider {
		t.Fatalf("role = %q", st.Role)
	}
	if st.ClusterID != testClusterID || st.LiqoClusterID != testLiqoID {
		t.Errorf("identity = %q / %q, want %q / %q", st.ClusterID, st.LiqoClusterID, testClusterID, testLiqoID)
	}
	if st.Prices["cpu"] != "0.050" || st.Prices["memory"] != "0.006" {
		t.Errorf("prices = %+v", st.Prices)
	}
	if st.Capacity["cpu"] != "80" || st.Capacity["memory"] != "8Gi" {
		t.Errorf("capacity = %+v (want raw literals: percent \"80\", fixed \"8Gi\")", st.Capacity)
	}
}

// --- Role gating -------------------------------------------------------------

func TestRoleGating(t *testing.T) {
	_, consumerTS := newTestServer(t, RoleConsumer)
	if got := post(t, consumerTS, "/api/prices", `{"cpu":"0.01"}`); got == http.StatusOK {
		t.Errorf("consumer must not accept /api/prices, got %d", got)
	}

	_, providerTS := newTestServer(t, RoleProvider)
	if got := post(t, providerTS, "/api/policy", `{"type":"Price"}`); got == http.StatusOK {
		t.Errorf("provider must not accept /api/policy, got %d", got)
	}
}

// --- Index by role -----------------------------------------------------------

func TestIndexByRole(t *testing.T) {
	_, consumerTS := newTestServer(t, RoleConsumer)
	if body := getText(t, consumerTS, "/"); !strings.Contains(body, "Consumer Console") {
		t.Errorf("consumer index missing marker")
	}
	_, providerTS := newTestServer(t, RoleProvider)
	if body := getText(t, providerTS, "/"); !strings.Contains(body, "Provider Console") {
		t.Errorf("provider index missing marker")
	}
}

// --- Embedded-manifest drift guard -------------------------------------------

func TestEmbeddedWorkloadSizing(t *testing.T) {
	dep, err := decodeWorkload()
	if err != nil {
		t.Fatalf("decode embedded workload: %v", err)
	}
	if dep.Name != workloadName || dep.Namespace != workloadNamespace {
		t.Errorf("workload identity = %s/%s, want %s/%s", dep.Namespace, dep.Name, workloadNamespace, workloadName)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 5 {
		t.Errorf("replicas = %v, want 5", dep.Spec.Replicas)
	}
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("want 1 container, got %d", len(dep.Spec.Template.Spec.Containers))
	}
	req := dep.Spec.Template.Spec.Containers[0].Resources.Requests
	if req.Cpu().String() != "1" || req.Memory().String() != "1Gi" {
		t.Errorf("requests = %s CPU / %s mem, want 1 / 1Gi", req.Cpu(), req.Memory())
	}
}

// --- helpers -----------------------------------------------------------------

// getState fetches GET /api/state and decodes it into dst (the only JSON GET the
// console now exposes; region listing was removed with the region selector).
func getState(t *testing.T, ts *httptest.Server, dst any) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatalf("GET /api/state: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	wantStatus(t, resp.StatusCode, http.StatusOK)
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode /api/state: %v", err)
	}
}

func getText(t *testing.T, ts *httptest.Server, path string) string {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return buf.String()
}

func TestRenewableToggle(t *testing.T) {
	fc, ts := newTestServer(t, RoleProvider)
	ctx := context.Background()
	key := types.NamespacedName{Namespace: testNS, Name: renewableConfigMap}

	wantStatus(t, post(t, ts, "/api/renewable", `{"renewable":true}`), http.StatusOK)
	var cm corev1.ConfigMap
	if err := fc.Get(ctx, key, &cm); err != nil {
		t.Fatalf("get agent-renewable: %v", err)
	}
	if !strings.Contains(cm.Data[renewableKey], "renewable: true") {
		t.Errorf("want renewable: true, got %q", cm.Data[renewableKey])
	}
	var st providerState
	getState(t, ts, &st)
	if !st.Renewable {
		t.Error("state.renewable should be true")
	}

	// Clearing it flips the state back.
	wantStatus(t, post(t, ts, "/api/renewable", `{"renewable":false}`), http.StatusOK)
	getState(t, ts, &st)
	if st.Renewable {
		t.Error("state.renewable should be false after clearing")
	}

	// Consumer role has no renewable endpoint.
	_, tsC := newTestServer(t, RoleConsumer)
	wantStatus(t, post(t, tsC, "/api/renewable", `{"renewable":true}`), http.StatusMethodNotAllowed)
}
