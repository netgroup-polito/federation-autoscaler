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

package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/latency"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// fakeProber is a deterministic Prober for the latency-masking tests. It records
// the candidates it was asked to measure and returns a preset result.
type fakeProber struct {
	chosen string
	rtts   map[string]float64
	called bool
	cands  []latency.Candidate
}

func (f *fakeProber) MeasureAndPick(_ context.Context, cands []latency.Candidate) latency.Result {
	f.called = true
	f.cands = cands
	return latency.Result{Chosen: f.chosen, RTTs: f.rtts}
}

// localTestServerProber builds a localapi.Server with a measured-latency Prober.
func localTestServerProber(t *testing.T, fb *fakeBroker, p Prober) *httptest.Server {
	t.Helper()
	s, err := New(Options{
		BindAddress: "127.0.0.1:0",
		Client:      fb.buildClient(t),
		LocalClient: newFakeKubeClient(),
		Prober:      p,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// getNodeGroups issues GET /local/nodegroups and decodes the response.
func getNodeGroups(t *testing.T, ts *httptest.Server) brokerapi.NodeGroupListResponse {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + "/local/nodegroups")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	var out brokerapi.NodeGroupListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func headroom(resp brokerapi.NodeGroupListResponse) map[string]int32 {
	out := map[string]int32{}
	for _, v := range resp.NodeGroups {
		out[v.ProviderClusterID] = v.MaxSize - v.CurrentReserved
	}
	return out
}

func shortlistResp() brokerapi.NodeGroupListResponse {
	return brokerapi.NodeGroupListResponse{
		LatencyShortlist: true,
		NodeGroups: []brokerapi.NodeGroupView{
			{ID: "p1/standard", ProviderClusterID: "p1", Type: brokerv1alpha1.ChunkTypeStandard, MaxSize: 4, ProbeEndpoint: "10.0.0.1:30100"},
			{ID: "p2/standard", ProviderClusterID: "p2", Type: brokerv1alpha1.ChunkTypeStandard, MaxSize: 4, ProbeEndpoint: "10.0.0.2:30100"},
		},
	}
}

func TestNodeGroups_LatencyShortlist_MasksToMeasuredWinner(t *testing.T) {
	fb := newFakeBroker(t)
	fb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(shortlistResp())
	})
	fp := &fakeProber{chosen: "p2", rtts: map[string]float64{"p1": 40, "p2": 12}}
	ts := localTestServerProber(t, fb, fp)

	hr := headroom(getNodeGroups(t, ts))
	if hr["p2"] != 4 {
		t.Errorf("measured winner p2 must stay growable; got %+v", hr)
	}
	if hr["p1"] != 0 {
		t.Errorf("probe loser p1 must be masked (headroom 0); got %+v", hr)
	}
	if !fp.called || len(fp.cands) != 2 {
		t.Errorf("prober must be asked to measure both candidates; called=%v cands=%+v", fp.called, fp.cands)
	}
}

func TestNodeGroups_NoShortlist_PassesThrough(t *testing.T) {
	fb := newFakeBroker(t)
	fb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		r := shortlistResp()
		r.LatencyShortlist = false // e.g. a non-latency policy the Broker already masked
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(r)
	})
	fp := &fakeProber{chosen: "p2", rtts: map[string]float64{"p1": 40, "p2": 12}}
	ts := localTestServerProber(t, fb, fp)

	hr := headroom(getNodeGroups(t, ts))
	if hr["p1"] != 4 || hr["p2"] != 4 {
		t.Errorf("without a shortlist the list must pass through unchanged; got %+v", hr)
	}
	if fp.called {
		t.Error("prober must not be consulted when LatencyShortlist is false")
	}
}

func TestNodeGroups_Shortlist_AllUnreachable_NoInterference(t *testing.T) {
	fb := newFakeBroker(t)
	fb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(shortlistResp())
	})
	// Prober measures nobody (Chosen empty) → leave the Broker's shortlist as-is.
	fp := &fakeProber{chosen: ""}
	ts := localTestServerProber(t, fb, fp)

	hr := headroom(getNodeGroups(t, ts))
	if hr["p1"] != 4 || hr["p2"] != 4 {
		t.Errorf("all-unreachable must not mask the shortlist; got %+v", hr)
	}
}

// rewriteScheme forwards a request whose URL the test client built
// with scheme=https to the underlying httptest.Server's actual http URL.
type rewriteScheme struct {
	base   http.RoundTripper
	target *url.URL
}

func (rt rewriteScheme) RoundTrip(req *http.Request) (*http.Response, error) {
	cp := *req.URL
	cp.Scheme = rt.target.Scheme
	cp.Host = rt.target.Host
	r2 := req.Clone(req.Context())
	r2.URL = &cp
	return rt.base.RoundTrip(r2)
}

// fakeBroker is the upstream Broker the localapi proxies through to.
// It captures every inbound request for assertion.
type fakeBroker struct {
	t *testing.T

	srv *httptest.Server

	mu      sync.Mutex
	method  string
	path    string
	headers http.Header
	body    []byte
	postCnt atomic.Int32

	// nextHandler picks the response for the next request. Set
	// per-test before issuing the local-API call.
	nextHandler http.HandlerFunc
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	fb := &fakeBroker{t: t}
	fb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.postCnt.Add(1)
		fb.mu.Lock()
		fb.method = r.Method
		fb.path = r.URL.Path
		fb.headers = r.Header.Clone()
		fb.body, _ = io.ReadAll(r.Body)
		h := fb.nextHandler
		fb.mu.Unlock()
		if h == nil {
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		h(w, r)
	}))
	t.Cleanup(fb.srv.Close)
	return fb
}

func (fb *fakeBroker) setHandler(h http.HandlerFunc) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.nextHandler = h
}

func (fb *fakeBroker) buildClient(t *testing.T) *agentclient.Client {
	t.Helper()
	u, _ := url.Parse(fb.srv.URL)
	c, err := agentclient.New(agentclient.Options{
		BrokerURL:      "https://" + u.Host,
		Transport:      rewriteScheme{base: fb.srv.Client().Transport, target: u},
		MaxRetries:     1,
		InitialBackoff: time.Microsecond,
		MaxBackoff:     time.Microsecond,
		RequestTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newFakeKubeClient(objs ...ctrlclient.Object) ctrlclient.Client {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = autoscalingv1alpha1.AddToScheme(scheme)
	return clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
}

// localTestServer builds a localapi.Server mounted on an
// httptest.NewServer for the test client to call.
func localTestServer(t *testing.T, fb *fakeBroker) *httptest.Server {
	t.Helper()
	s, err := New(Options{
		BindAddress: "127.0.0.1:0", // placeholder; httptest assigns its own
		Client:      fb.buildClient(t),
		LocalClient: newFakeKubeClient(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// -----------------------------------------------------------------------------
// Validation
// -----------------------------------------------------------------------------

func TestNew_Validation(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"missing bind", Options{Client: &agentclient.Client{}, LocalClient: newFakeKubeClient()}, "BindAddress is required"},
		{"missing client", Options{BindAddress: "127.0.0.1:0", LocalClient: newFakeKubeClient()}, "Client is required"},
		{"missing local client", Options{BindAddress: "127.0.0.1:0", Client: &agentclient.Client{}}, "LocalClient is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// GET /local/nodegroups
// -----------------------------------------------------------------------------

func TestNodeGroups_ProxiesToBroker(t *testing.T) {
	fb := newFakeBroker(t)
	fb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.NodeGroupListResponse{
			NodeGroups: []brokerapi.NodeGroupView{
				{ID: "p1/standard", ProviderClusterID: "p1", Type: brokerv1alpha1.ChunkTypeStandard, MaxSize: 4},
			},
			Generation: 7,
		})
	})

	ts := localTestServer(t, fb)
	resp, err := ts.Client().Get(ts.URL + "/local/nodegroups")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	var out brokerapi.NodeGroupListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.NodeGroups) != 1 || out.NodeGroups[0].ID != "p1/standard" || out.Generation != 7 {
		t.Errorf("unexpected response: %+v", out)
	}
	if fb.path != "/api/v1/nodegroups" || fb.method != http.MethodGet {
		t.Errorf("broker call mismatch: %s %s", fb.method, fb.path)
	}
}

func TestNodeGroups_BrokerError_Forwarded(t *testing.T) {
	fb := newFakeBroker(t)
	fb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(brokerapi.ErrorResponse{
			Code: brokerapi.ErrCodeForbidden, Message: "no", RequestID: "rid",
		})
	})

	ts := localTestServer(t, fb)
	resp, err := ts.Client().Get(ts.URL + "/local/nodegroups")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
	var out brokerapi.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Code != brokerapi.ErrCodeForbidden || out.RequestID != "rid" {
		t.Errorf("error body not forwarded: %+v", out)
	}
}

// -----------------------------------------------------------------------------
// POST /local/reservations
// -----------------------------------------------------------------------------

func TestReservationCreate_ProxiesAndPreservesIdempotencyKey(t *testing.T) {
	fb := newFakeBroker(t)
	fb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(brokerapi.ReservationResponse{
			ReservationID: "res-from-caller",
			Status:        brokerv1alpha1.ReservationPhasePending,
			ChunkCount:    1,
			CreatedAt:     metav1.NewTime(time.Now()),
		})
	})

	ts := localTestServer(t, fb)
	req := brokerapi.ReservationRequest{
		ProviderClusterID: "p1",
		ChunkCount:        1,
		ChunkType:         brokerv1alpha1.ChunkTypeStandard,
	}
	buf, _ := json.Marshal(req)
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/local/reservations", bytes.NewReader(buf))
	r.Header.Set(brokerapi.HeaderReservationID, "res-from-caller")
	resp, err := ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	// The reservation id must be propagated unchanged to the broker.
	if got := fb.headers.Get(brokerapi.HeaderReservationID); got != "res-from-caller" {
		t.Errorf("X-Reservation-Id to broker: want res-from-caller, got %q", got)
	}
	// And echoed back to the caller as a response header.
	if got := resp.Header.Get(brokerapi.HeaderReservationID); got != "res-from-caller" {
		t.Errorf("X-Reservation-Id in response: want res-from-caller, got %q", got)
	}
}

func TestReservationCreate_MintsIdempotencyKeyWhenAbsent(t *testing.T) {
	fb := newFakeBroker(t)
	fb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(brokerapi.ReservationResponse{ChunkCount: 1})
	})

	ts := localTestServer(t, fb)
	buf, _ := json.Marshal(brokerapi.ReservationRequest{
		ProviderClusterID: "p1", ChunkCount: 1, ChunkType: brokerv1alpha1.ChunkTypeStandard,
	})
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/local/reservations", bytes.NewReader(buf))
	resp, err := ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if got := fb.headers.Get(brokerapi.HeaderReservationID); got == "" || !strings.HasPrefix(got, "res-") {
		t.Errorf("expected minted X-Reservation-Id (res-*), got %q", got)
	}
}

func TestReservationCreate_BadBody_400(t *testing.T) {
	fb := newFakeBroker(t)
	ts := localTestServer(t, fb)

	r, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/local/reservations", strings.NewReader("not json"))
	resp, err := ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// DELETE /local/reservations/{id}
// -----------------------------------------------------------------------------

func TestReservationDelete_ProxiesToBroker(t *testing.T) {
	fb := newFakeBroker(t)
	fb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.ReleaseResponse{
			ReservationID: "res-xyz",
			Status:        brokerv1alpha1.ReservationPhaseUnpeering,
		})
	})

	ts := localTestServer(t, fb)
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, ts.URL+"/local/reservations/res-xyz", nil)
	resp, err := ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	if fb.path != "/api/v1/reservations/res-xyz" || fb.method != http.MethodDelete {
		t.Errorf("broker call mismatch: %s %s", fb.method, fb.path)
	}
}

// -----------------------------------------------------------------------------
// GET /local/virtual-nodes
// -----------------------------------------------------------------------------

func TestVirtualNodes_EmptyList(t *testing.T) {
	fb := newFakeBroker(t)
	ts := localTestServer(t, fb)

	resp, err := ts.Client().Get(ts.URL + "/local/virtual-nodes")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out VirtualNodeListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.VirtualNodes == nil {
		t.Error("VirtualNodes should be an empty slice, not nil (JSON expects []) — got nil")
	}
	if len(out.VirtualNodes) != 0 {
		t.Errorf("want empty list, got %+v", out.VirtualNodes)
	}
	// And the localapi did NOT call the broker for this route.
	if got := fb.postCnt.Load(); got != 0 {
		t.Errorf("virtual-nodes should not hit the broker; saw %d calls", got)
	}
}

func TestVirtualNodes_ProjectsCRsToViews(t *testing.T) {
	running := &autoscalingv1alpha1.VirtualNodeState{
		ObjectMeta: metav1.ObjectMeta{Name: "vns-res-running", Namespace: "liqo"},
		Spec: autoscalingv1alpha1.VirtualNodeStateSpec{
			ProviderClusterID:     "provider-1",
			ProviderLiqoClusterID: "liqo-provider-1",
			NodeGroupID:           "ng-provider-1-standard",
			ReservationID:         "res-running",
			Resources: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
		Status: autoscalingv1alpha1.VirtualNodeStateStatus{
			Phase:           autoscalingv1alpha1.VirtualNodeStatePhaseRunning,
			VirtualNodeName: "rs-res-running",
			ProviderID:      "liqo://liqo-provider-1",
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("7800Mi"),
			},
		},
	}
	creating := &autoscalingv1alpha1.VirtualNodeState{
		ObjectMeta: metav1.ObjectMeta{Name: "vns-res-creating", Namespace: "liqo"},
		Spec: autoscalingv1alpha1.VirtualNodeStateSpec{
			ProviderClusterID:     "provider-2",
			ProviderLiqoClusterID: "liqo-provider-2",
			NodeGroupID:           "ng-provider-2-standard",
			ReservationID:         "res-creating",
		},
		Status: autoscalingv1alpha1.VirtualNodeStateStatus{
			Phase: autoscalingv1alpha1.VirtualNodeStatePhaseCreating,
			// VirtualNodeName intentionally empty — CR name should
			// surface as the placeholder.
		},
	}

	fb := newFakeBroker(t)
	s, err := New(Options{
		BindAddress: "127.0.0.1:0",
		Client:      fb.buildClient(t),
		LocalClient: newFakeKubeClient(running, creating),
		Namespace:   "liqo",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := ts.Client().Get(ts.URL + "/local/virtual-nodes")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	var out VirtualNodeListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.VirtualNodes) != 2 {
		t.Fatalf("want 2 views, got %d: %+v", len(out.VirtualNodes), out.VirtualNodes)
	}
	byID := map[string]VirtualNodeView{}
	for _, v := range out.VirtualNodes {
		byID[v.ReservationID] = v
	}

	r := byID["res-running"]
	if r.Name != "rs-res-running" {
		t.Errorf("running.Name: want rs-res-running, got %q", r.Name)
	}
	if r.VirtualNodeName != "rs-res-running" {
		t.Errorf("running.VirtualNodeName: want rs-res-running (status populated), got %q", r.VirtualNodeName)
	}
	if r.ProviderID != "liqo://liqo-provider-1" {
		t.Errorf("running.ProviderID: want liqo://liqo-provider-1, got %q", r.ProviderID)
	}
	if r.NodeGroupID != "ng-provider-1-standard" {
		t.Errorf("running.NodeGroupID: want ng-provider-1-standard, got %q", r.NodeGroupID)
	}
	if r.Phase != autoscalingv1alpha1.VirtualNodeStatePhaseRunning {
		t.Errorf("running.Phase: want Running, got %s", r.Phase)
	}
	if got := r.Allocatable[corev1.ResourceCPU]; got.Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("running.Allocatable[cpu]: want 4, got %s", got.String())
	}

	c := byID["res-creating"]
	// Status.VirtualNodeName is empty so the CR name is the placeholder.
	if c.Name != "vns-res-creating" {
		t.Errorf("creating.Name: want vns-res-creating placeholder, got %q", c.Name)
	}
	// The raw VirtualNodeName stays empty (no fallback) — this is what
	// NodeGroupNodes keys off to skip un-materialised nodes.
	if c.VirtualNodeName != "" {
		t.Errorf("creating.VirtualNodeName: want empty (node not materialised), got %q", c.VirtualNodeName)
	}
	if c.Phase != autoscalingv1alpha1.VirtualNodeStatePhaseCreating {
		t.Errorf("creating.Phase: want Creating, got %s", c.Phase)
	}
	if len(c.Allocatable) != 0 {
		t.Errorf("creating.Allocatable: want empty, got %+v", c.Allocatable)
	}

	if got := fb.postCnt.Load(); got != 0 {
		t.Errorf("virtual-nodes must not hit the broker; saw %d calls", got)
	}
}

func TestVirtualNodes_ScopedToConfiguredNamespace(t *testing.T) {
	inScope := &autoscalingv1alpha1.VirtualNodeState{
		ObjectMeta: metav1.ObjectMeta{Name: "vns-1", Namespace: "liqo"},
		Spec:       autoscalingv1alpha1.VirtualNodeStateSpec{ReservationID: "res-1"},
	}
	outOfScope := &autoscalingv1alpha1.VirtualNodeState{
		ObjectMeta: metav1.ObjectMeta{Name: "vns-2", Namespace: "other"},
		Spec:       autoscalingv1alpha1.VirtualNodeStateSpec{ReservationID: "res-2"},
	}

	fb := newFakeBroker(t)
	s, err := New(Options{
		BindAddress: "127.0.0.1:0",
		Client:      fb.buildClient(t),
		LocalClient: newFakeKubeClient(inScope, outOfScope),
		Namespace:   "liqo",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, _ := ts.Client().Get(ts.URL + "/local/virtual-nodes")
	defer func() { _ = resp.Body.Close() }()
	var out VirtualNodeListResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.VirtualNodes) != 1 || out.VirtualNodes[0].ReservationID != "res-1" {
		t.Fatalf("want only res-1 in liqo namespace; got %+v", out.VirtualNodes)
	}
}

// -----------------------------------------------------------------------------
// Healthz / lifecycle
// -----------------------------------------------------------------------------

func TestHealthz(t *testing.T) {
	fb := newFakeBroker(t)
	ts := localTestServer(t, fb)
	resp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}

func TestRun_StartsAndStops(t *testing.T) {
	fb := newFakeBroker(t)
	addr := pickFreeAddr(t)
	s, err := New(Options{
		BindAddress: addr,
		Client:      fb.buildClient(t),
		LocalClient: newFakeKubeClient(),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Wait briefly for the listener to bind, then exercise it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz") //nolint:noctx
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error on shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of ctx cancel")
	}
}

// pickFreeAddr returns a 127.0.0.1 address the OS just confirmed is
// free. There's a sub-millisecond race window between close and the
// caller's bind on Linux; acceptable for unit tests.
func pickFreeAddr(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	return addr
}
