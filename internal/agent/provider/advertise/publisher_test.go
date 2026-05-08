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

package advertise

import (
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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// rewriteScheme rewrites a request whose URL the test client built with
// scheme=https to the underlying httptest.Server's actual http URL.
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

// fakeBroker captures advertisement POSTs and serves a configurable response.
type fakeBroker struct {
	mu       sync.Mutex
	srv      *httptest.Server
	posted   []brokerapi.AdvertisementRequest
	postCnt  atomic.Int32
	respond  func() (int, brokerapi.AdvertisementResponse)
	failNext atomic.Int32 // when >0, decremented per call and returns 503
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	fb := &fakeBroker{
		respond: func() (int, brokerapi.AdvertisementResponse) {
			return http.StatusOK, brokerapi.AdvertisementResponse{
				Accepted:   true,
				ChunkCount: 4,
			}
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/advertisements", func(w http.ResponseWriter, r *http.Request) {
		fb.postCnt.Add(1)
		if fb.failNext.Load() > 0 {
			fb.failNext.Add(-1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req brokerapi.AdvertisementRequest
		_ = json.Unmarshal(body, &req)

		fb.mu.Lock()
		fb.posted = append(fb.posted, req)
		fb.mu.Unlock()

		status, resp := fb.respond()
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	})
	fb.srv = httptest.NewServer(mux)
	t.Cleanup(fb.srv.Close)
	return fb
}

func (fb *fakeBroker) snapshotPosted() []brokerapi.AdvertisementRequest {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	out := make([]brokerapi.AdvertisementRequest, len(fb.posted))
	copy(out, fb.posted)
	return out
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

func newFakeKubeClient(nodes ...*corev1.Node) ctrlclient.Client {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	objs := make([]ctrlclient.Object, 0, len(nodes))
	for _, n := range nodes {
		objs = append(objs, n)
	}
	return clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func readyNode(name string, alloc corev1.ResourceList) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: alloc,
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
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
		{"missing client", Options{LocalClient: newFakeKubeClient(), ClusterID: "c", LiqoClusterID: "l"}, "Client is required"},
		{"missing local client", Options{Client: &agentclient.Client{}, ClusterID: "c", LiqoClusterID: "l"}, "LocalClient is required"},
		{"missing cluster id", Options{Client: &agentclient.Client{}, LocalClient: newFakeKubeClient(), LiqoClusterID: "l"}, "ClusterID is required"},
		{"missing liqo id", Options{Client: &agentclient.Client{}, LocalClient: newFakeKubeClient(), ClusterID: "c"}, "LiqoClusterID is required"},
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
// Run loop
// -----------------------------------------------------------------------------

func TestPublisher_PublishesImmediately_AndPostsCorrectBody(t *testing.T) {
	fb := newFakeBroker(t)
	kube := newFakeKubeClient(readyNode("worker-1", corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("8"),
		corev1.ResourceMemory: resource.MustParse("16Gi"),
	}))

	p, err := New(Options{
		Client:        fb.buildClient(t),
		LocalClient:   kube,
		ClusterID:     "provider-int",
		LiqoClusterID: "liqo-provider-int",
		Interval:      time.Hour, // never fires; we only want the immediate publish
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	// Allow the immediate publish to land.
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	posted := fb.snapshotPosted()
	if len(posted) != 1 {
		t.Fatalf("want 1 advertisement, got %d", len(posted))
	}
	if posted[0].ClusterID != "provider-int" || posted[0].LiqoClusterID != "liqo-provider-int" {
		t.Errorf("clusterID/liqoID mismatch: %+v", posted[0])
	}
	if got := posted[0].Resources[corev1.ResourceCPU]; got.Cmp(resource.MustParse("8")) != 0 {
		t.Errorf("CPU resource: want 8, got %s", got.String())
	}
}

func TestPublisher_TicksOnInterval(t *testing.T) {
	fb := newFakeBroker(t)
	kube := newFakeKubeClient(readyNode("worker-1", corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("1"),
	}))

	p, _ := New(Options{
		Client:        fb.buildClient(t),
		LocalClient:   kube,
		ClusterID:     "p",
		LiqoClusterID: "l",
		Interval:      40 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	// Initial + several ticks at 40ms over 200ms ≈ 4-5 publishes.
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	if got := fb.postCnt.Load(); got < 3 {
		t.Fatalf("want >=3 publishes over 200ms, got %d", got)
	}
}

func TestPublisher_OnResult_FailureThenSuccess(t *testing.T) {
	fb := newFakeBroker(t)
	fb.failNext.Store(2) // 2 raw HTTP failures consumed by client retry on first publish
	kube := newFakeKubeClient(readyNode("w", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}))

	var seen []bool
	var mu sync.Mutex
	p, _ := New(Options{
		Client:        fb.buildClient(t),
		LocalClient:   kube,
		ClusterID:     "p",
		LiqoClusterID: "l",
		Interval:      40 * time.Millisecond,
		OnPublishResult: func(ok bool) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, ok)
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	time.Sleep(180 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("want >=2 callbacks, got %d", len(seen))
	}
	if seen[0] {
		t.Errorf("first callback should be failure (broker forced 503), got %v", seen)
	}
	gotSuccess := false
	for _, ok := range seen[1:] {
		if ok {
			gotSuccess = true
			break
		}
	}
	if !gotSuccess {
		t.Errorf("want a later success callback, got %v", seen)
	}
}

// -----------------------------------------------------------------------------
// Snapshot failure path
// -----------------------------------------------------------------------------

type failingClient struct{ ctrlclient.Client }

func (failingClient) List(_ context.Context, _ ctrlclient.ObjectList, _ ...ctrlclient.ListOption) error {
	return errSnapshotBoom
}

var errSnapshotBoom = &snapshotErr{msg: "list nodes blew up"}

type snapshotErr struct{ msg string }

func (e *snapshotErr) Error() string { return e.msg }

func TestPublisher_SnapshotFailure_ReportsResultFalse(t *testing.T) {
	fb := newFakeBroker(t)

	var seen []bool
	var mu sync.Mutex
	p, _ := New(Options{
		Client:        fb.buildClient(t),
		LocalClient:   failingClient{},
		ClusterID:     "p",
		LiqoClusterID: "l",
		Interval:      time.Hour,
		OnPublishResult: func(ok bool) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, ok)
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	if got := fb.postCnt.Load(); got != 0 {
		t.Errorf("want 0 advertisements posted (snapshot failed), got %d", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] {
		t.Fatalf("want exactly one false callback, got %v", seen)
	}
}

// -----------------------------------------------------------------------------
// Cancellation
// -----------------------------------------------------------------------------

func TestPublisher_ContextCancellation_StopsPromptly(t *testing.T) {
	fb := newFakeBroker(t)
	kube := newFakeKubeClient()

	p, _ := New(Options{
		Client:        fb.buildClient(t),
		LocalClient:   kube,
		ClusterID:     "p",
		LiqoClusterID: "l",
		Interval:      time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s of ctx cancel")
	}
}
