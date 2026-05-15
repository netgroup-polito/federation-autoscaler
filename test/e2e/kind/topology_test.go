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

package kind

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClusterName(t *testing.T) {
	cases := map[Role]string{
		RoleCentral:   "fa-central",
		RoleConsumer1: "fa-consumer-1",
		RoleProvider1: "fa-provider-1",
		RoleProvider2: "fa-provider-2",
	}
	for role, want := range cases {
		if got := ClusterName(role); got != want {
			t.Errorf("ClusterName(%q) = %q, want %q", role, got, want)
		}
	}
}

func TestAllRolesIncludesEveryConstant(t *testing.T) {
	got := AllRoles()
	if len(got) != 4 {
		t.Fatalf("AllRoles() returned %d roles, want 4: %v", len(got), got)
	}
	seen := make(map[Role]bool, 4)
	for _, r := range got {
		seen[r] = true
	}
	for _, want := range []Role{RoleCentral, RoleConsumer1, RoleProvider1, RoleProvider2} {
		if !seen[want] {
			t.Errorf("AllRoles() missing %q (got %v)", want, got)
		}
	}
}

// TestConfigFilesExist confirms a config file is shipped for every role,
// so a future role addition without a config doesn't silently regress.
func TestConfigFilesExist(t *testing.T) {
	dir, err := defaultConfigDir()
	if err != nil {
		t.Fatalf("defaultConfigDir: %v", err)
	}
	for _, role := range AllRoles() {
		path := filepath.Join(dir, string(role)+".yaml")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("config for role %q missing at %q: %v", role, path, err)
		}
	}
}

func TestResolveKindBinary_Order(t *testing.T) {
	// Explicit beats the env var.
	t.Setenv("KIND", "from-env")
	got, err := resolveKindBinary("from-arg")
	if err != nil || got != "from-arg" {
		t.Fatalf("explicit arg should win; got %q err %v", got, err)
	}
	// Env var beats $PATH lookup.
	got, err = resolveKindBinary("")
	if err != nil || got != "from-env" {
		t.Fatalf("env var should win when arg empty; got %q err %v", got, err)
	}
}
