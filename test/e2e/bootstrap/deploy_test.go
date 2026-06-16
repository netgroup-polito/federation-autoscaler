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

package bootstrap

import (
	"strings"
	"testing"

	"github.com/netgroup-polito/federation-autoscaler/test/e2e/kind"
)

func TestImagesForRole(t *testing.T) {
	cases := map[kind.Role][]string{
		kind.RoleCentral:   {"federation-autoscaler/broker:latest"},
		kind.RoleConsumer1: {"federation-autoscaler/agent:latest", "federation-autoscaler/grpc-server:latest"},
		kind.RoleProvider1: {"federation-autoscaler/agent:latest"},
		kind.RoleProvider2: {"federation-autoscaler/agent:latest"},
	}
	for role, want := range cases {
		got := ImagesForRole(role)
		if len(got) != len(want) {
			t.Errorf("%s: len(images) = %d, want %d (got %v)", role, len(got), len(want), got)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: image[%d] = %q, want %q", role, i, got[i], want[i])
			}
		}
	}
}

func TestOverlayPathForRole(t *testing.T) {
	cases := map[kind.Role][]string{
		kind.RoleCentral:   {"config/broker"},
		kind.RoleConsumer1: {"config/agent/consumer", "config/grpc-server"},
		kind.RoleProvider1: {"config/agent/provider"},
		kind.RoleProvider2: {"config/agent/provider"},
	}
	for role, want := range cases {
		got := OverlayPathForRole(role)
		if len(got) != len(want) {
			t.Errorf("%s: len(paths) = %d, want %d (got %v)", role, len(got), len(want), got)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: path[%d] = %q, want %q", role, i, got[i], want[i])
			}
		}
	}
}

// TestRenderClusterAutoscalerManifest spot-checks that the CA template
// expands with default options and surfaces every wire-load-bearing
// value (image, expander, gRPC address, cloud-config mount paths).
func TestRenderClusterAutoscalerManifest(t *testing.T) {
	out, err := renderClusterAutoscalerManifest(ClusterAutoscalerOptions{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	mustContain := []string{
		ClusterAutoscalerImage,
		"--cloud-provider=externalgrpc",
		"--expander=least-waste",
		"grpc-server.federation-autoscaler-system.svc.cluster.local:9443",
		"secretName: cluster-autoscaler-client-cert",
		"name: cluster-autoscaler-cloud-config",
		"issuerRef:\n    name: federation-autoscaler-ca-issuer",
		"namespace: federation-autoscaler-system",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("rendered manifest missing %q", s)
		}
	}
}

func TestRenderClusterAutoscalerManifest_HonorsOverrides(t *testing.T) {
	out, err := renderClusterAutoscalerManifest(ClusterAutoscalerOptions{
		Namespace:         "alt-ns",
		GRPCServerAddress: "alt-host:1234",
		Image:             "alt/ca:v0",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, s := range []string{"namespace: alt-ns", "address: alt-host:1234", "image: alt/ca:v0"} {
		if !strings.Contains(out, s) {
			t.Errorf("rendered manifest missing override %q", s)
		}
	}
}

func TestApplyOverlay_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		opts ApplyOverlayOptions
		want string
	}{
		{"missing kubeconfig", ApplyOverlayOptions{OverlayPath: "config/broker"}, "Kubeconfig"},
		{"missing overlay path", ApplyOverlayOptions{Kubeconfig: "kc"}, "OverlayPath"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ApplyOverlay(t.Context(), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q; got %v", tc.want, err)
			}
		})
	}
}

func TestLoadImage_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		opts LoadImageOptions
		want string
	}{
		{"missing cluster name", LoadImageOptions{Image: "x:latest"}, "ClusterName"},
		{"missing image", LoadImageOptions{ClusterName: "fa-x"}, "Image"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := LoadImage(t.Context(), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q; got %v", tc.want, err)
			}
		})
	}
}

func TestDeployClusterAutoscaler_RequiresKubeconfig(t *testing.T) {
	err := DeployClusterAutoscaler(t.Context(), ClusterAutoscalerOptions{})
	if err == nil || !strings.Contains(err.Error(), "Kubeconfig") {
		t.Fatalf("want Kubeconfig error; got %v", err)
	}
}

func TestPinBrokerServiceNodePort_RequiresKubeconfig(t *testing.T) {
	err := PinBrokerServiceNodePort(t.Context(), "")
	if err == nil || !strings.Contains(err.Error(), "kubeconfig") {
		t.Fatalf("want kubeconfig error; got %v", err)
	}
}
