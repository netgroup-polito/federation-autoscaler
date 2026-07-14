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

package heartbeat

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/latency"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// fakeLatencyProber returns a preset measured-latency result.
type fakeLatencyProber struct{ res latency.Result }

func (f fakeLatencyProber) LastMeasurements() latency.Result { return f.res }

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

// fakeBroker captures heartbeat POSTs and serves a configurable
// response.
type fakeBroker struct {
	mu       sync.Mutex
	srv      *httptest.Server
	posted   []brokerapi.HeartbeatRequest
	hitCnt   atomic.Int32
	failNext atomic.Int32 // when >0, decremented per call and returns 503
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	fb := &fakeBroker{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		fb.hitCnt.Add(1)
		if fb.failNext.Load() > 0 {
			fb.failNext.Add(-1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req brokerapi.HeartbeatRequest
		_ = json.Unmarshal(body, &req)

		fb.mu.Lock()
		fb.posted = append(fb.posted, req)
		fb.mu.Unlock()

		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.HeartbeatResponse{
			AckAt: metav1.NewTime(time.Now()),
		})
	})
	fb.srv = httptest.NewServer(mux)
	t.Cleanup(fb.srv.Close)
	return fb
}

func (fb *fakeBroker) snapshotPosted() []brokerapi.HeartbeatRequest {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	out := make([]brokerapi.HeartbeatRequest, len(fb.posted))
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

// -----------------------------------------------------------------------------
// Validation
// -----------------------------------------------------------------------------

func TestNew_Validation(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"missing client", Options{ClusterID: "c", LiqoClusterID: "l"}, "Client is required"},
		{"missing cluster id", Options{Client: &agentclient.Client{}, LiqoClusterID: "l"}, "ClusterID is required"},
		{"missing liqo id", Options{Client: &agentclient.Client{}, ClusterID: "c"}, "LiqoClusterID is required"},
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

func TestHeartbeater_BeatsImmediately_AndPostsCorrectBody(t *testing.T) {
	fb := newFakeBroker(t)

	h, err := New(Options{
		Client:        fb.buildClient(t),
		ClusterID:     "consumer-int",
		LiqoClusterID: "liqo-consumer-int",
		Interval:      time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.Run(ctx)
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	posted := fb.snapshotPosted()
	if len(posted) != 1 {
		t.Fatalf("want 1 heartbeat, got %d", len(posted))
	}
	if posted[0].ClusterID != "consumer-int" || posted[0].LiqoClusterID != "liqo-consumer-int" {
		t.Errorf("body mismatch: %+v", posted[0])
	}
}

func TestHeartbeater_ReportsMeasuredLatency(t *testing.T) {
	fb := newFakeBroker(t)
	h, err := New(Options{
		Client:        fb.buildClient(t),
		ClusterID:     "c",
		LiqoClusterID: "liqo-c",
		Interval:      time.Hour,
		Prober: fakeLatencyProber{res: latency.Result{
			Chosen: "p2",
			RTTs:   map[string]float64{"p1": 40, "p2": 12, "p3": math.Inf(1)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.beatOnce(context.Background())

	posted := fb.snapshotPosted()
	if len(posted) != 1 {
		t.Fatalf("want 1 heartbeat, got %d", len(posted))
	}
	got := posted[0]
	if got.ChosenProvider != "p2" {
		t.Errorf("chosenProvider = %q, want p2", got.ChosenProvider)
	}
	if got.MeasuredLatencies["p1"] != 40 || got.MeasuredLatencies["p2"] != 12 {
		t.Errorf("measured latencies = %+v", got.MeasuredLatencies)
	}
	if _, ok := got.MeasuredLatencies["p3"]; ok {
		t.Error("unreachable (+Inf) provider must be dropped from the report")
	}
}

func TestHeartbeater_TicksOnInterval(t *testing.T) {
	fb := newFakeBroker(t)

	h, _ := New(Options{
		Client:        fb.buildClient(t),
		ClusterID:     "c",
		LiqoClusterID: "l",
		Interval:      40 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.Run(ctx)
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	if got := fb.hitCnt.Load(); got < 3 {
		t.Fatalf("want >=3 heartbeats over 200ms, got %d", got)
	}
}

func TestHeartbeater_OnResult_FailureThenSuccess(t *testing.T) {
	fb := newFakeBroker(t)
	fb.failNext.Store(2) // first 2 raw HTTP attempts fail; client retries once per beat, so first beat fails

	var seen []bool
	var mu sync.Mutex
	h, _ := New(Options{
		Client:        fb.buildClient(t),
		ClusterID:     "c",
		LiqoClusterID: "l",
		Interval:      40 * time.Millisecond,
		OnHeartbeatResult: func(ok bool) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, ok)
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.Run(ctx)
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("want >=2 callbacks, got %d", len(seen))
	}
	if seen[0] {
		t.Errorf("first callback should be failure, got %v", seen)
	}
	sawSuccess := false
	for _, ok := range seen[1:] {
		if ok {
			sawSuccess = true
			break
		}
	}
	if !sawSuccess {
		t.Errorf("want a later success callback, got %v", seen)
	}
}

func TestHeartbeater_ContextCancellation_StopsPromptly(t *testing.T) {
	fb := newFakeBroker(t)

	h, _ := New(Options{
		Client:        fb.buildClient(t),
		ClusterID:     "c",
		LiqoClusterID: "l",
		Interval:      time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s of ctx cancel")
	}
}
