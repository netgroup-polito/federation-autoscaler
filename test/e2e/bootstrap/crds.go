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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// CRDOptions configures InstallCRDs.
type CRDOptions struct {
	// Kubeconfig is the path to the kubeconfig of the target cluster.
	// Required.
	Kubeconfig string

	// CRDDir is the directory containing the federation-autoscaler CRD
	// YAMLs. Empty resolves to config/crd/bases relative to the project
	// root via runtime.Caller — convenient for in-tree e2e tests.
	CRDDir string

	// KubectlBinary overrides the kubectl executable. Empty resolves to
	// $KUBECTL or "kubectl" on $PATH.
	KubectlBinary string
}

// InstallCRDs applies the federation-autoscaler CRDs to the target
// cluster. Idempotent — `kubectl apply` updates existing CRDs in
// place. Every cluster in the four-cluster topology gets the same set
// of CRDs because the broker / agent / grpc-server overlays can be
// composed in different combinations per cluster, and a missing CRD
// would surface as a confusing apply error.
func InstallCRDs(ctx context.Context, opts CRDOptions) error {
	if opts.Kubeconfig == "" {
		return fmt.Errorf("InstallCRDs: Kubeconfig %w", errEmpty)
	}
	kubectl, err := resolveBinary(opts.KubectlBinary, envKubectl, defaultKubectlBin)
	if err != nil {
		return err
	}
	crdDir := opts.CRDDir
	if crdDir == "" {
		crdDir, err = defaultCRDDir()
		if err != nil {
			return fmt.Errorf("resolve CRD dir: %w", err)
		}
	}
	if _, err := os.Stat(crdDir); err != nil {
		return fmt.Errorf("CRD dir %q: %w", crdDir, err)
	}

	args := withKubeconfig(opts.Kubeconfig, "apply", "-f", crdDir)
	if err := runCommand(ctx, kubectl, args...); err != nil {
		return fmt.Errorf("apply CRDs: %w", err)
	}
	return nil
}

// defaultCRDDir resolves to <project-root>/config/crd/bases. We assume
// this source file lives at test/e2e/bootstrap/crds.go and walk up
// three directories to reach the repo root.
func defaultCRDDir() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	// thisFile = .../test/e2e/bootstrap/crds.go
	// → ../../../ = repo root
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	return filepath.Join(root, "config", "crd", "bases"), nil
}
