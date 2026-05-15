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

// Package scenario contains the happy-path assertions the step-13 e2e
// suite drives across the four Kind clusters: unschedulable Pods on the
// consumer cluster → Reservation Pending→Peered on the central cluster
// → Liqo VirtualNode Ready on the consumer cluster → Pods Scheduled.
//
// The helpers here are plain Go functions; step 13e wraps them in
// Ginkgo specs once the suite-level orchestration lands.
package scenario

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// resolveKubectl mirrors the bootstrap package's binary-resolution
// order so callers can override via $KUBECTL.
func resolveKubectl(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if v := os.Getenv("KUBECTL"); v != "" {
		return v, nil
	}
	if path, err := exec.LookPath("kubectl"); err == nil {
		return path, nil
	}
	return "", errors.New("kubectl binary not found; install kubectl on $PATH or set $KUBECTL")
}

// runKubectl invokes kubectl with --kubeconfig=<path> prepended and
// returns its combined stdout/stderr; errors include the full command
// line + trimmed output for diagnosis.
func runKubectl(ctx context.Context, kubectl, kubeconfig string, args ...string) (string, error) {
	full := append([]string{"--kubeconfig=" + kubeconfig}, args...)
	cmd := exec.CommandContext(ctx, kubectl, full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("kubectl %s: %w: %s",
			strings.Join(full, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// errEmpty is returned when a required argument is missing.
var errEmpty = errors.New("required argument is empty")

// WaitOptions bounds a polling loop. Sensible defaults: 5 m timeout,
// 5 s interval — long enough for Liqo to materialise the VirtualNode
// even in a CI-sized Kind cluster.
type WaitOptions struct {
	Timeout  time.Duration
	Interval time.Duration
}

const (
	// DefaultTimeout is the per-step ceiling. Reservation Peered →
	// VirtualNode Ready takes ~1 m on a healthy host; 5 m absorbs
	// pull-image stalls and slow Liqo handshakes.
	DefaultTimeout = 5 * time.Minute

	// DefaultInterval is the poll cadence. Fast enough to keep tests
	// snappy, slow enough not to thrash the apiserver.
	DefaultInterval = 5 * time.Second
)

// withDefaults fills in WaitOptions.Timeout / Interval when zero.
func (o WaitOptions) withDefaults() WaitOptions {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	return o
}

// waitUntil runs check on Interval ticks until it returns (true, nil),
// Timeout fires, or ctx cancels. A non-nil error from check does NOT
// abort — many kubectl calls fail transiently while a resource is
// being created — but the last error is reported on timeout.
func waitUntil(ctx context.Context, opts WaitOptions, check func() (bool, error)) error {
	opts = opts.withDefaults()
	deadline := time.Now().Add(opts.Timeout)
	var lastErr error
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ok, err := check()
		if err != nil {
			lastErr = err
		} else if ok {
			return nil
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("timed out after %s: %w", opts.Timeout, lastErr)
			}
			return fmt.Errorf("timed out after %s", opts.Timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opts.Interval):
		}
	}
}
