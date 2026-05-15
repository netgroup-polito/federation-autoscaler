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

func TestIdentityForRole(t *testing.T) {
	for _, role := range kind.AllRoles() {
		id := IdentityForRole(role)
		wantClusterID := kind.ClusterName(role)
		if id.ClusterID != wantClusterID {
			t.Errorf("%s: ClusterID = %q, want %q", role, id.ClusterID, wantClusterID)
		}
		if id.LiqoClusterID != id.ClusterID {
			t.Errorf("%s: LiqoClusterID %q != ClusterID %q (must be equal per docs/design.md §7.0)",
				role, id.LiqoClusterID, id.ClusterID)
		}
		if !strings.HasPrefix(id.BrokerURL, "https://fa-central-control-plane:") {
			t.Errorf("%s: BrokerURL %q must target the central cluster's control-plane container",
				role, id.BrokerURL)
		}
	}
}

func TestResolveBinary_Order(t *testing.T) {
	t.Setenv(envKubectl, "from-env")
	got, err := resolveBinary("from-arg", envKubectl, defaultKubectlBin)
	if err != nil || got != "from-arg" {
		t.Fatalf("explicit arg should win; got %q err %v", got, err)
	}
	got, err = resolveBinary("", envKubectl, defaultKubectlBin)
	if err != nil || got != "from-env" {
		t.Fatalf("env var should win when arg empty; got %q err %v", got, err)
	}
}

func TestWithKubeconfig_PrependsFlag(t *testing.T) {
	got := withKubeconfig("/tmp/kc", "apply", "-f", "thing.yaml")
	want := []string{"--kubeconfig=/tmp/kc", "apply", "-f", "thing.yaml"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("arg[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestInstallCertManager_ValidationErrors(t *testing.T) {
	err := InstallCertManager(t.Context(), CertManagerOptions{}) //nolint:contextcheck
	if err == nil || !strings.Contains(err.Error(), "Kubeconfig") {
		t.Fatalf("missing Kubeconfig must error; got %v", err)
	}
}

func TestInstallLiqo_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		opts LiqoOptions
		want string
	}{
		{"missing kubeconfig", LiqoOptions{ClusterName: "c", Identity: Identity{ClusterID: "x"}}, "Kubeconfig"},
		{"missing cluster name", LiqoOptions{Kubeconfig: "kc", Identity: Identity{ClusterID: "x"}}, "ClusterName"},
		{"missing identity", LiqoOptions{Kubeconfig: "kc", ClusterName: "c"}, "ClusterID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := InstallLiqo(t.Context(), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q; got %v", tc.want, err)
			}
		})
	}
}

func TestPatchAgentConfig_ValidationErrors(t *testing.T) {
	fullID := Identity{ClusterID: "c", LiqoClusterID: "c", BrokerURL: "https://x"}
	cases := []struct {
		name string
		opts AgentConfigOptions
		want string
	}{
		{"missing kubeconfig", AgentConfigOptions{
			Identity: fullID,
		}, "Kubeconfig"},
		{"missing cluster id", AgentConfigOptions{
			Kubeconfig: "kc",
			Identity:   Identity{LiqoClusterID: "c", BrokerURL: "https://x"},
		}, "ClusterID"},
		{"missing liqo id", AgentConfigOptions{
			Kubeconfig: "kc",
			Identity:   Identity{ClusterID: "c", BrokerURL: "https://x"},
		}, "LiqoClusterID"},
		{"missing broker url", AgentConfigOptions{
			Kubeconfig: "kc",
			Identity:   Identity{ClusterID: "c", LiqoClusterID: "c"},
		}, "BrokerURL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := PatchAgentConfig(t.Context(), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q; got %v", tc.want, err)
			}
		})
	}
}

func TestInstallCRDs_DefaultDirResolvesToConfigCRD(t *testing.T) {
	dir, err := defaultCRDDir()
	if err != nil {
		t.Fatalf("defaultCRDDir: %v", err)
	}
	if !strings.HasSuffix(dir, "/config/crd/bases") {
		t.Errorf("defaultCRDDir = %q, want suffix /config/crd/bases", dir)
	}
}
