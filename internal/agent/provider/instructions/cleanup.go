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

package instructions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/poller"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// DefaultCleanupTimeout caps the time a Cleanup invocation may take.
// Half the GenerateKubeconfig budget — deletion is much cheaper than
// the full peering-user mint/grant cycle.
const DefaultCleanupTimeout = 30 * time.Second

// notFoundMarkers are substrings the handler treats as a successful
// cleanup, not a failure. They indicate the underlying resources were
// already gone, which is exactly the idempotency guarantee the broker
// relies on when it re-issues a Cleanup after a transient network
// failure.
var notFoundMarkers = []string{
	"not found",
	"does not exist",
	"NotFound",
}

// CleanupConfig configures the Cleanup handler.
type CleanupConfig struct {
	// LiqoctlPath is the path to the liqoctl binary. Empty falls back
	// to DefaultLiqoctlPath ("liqoctl"); resolved via $PATH.
	LiqoctlPath string

	// ExecTimeout caps how long the handler waits for liqoctl to exit.
	// Defaults to DefaultCleanupTimeout.
	ExecTimeout time.Duration

	// Logger is the structured logger the handler logs through.
	Logger logr.Logger

	// Run is the exec hook. Production code leaves it nil so a real
	// os/exec.Cmd is built; tests inject a fake.
	Run RunFunc
}

// NewCleanupHandler returns a poller.HandlerFunc that runs
// `liqoctl delete peering-user --consumer-cluster-id <id>` to tear down
// the ServiceAccount/RoleBinding the matching GenerateKubeconfig
// instruction created. The broker queues this instruction whenever a
// Reservation enters a terminal phase (Released / Failed / Expired),
// see internal/controller/broker/reservation_controller.go's
// handleTerminal.
//
// Idempotency: if liqoctl reports the resources are already gone
// (stderr matches a notFoundMarker), the handler returns Succeeded so
// retries after a successful first run do not falsely re-fail the
// reservation.
//
// On hard failures (non-zero exit + unrecognised stderr) the handler
// returns a non-nil error; the poller wraps that into a Failed result
// envelope.
func NewCleanupHandler(cfg CleanupConfig) poller.HandlerFunc {
	if cfg.LiqoctlPath == "" {
		cfg.LiqoctlPath = DefaultLiqoctlPath
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = DefaultCleanupTimeout
	}
	if cfg.Run == nil {
		cfg.Run = defaultRunFunc
	}
	if cfg.Logger.GetSink() == nil {
		cfg.Logger = log.Log.WithName("provider-handler-cleanup")
	}

	return func(ctx context.Context, in *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		if in == nil {
			return nil, errors.New("nil instruction")
		}
		if in.Kind != string(autoscalingv1alpha1.ProviderInstructionCleanup) {
			return nil, fmt.Errorf("unexpected kind %q (want %s)",
				in.Kind, autoscalingv1alpha1.ProviderInstructionCleanup)
		}
		if in.ConsumerClusterID == "" {
			return nil, errors.New("instruction is missing consumerClusterId")
		}

		execCtx, cancel := context.WithTimeout(ctx, cfg.ExecTimeout)
		defer cancel()

		args := []string{"delete", "peering-user", "--consumer-cluster-id", in.ConsumerClusterID}
		cfg.Logger.V(1).Info("running liqoctl",
			"path", cfg.LiqoctlPath, "args", args, "reservationId", in.ReservationID)

		_, stderr, err := cfg.Run(execCtx, cfg.LiqoctlPath, args...)
		if err == nil {
			return succeededNoPayload(), nil
		}

		// Idempotent recovery: a "not found" stderr is a successful
		// noop, not a failure.
		trimmed := strings.TrimSpace(string(stderr))
		if isNotFound(trimmed) {
			cfg.Logger.V(1).Info("cleanup target already gone — treated as success",
				"reservationId", in.ReservationID, "stderr", trimmed)
			return succeededNoPayload(), nil
		}

		if trimmed == "" {
			trimmed = "(no stderr)"
		}
		return nil, fmt.Errorf("liqoctl delete peering-user failed: %w — stderr: %s", err, trimmed)
	}
}

// succeededNoPayload returns the canonical "Succeeded with no payload"
// envelope used by Cleanup (and any future handler whose result has no
// kind-specific data).
func succeededNoPayload() *brokerapi.InstructionResultRequest {
	return &brokerapi.InstructionResultRequest{Status: brokerapi.ResultStatusSucceeded}
}

// isNotFound reports whether stderr matches any of the notFoundMarkers.
// Substring match is intentional — liqoctl's exact phrasing changes
// across versions, so we look for the smallest stable substring.
func isNotFound(stderr string) bool {
	for _, m := range notFoundMarkers {
		if strings.Contains(stderr, m) {
			return true
		}
	}
	return false
}
