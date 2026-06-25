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

const testNS = "federation-autoscaler-system"

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
	s, err := New(Options{Role: role, BindAddress: ":0", LocalClient: fc, Namespace: testNS})
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

// --- Region (ConfigMap upsert: create + update paths) ------------------------

func TestRegionUpsertCreatesConfigMap(t *testing.T) {
	fc, ts := newTestServer(t, RoleConsumer)
	wantStatus(t, post(t, ts, "/api/region", `{"region":"QC"}`), http.StatusOK)

	var cm corev1.ConfigMap
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: locationConfigMap}, &cm); err != nil {
		t.Fatalf("get agent-location: %v", err)
	}
	if got := cm.Data[locationKey]; !strings.Contains(got, `region: "QC"`) {
		t.Errorf("location.yaml = %q, want region QC", got)
	}
}

func TestRegionUpsertUpdatesAndPreservesOtherKeys(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: locationConfigMap, Namespace: testNS},
		Data:       map[string]string{locationKey: "region: \"NSW\"\n", "extra": "keep-me"},
	}
	fc, ts := newTestServer(t, RoleProvider, existing)
	wantStatus(t, post(t, ts, "/api/region", `{"region":"IDF"}`), http.StatusOK)

	var cm corev1.ConfigMap
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: locationConfigMap}, &cm); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(cm.Data[locationKey], `region: "IDF"`) {
		t.Errorf("region not updated: %q", cm.Data[locationKey])
	}
	if cm.Data["extra"] != "keep-me" {
		t.Errorf("unrelated key clobbered: %q", cm.Data["extra"])
	}
}

func TestRegionRejectsUnknownCode(t *testing.T) {
	_, ts := newTestServer(t, RoleConsumer)
	wantStatus(t, post(t, ts, "/api/region", `{"region":"ZZ"}`), http.StatusBadRequest)
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
	wantStatus(t, post(t, ts, "/api/capacity", `{"cpu":150,"memory":50}`), http.StatusOK)
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

// --- GET /api/state ----------------------------------------------------------

func TestStateConsumer(t *testing.T) {
	cp := &autoscalingv1alpha1.ConsumerPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: consumerPolicyName, Namespace: testNS},
		Spec:       autoscalingv1alpha1.ConsumerPolicySpec{Placement: autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyLatency}},
	}
	loc := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: locationConfigMap, Namespace: testNS},
		Data:       map[string]string{locationKey: "region: \"QC\"\n"},
	}
	_, ts := newTestServer(t, RoleConsumer, cp, loc)

	var st consumerState
	getJSON(t, ts, "/api/state", &st)
	if st.Role != RoleConsumer || st.Policy != "Latency" || st.Region != "QC" {
		t.Errorf("state = %+v", st)
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
		Data:       map[string]string{capacityKey: "cpu: 80\nmemory: 50\n"}, // bare ints
	}
	_, ts := newTestServer(t, RoleProvider, prices, capacity)

	var st providerState
	getJSON(t, ts, "/api/state", &st)
	if st.Role != RoleProvider {
		t.Fatalf("role = %q", st.Role)
	}
	if st.Prices["cpu"] != "0.050" || st.Prices["memory"] != "0.006" {
		t.Errorf("prices = %+v", st.Prices)
	}
	if st.Capacity["cpu"] != 80 || st.Capacity["memory"] != 50 {
		t.Errorf("capacity = %+v (bare ints must parse)", st.Capacity)
	}
}

// --- GET /api/regions --------------------------------------------------------

func TestRegionsEndpoint(t *testing.T) {
	_, ts := newTestServer(t, RoleConsumer)
	var out struct {
		Regions []struct {
			Code string `json:"code"`
			City string `json:"city"`
		} `json:"regions"`
	}
	getJSON(t, ts, "/api/regions", &out)
	if len(out.Regions) != 10 {
		t.Fatalf("want 10 regions, got %d", len(out.Regions))
	}
	seen := map[string]bool{}
	for _, r := range out.Regions {
		if r.Code == "" || r.City == "" {
			t.Errorf("incomplete region: %+v", r)
		}
		seen[r.Code] = true
	}
	for _, code := range []string{"QC", "IDF", "ENG", "NSW", "13"} {
		if !seen[code] {
			t.Errorf("missing region %q", code)
		}
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

func getJSON(t *testing.T, ts *httptest.Server, path string, dst any) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	wantStatus(t, resp.StatusCode, http.StatusOK)
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode %s: %v", path, err)
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
