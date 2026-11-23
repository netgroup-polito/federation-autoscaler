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
	"time"

	"github.com/netgroup-polito/federation-autoscaler/test/e2e/kind"
)

// LiqoOptions configures InstallLiqo.
type LiqoOptions struct {
	// Kubeconfig is the path to the kubeconfig of the target cluster.
	// Required.
	Kubeconfig string

	// ClusterName is the Kind cluster name (e.g. "fa-consumer-1"). Used
	// by `liqoctl install kind` to derive its networking defaults.
	// Required.
	ClusterName string

	// Identity carries the federation-autoscaler-facing cluster id; we
	// pass it through to Liqo via --cluster-id so Liqo's own identity
	// matches docs/design.md §7.0's invariant.
	Identity Identity

	// LiqoctlBinary overrides the liqoctl executable. Empty resolves to
	// $LIQOCTL or "liqoctl" on $PATH.
	LiqoctlBinary string
}

// InstallLiqo runs `liqoctl install kind` against the target cluster.
// liqoctl is idempotent — a second invocation against an already-Liqo'd
// cluster verifies the install rather than re-running it. The e2e
// suite invokes this once per Kind cluster after kind.Bootstrap.
//
// NOTE: liqoctl must be on $PATH or via $LIQOCTL. The agent Docker
// image already bundles liqoctl (see Dockerfile, step 12a), but the
// suite runs liqoctl from the host — install it locally before
// running the e2e tests.
func InstallLiqo(ctx context.Context, opts LiqoOptions) error {
	switch {
	case opts.Kubeconfig == "":
		return fmt.Errorf("InstallLiqo: Kubeconfig %w", errEmpty)
	case opts.ClusterName == "":
		return fmt.Errorf("InstallLiqo: ClusterName %w", errEmpty)
	case opts.Identity.ClusterID == "":
		return fmt.Errorf("InstallLiqo: Identity.ClusterID %w", errEmpty)
	}
	liqoctl, err := resolveBinary(opts.LiqoctlBinary, envLiqoctl, defaultLiqoctlBin)
	if err != nil {
		return err
	}

	args := withKubeconfig(opts.Kubeconfig,
		"install", "kind",
		"--cluster-id", opts.Identity.LiqoClusterID,
		"--timeout", "10m",
	)
	// Retry a few times: right after a Kind cluster comes up, liqoctl's first
	// call to its API server can time out ("Unable to connect to the server:
	// context deadline exceeded") although the same install works seconds
	// later (seen on the lab server, 23 Nov 2026). liqoctl install is
	// idempotent, so a retry is safe. Same idea as retry_liqo_install in
	// deploy/standalone/common.sh.
	var lastErr error
	for attempt := 1; attempt <= liqoInstallAttempts; attempt++ {
		if lastErr = runCommand(ctx, liqoctl, args...); lastErr == nil {
			return nil
		}
		if attempt < liqoInstallAttempts {
			select {
			case <-ctx.Done():
				return fmt.Errorf("liqoctl install: %w (last error: %v)", ctx.Err(), lastErr)
			case <-time.After(liqoInstallBackoff):
			}
		}
	}
	return fmt.Errorf("liqoctl install (%d attempts): %w", liqoInstallAttempts, lastErr)
}

// liqoInstallAttempts and liqoInstallBackoff bound the retry of InstallLiqo.
const (
	liqoInstallAttempts = 3
	liqoInstallBackoff  = 20 * time.Second
)

// InstallLiqoForRole is a convenience wrapper that derives the
// Identity + ClusterName from a kind.Role and delegates to InstallLiqo.
func InstallLiqoForRole(ctx context.Context, role kind.Role, kubeconfig string) error {
	return InstallLiqo(ctx, LiqoOptions{
		Kubeconfig:  kubeconfig,
		ClusterName: kind.ClusterName(role),
		Identity:    IdentityForRole(role),
	})
}
