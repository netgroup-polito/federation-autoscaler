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

package consumer

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/health"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/poller"
	"github.com/netgroup-polito/federation-autoscaler/internal/catalog"
)

// noNetTransport returns an error for every request. We use it instead
// of an httptest.Server because the unit tests here exercise wiring,
// not HTTP behaviour — and we MUST NOT pass an uninitialised
// *agentclient.Client to consumer.Run (it spawns a heartbeat goroutine
// that immediately calls PostHeartbeat, and a nil baseURL would crash
// the test process). The errored response propagates harmlessly
// through the heartbeat loop until ctx is cancelled.
type noNetTransport struct{}

func (noNetTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("test: no network")
}

// newTestBrokerClient builds an agentclient.Client whose Transport
// never reaches the network. Calls return errors; nothing panics.
func newTestBrokerClient(t *testing.T) *agentclient.Client {
	t.Helper()
	c, err := agentclient.New(agentclient.Options{
		BrokerURL:      "https://test.invalid:8443",
		Transport:      noNetTransport{},
		MaxRetries:     1,
		InitialBackoff: 1,
		MaxBackoff:     1,
		RequestTimeout: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func validOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Client:        newTestBrokerClient(t),
		Registry:      poller.NewRegistry(),
		LocalClient:   newFakeClient(),
		ClusterID:     "consumer-test",
		LiqoClusterID: "liqo-consumer-test",
		// Port 0 = OS-assigned free port; avoids cross-test collisions
		// in `go test -count=N` runs.
		LocalAPIAddr: "127.0.0.1:0",
		Probe:        health.New(health.Options{}),
	}
}

func newFakeClient() ctrlclient.Client {
	return clientfake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
}

func TestRun_RejectsMissingFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(o *Options)
		want   string
	}{
		{"missing client", func(o *Options) { o.Client = nil }, "Client is required"},
		{"missing registry", func(o *Options) { o.Registry = nil }, "Registry is required"},
		{"missing local client", func(o *Options) { o.LocalClient = nil }, "LocalClient is required"},
		{"missing cluster id", func(o *Options) { o.ClusterID = "" }, "ClusterID is required"},
		{"missing liqo id", func(o *Options) { o.LiqoClusterID = "" }, "LiqoClusterID is required"},
		{"missing local api addr", func(o *Options) { o.LocalAPIAddr = "" }, "LocalAPIAddr is required"},
		{"missing probe", func(o *Options) { o.Probe = nil }, "Probe is required"},
		{"unknown brain mode", func(o *Options) { o.BrainMode = "smart" }, "BrainMode must be"},
		{"local brain without catalog", func(o *Options) { o.BrainMode = BrainLocal }, "needs a catalog"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := validOptions(t)
			tc.mutate(&opts)
			// Rejection cases never reach the goroutine-spawning code,
			// so context.Background() is fine.
			err := Run(context.Background(), opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRun_BootstrapSucceedsWithValidOptions(t *testing.T) {
	// Run spawns long-lived goroutines (heartbeat poster + loopback
	// REST server). We MUST give them a cancellable ctx so they exit
	// when the test finishes; otherwise leaked goroutines keep firing
	// against the no-network transport / bound listener until the
	// process dies. defer cancel() runs before t.Cleanup, ensuring
	// orderly shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := Run(ctx, validOptions(t)); err != nil {
		t.Fatalf("Run with valid options should succeed; got %v", err)
	}
}

func TestRun_LocalBrainWithCatalogBootstraps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := validOptions(t)
	opts.BrainMode = BrainLocal
	opts.Catalog = catalog.NewMemoryCatalog()
	if err := Run(ctx, opts); err != nil {
		t.Fatalf("brain=local with a catalog should bootstrap; got %v", err)
	}
}
