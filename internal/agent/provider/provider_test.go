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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/health"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/poller"
)

func validOptions() Options {
	return Options{
		Client:        &agentclient.Client{},
		Registry:      poller.NewRegistry(),
		LocalClient:   newFakeClient(),
		ClusterID:     "provider-test",
		LiqoClusterID: "liqo-provider-test",
		Probe:         health.New(health.Options{}),
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
		{"missing probe", func(o *Options) { o.Probe = nil }, "Probe is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := validOptions()
			tc.mutate(&opts)
			err := Run(context.Background(), opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRun_BootstrapSucceedsWithValidOptions(t *testing.T) {
	if err := Run(context.Background(), validOptions()); err != nil {
		t.Fatalf("Run with valid options should succeed; got %v", err)
	}
}
