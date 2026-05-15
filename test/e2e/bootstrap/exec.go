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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Binary names resolved via $-env-var / $PATH lookup. The naming mirrors
// the Makefile's KIND ?= kind convention so operators can override any
// of them at invocation time.
const (
	envKubectl        = "KUBECTL"
	envLiqoctl        = "LIQOCTL"
	defaultKubectlBin = "kubectl"
	defaultLiqoctlBin = "liqoctl"
)

// resolveBinary mirrors kind.resolveKindBinary: explicit > $envVar > $PATH.
func resolveBinary(explicit, envVar, defaultBin string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if v := os.Getenv(envVar); v != "" {
		return v, nil
	}
	if path, err := exec.LookPath(defaultBin); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("%s binary not found; install it and ensure it's on $PATH, or set $%s",
		defaultBin, envVar)
}

// runCommand wraps exec.CommandContext + CombinedOutput with a uniform
// error format that includes the command line and stderr (kubectl /
// liqoctl exit codes alone are useless for diagnosis).
func runCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s",
			name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// withKubeconfig prepends --kubeconfig=<path> to args. The kubectl /
// liqoctl conventions both accept this flag; we use the flag form
// rather than the KUBECONFIG env var so concurrent installs against
// different clusters don't race on a shared environment.
func withKubeconfig(kubeconfig string, args ...string) []string {
	out := make([]string, 0, len(args)+1)
	out = append(out, "--kubeconfig="+kubeconfig)
	out = append(out, args...)
	return out
}

// errEmpty is returned when a required argument is missing.
var errEmpty = errors.New("required argument is empty")
