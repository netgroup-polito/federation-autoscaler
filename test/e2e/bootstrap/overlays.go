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
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/netgroup-polito/federation-autoscaler/test/e2e/kind"
)

// envKustomize is the env-var name used to override the kustomize binary
// (matches the Makefile-style KIND ?= kind convention).
const envKustomize = "KUSTOMIZE"
const defaultKustomizeBin = "kustomize"

// OverlayPathForRole returns the path (relative to repo root) of the
// kustomize overlay that should be applied to a given Kind role.
//
// The central cluster gets the broker; consumer-1 gets the consumer
// agent + the gRPC server; provider clusters get the provider agent.
func OverlayPathForRole(role kind.Role) []string {
	switch role {
	case kind.RoleCentral:
		return []string{"config/broker"}
	case kind.RoleConsumer1:
		return []string{"config/agent/consumer", "config/grpc-server"}
	case kind.RoleProvider1, kind.RoleProvider2:
		return []string{"config/agent/provider"}
	default:
		return nil
	}
}

// ApplyOverlayOptions configures ApplyOverlay.
type ApplyOverlayOptions struct {
	// Kubeconfig is the path to the kubeconfig of the target cluster.
	// Required.
	Kubeconfig string

	// OverlayPath is the kustomize overlay directory (e.g.
	// "config/broker"). Resolved relative to the repo root when not
	// absolute. Required.
	OverlayPath string

	// CRDs (default true) decides whether the federation-autoscaler
	// CRDs are bundled with the overlay apply. Most overlays do NOT
	// include CRDs themselves so the suite layers them in via
	// InstallCRDs; set this false when the caller has already invoked
	// InstallCRDs.
	CRDs bool

	// KustomizeBinary / KubectlBinary override the executables. Empty
	// resolves to $KUSTOMIZE / $KUBECTL or the default name on $PATH.
	KustomizeBinary string
	KubectlBinary   string
}

// ApplyOverlay renders the kustomize overlay and pipes it through
// `kubectl apply -f -` against the target kubeconfig. Idempotent.
//
// The kustomize binary is invoked from the repo root so relative
// `resources:` references resolve correctly.
func ApplyOverlay(ctx context.Context, opts ApplyOverlayOptions) error {
	switch {
	case opts.Kubeconfig == "":
		return fmt.Errorf("ApplyOverlay: Kubeconfig %w", errEmpty)
	case opts.OverlayPath == "":
		return fmt.Errorf("ApplyOverlay: OverlayPath %w", errEmpty)
	}
	kustomizeBin, err := resolveBinary(opts.KustomizeBinary, envKustomize, defaultKustomizeBin)
	if err != nil {
		return err
	}
	kubectl, err := resolveBinary(opts.KubectlBinary, envKubectl, defaultKubectlBin)
	if err != nil {
		return err
	}
	overlayPath := opts.OverlayPath
	if !filepath.IsAbs(overlayPath) {
		root, err := repoRoot()
		if err != nil {
			return fmt.Errorf("resolve repo root: %w", err)
		}
		overlayPath = filepath.Join(root, overlayPath)
	}

	build := exec.CommandContext(ctx, kustomizeBin, "build", overlayPath)
	apply := exec.CommandContext(ctx, kubectl, "--kubeconfig="+opts.Kubeconfig, "apply", "-f", "-")

	apply.Stdin, err = build.StdoutPipe()
	if err != nil {
		return fmt.Errorf("kustomize stdout pipe: %w", err)
	}
	var applyStderr strings.Builder
	apply.Stderr = &applyStderr

	if err := apply.Start(); err != nil {
		return fmt.Errorf("kubectl apply start: %w", err)
	}
	buildOut, buildErr := build.CombinedOutput()
	if buildErr != nil {
		_ = apply.Process.Kill()
		return fmt.Errorf("kustomize build %q: %w: %s",
			overlayPath, buildErr, strings.TrimSpace(string(buildOut)))
	}
	if err := apply.Wait(); err != nil {
		return fmt.Errorf("kubectl apply: %w: %s", err, strings.TrimSpace(applyStderr.String()))
	}
	return nil
}

// PinBrokerServiceNodePort kubectl-patches the broker Service to a
// NodePort on CentralBrokerNodePort. Idempotent. Agents on consumer /
// provider clusters reach the broker via this NodePort across the
// shared docker network the Kind clusters all sit on.
func PinBrokerServiceNodePort(ctx context.Context, kubeconfig string) error {
	if kubeconfig == "" {
		return fmt.Errorf("PinBrokerServiceNodePort: kubeconfig %w", errEmpty)
	}
	kubectl, err := resolveBinary("", envKubectl, defaultKubectlBin)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"type": "NodePort",
			"ports": []map[string]any{
				{
					"name":     "api",
					"port":     9443,
					"nodePort": CentralBrokerNodePort,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("encode service patch: %w", err)
	}
	return runCommand(ctx, kubectl, withKubeconfig(kubeconfig,
		"patch", "service", "broker",
		"--namespace", BrokerNamespace,
		"--type", "strategic",
		"--patch", string(patch),
	)...)
}

// repoRoot resolves to the repo root by walking up from this source
// file. Cheap, deterministic, no env vars.
func repoRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("runtime.Caller failed")
	}
	// thisFile = .../test/e2e/bootstrap/overlays.go
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..")), nil
}
