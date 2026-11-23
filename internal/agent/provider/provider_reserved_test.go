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

package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/poller"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/catalog"
)

// toServer sends every request to target, so an agent client that insists on
// https can talk to a plain httptest server.
type toServer struct{ target *url.URL }

func (rt toServer) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.URL.Scheme = rt.target.Scheme
	r2.URL.Host = rt.target.Host
	return http.DefaultTransport.RoundTrip(r2)
}

// TestRun_PollReportedReservedChangeRewritesTheOffer runs the whole chain: a
// fake Broker answers the advertisement and the instruction poll, the real
// poller reads the reserved chunk count from every poll, and the provider
// rewrites its offer when it changes. The advertisement interval is an hour,
// so only the poll can change the offer.
func TestRun_PollReportedReservedChangeRewritesTheOffer(t *testing.T) {
	var reserved atomic.Int32
	reserved.Store(2)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/advertisements", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.AdvertisementResponse{
			Accepted:   true,
			ChunkCount: 4,
			ChunkResources: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
			ReservedChunks: reserved.Load(),
		})
	})
	mux.HandleFunc("GET /api/v1/instructions", func(w http.ResponseWriter, _ *http.Request) {
		n := reserved.Load()
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(brokerapi.InstructionsResponse{ReservedChunks: &n})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c, err := agentclient.New(agentclient.Options{
		BrokerURL:      "https://" + u.Host,
		Transport:      toServer{target: u},
		MaxRetries:     1,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		RequestTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("8"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
			},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	mem := catalog.NewMemoryCatalog()
	opts := validOptions(t)
	opts.Client = c
	opts.LocalClient = clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	opts.Catalog = mem
	opts.AdvertisementInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Run(ctx, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	p, err := poller.New(poller.Options{Client: c, Registry: opts.Registry, Interval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	go p.Run(ctx)

	waitCPU := func(want string) {
		t.Helper()
		var last corev1.ResourceList
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if got, err := mem.GetOffer(ctx, "provider-test", "default"); err == nil {
				last = got.Available
				if q := last[corev1.ResourceCPU]; q.Cmp(resource.MustParse(want)) == 0 {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("offer never reached %s cpu, last seen %v", want, last)
	}

	// 8 cpu minus 2 reserved chunks of 2 cpu.
	waitCPU("4")
	// The Broker now holds 3 chunks for this provider; the poll reports it.
	reserved.Store(3)
	waitCPU("2")
}
