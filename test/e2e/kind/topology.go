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

// Package kind boots and tears down the four-cluster Kind topology the
// step-13 e2e suite drives the happy-path federation scale-up through:
//
//   - central    — broker (REST API + reconcilers) + cert-manager Issuer chain
//   - consumer-1 — consumer agent + gRPC server + Cluster Autoscaler
//   - provider-1 — provider agent (donates capacity)
//   - provider-2 — provider agent (donates capacity)
//
// All four clusters share Kind's default docker network, which is what
// Liqo needs for cross-cluster pod / service routing. The helper shells
// out to the `kind` CLI rather than re-implementing Kind's bootstrap; the
// project's e2e setup already assumes Kind is on $PATH (see Makefile's
// KIND ?= kind).
//
// Bootstrap is idempotent: an existing cluster with the matching name is
// kept and reused, so iterating on the suite doesn't pay the full
// provisioning cost every run. Teardown removes every cluster Bootstrap
// touched.
package kind

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Role identifies a cluster in the topology. The values double as the
// Kind cluster name suffixes — Bootstrap prefixes "fa-" so a stray
// `kind get clusters` on a developer box doesn't trip over generic names.
type Role string

const (
	RoleCentral   Role = "central"
	RoleConsumer1 Role = "consumer-1"
	RoleProvider1 Role = "provider-1"
	RoleProvider2 Role = "provider-2"

	// ClusterNamePrefix scopes every Kind cluster name to this project
	// so other Kind users on the same host don't collide.
	ClusterNamePrefix = "fa-"
)

// AllRoles is the canonical order Bootstrap creates clusters in. central
// is first so its API server is ready by the time consumer / provider
// agents poll the broker.
func AllRoles() []Role {
	return []Role{RoleCentral, RoleConsumer1, RoleProvider1, RoleProvider2}
}

// ClusterName returns the Kind cluster name for a role.
func ClusterName(role Role) string { return ClusterNamePrefix + string(role) }

// Topology is the handle Bootstrap returns; the suite uses it to look up
// per-cluster kubeconfigs and to drive teardown.
type Topology struct {
	// KubeconfigDir is the directory where per-cluster kubeconfigs are
	// staged. Teardown removes it.
	KubeconfigDir string

	kubeconfigs map[Role]string
	kindBinary  string
	configDir   string
}

// Kubeconfig returns the absolute path to the kubeconfig for a role.
// Panics if the role isn't part of the topology — callers using
// AllRoles() are safe.
func (t *Topology) Kubeconfig(role Role) string {
	kc, ok := t.kubeconfigs[role]
	if !ok {
		panic(fmt.Sprintf("kind.Topology: no kubeconfig for role %q", role))
	}
	return kc
}

// Roles returns the roles whose clusters are part of this topology, in
// AllRoles() order.
func (t *Topology) Roles() []Role { return AllRoles() }

// BootstrapOptions tweaks the bootstrap behaviour. Sensible zero-value
// defaults: KindBinary "kind" from $PATH, ConfigDir computed relative
// to this source file.
type BootstrapOptions struct {
	// KindBinary overrides the `kind` executable. Empty resolves to
	// $KIND (env var, matching the Makefile) or "kind" on $PATH.
	KindBinary string

	// ConfigDir overrides the directory containing per-role Kind
	// configs (configs/central.yaml etc.). Empty resolves to
	// "configs/" next to this source file.
	ConfigDir string

	// KubeconfigDir overrides the directory where per-cluster
	// kubeconfigs are staged. Empty creates a fresh tmp dir.
	KubeconfigDir string
}

// Bootstrap creates the four-cluster topology, reusing any cluster
// whose name already exists. Returns a Topology whose Teardown method
// the caller must invoke to free resources.
func Bootstrap(ctx context.Context, opts BootstrapOptions) (*Topology, error) {
	kindBin, err := resolveKindBinary(opts.KindBinary)
	if err != nil {
		return nil, err
	}
	configDir := opts.ConfigDir
	if configDir == "" {
		configDir, err = defaultConfigDir()
		if err != nil {
			return nil, fmt.Errorf("resolve config dir: %w", err)
		}
	}
	kubeconfigDir := opts.KubeconfigDir
	if kubeconfigDir == "" {
		kubeconfigDir, err = os.MkdirTemp("", "fa-e2e-kubeconfigs-")
		if err != nil {
			return nil, fmt.Errorf("mkdir kubeconfig staging: %w", err)
		}
	}

	existing, err := listExistingClusters(ctx, kindBin)
	if err != nil {
		return nil, fmt.Errorf("list existing kind clusters: %w", err)
	}
	existingSet := make(map[string]struct{}, len(existing))
	for _, e := range existing {
		existingSet[e] = struct{}{}
	}

	t := &Topology{
		KubeconfigDir: kubeconfigDir,
		kubeconfigs:   make(map[Role]string, len(AllRoles())),
		kindBinary:    kindBin,
		configDir:     configDir,
	}

	for _, role := range AllRoles() {
		name := ClusterName(role)
		kubeconfigPath := filepath.Join(kubeconfigDir, string(role)+".kubeconfig")
		if _, alreadyUp := existingSet[name]; !alreadyUp {
			if err := createCluster(ctx, kindBin, name, t.configFor(role)); err != nil {
				return nil, fmt.Errorf("create cluster %q: %w", name, err)
			}
		}
		if err := exportKubeconfig(ctx, kindBin, name, kubeconfigPath); err != nil {
			return nil, fmt.Errorf("export kubeconfig for %q: %w", name, err)
		}
		t.kubeconfigs[role] = kubeconfigPath
	}
	return t, nil
}

// Teardown deletes every cluster in the topology. Failures are
// joined-and-returned rather than short-circuited so one bad cluster
// doesn't leak the other three.
func (t *Topology) Teardown(ctx context.Context) error {
	var errs []error
	for _, role := range AllRoles() {
		name := ClusterName(role)
		if err := deleteCluster(ctx, t.kindBinary, name); err != nil {
			errs = append(errs, fmt.Errorf("delete %q: %w", name, err))
		}
	}
	if t.KubeconfigDir != "" {
		if err := os.RemoveAll(t.KubeconfigDir); err != nil {
			errs = append(errs, fmt.Errorf("remove kubeconfig dir: %w", err))
		}
	}
	return errors.Join(errs...)
}

// configFor returns the absolute path to the Kind config for a role.
func (t *Topology) configFor(role Role) string {
	return filepath.Join(t.configDir, string(role)+".yaml")
}

// -----------------------------------------------------------------------------
// shell-out helpers
// -----------------------------------------------------------------------------

func createCluster(ctx context.Context, kindBin, name, configPath string) error {
	cmd := exec.CommandContext(ctx, kindBin, "create", "cluster",
		"--name", name, "--config", configPath, "--wait", "120s")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func deleteCluster(ctx context.Context, kindBin, name string) error {
	cmd := exec.CommandContext(ctx, kindBin, "delete", "cluster", "--name", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func exportKubeconfig(ctx context.Context, kindBin, name, dest string) error {
	cmd := exec.CommandContext(ctx, kindBin, "get", "kubeconfig", "--name", name)
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	return os.WriteFile(dest, out, 0o600)
}

func listExistingClusters(ctx context.Context, kindBin string) ([]string, error) {
	cmd := exec.CommandContext(ctx, kindBin, "get", "clusters")
	out, err := cmd.Output()
	if err != nil {
		// kind exits 1 ("No kind clusters found.") even on a clean host;
		// treat empty/single-message output as "no clusters" rather than
		// surfacing an error.
		if exit := new(exec.ExitError); errors.As(err, &exit) {
			if strings.Contains(string(exit.Stderr), "No kind clusters") {
				return nil, nil
			}
		}
		return nil, err
	}
	lines := strings.Split(string(out), "\n")
	clusters := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "No kind clusters") {
			continue
		}
		clusters = append(clusters, line)
	}
	return clusters, nil
}

// resolveKindBinary mirrors the Makefile's resolution order: explicit
// argument > $KIND env var > "kind" on $PATH.
func resolveKindBinary(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("KIND"); env != "" {
		return env, nil
	}
	if path, err := exec.LookPath("kind"); err == nil {
		return path, nil
	}
	return "", errors.New("kind binary not found; install kind and ensure it's on $PATH, or set $KIND")
}

// defaultConfigDir resolves to the configs/ directory living next to
// this source file at build time.
func defaultConfigDir() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("runtime.Caller failed to locate this source file")
	}
	return filepath.Join(filepath.Dir(thisFile), "configs"), nil
}
