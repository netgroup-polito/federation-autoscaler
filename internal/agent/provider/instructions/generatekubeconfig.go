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

// Default values used by NewGenerateKubeconfigHandler when the
// corresponding GenerateKubeconfigConfig field is unset.
const (
	DefaultLiqoctlPath               = "liqoctl"
	DefaultGenerateKubeconfigTimeout = 60 * time.Second
)

// GenerateKubeconfigConfig configures the GenerateKubeconfig handler.
// Empty values fall back to the constants above.
type GenerateKubeconfigConfig struct {
	// LiqoctlPath is the path to the liqoctl binary. Empty falls back
	// to DefaultLiqoctlPath ("liqoctl"), which os/exec resolves through
	// $PATH — that is the production posture: we ship liqoctl in the
	// agent image so it lives on the standard search path.
	LiqoctlPath string

	// ExecTimeout caps how long the handler waits for liqoctl to exit.
	// Defaults to DefaultGenerateKubeconfigTimeout. Hard-killed via
	// context after the deadline elapses.
	ExecTimeout time.Duration

	// Logger is the structured logger the handler logs through.
	Logger logr.Logger

	// Run is the exec hook. Production code leaves this nil so a real
	// os/exec.Cmd is built. Tests inject a fake so they do not spawn
	// liqoctl processes.
	Run RunFunc
}

// NewGenerateKubeconfigHandler returns a poller.HandlerFunc that runs
// `liqoctl generate peering-user --consumer-cluster-id <id>` and
// reports the captured stdout as the kubeconfig payload (docs/design.md
// §7.3.7, §8.2).
//
// On success the handler returns a Succeeded result with payload
// {Kind: KubeconfigPayload, Kubeconfig: <stdout-bytes>} — the broker
// stores the bytes verbatim into a Secret named kubeconfig-<resv> and
// inlines them on the matching Peer instruction it issues to the
// consumer.
//
// On failure (liqoctl non-zero exit, empty stdout, missing
// consumerClusterId in the instruction) the handler returns a non-nil
// error; the poller wraps that into a Failed result envelope with
// ErrCodeInternalError so the broker fails the parent Reservation.
func NewGenerateKubeconfigHandler(cfg GenerateKubeconfigConfig) poller.HandlerFunc {
	if cfg.LiqoctlPath == "" {
		cfg.LiqoctlPath = DefaultLiqoctlPath
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = DefaultGenerateKubeconfigTimeout
	}
	if cfg.Run == nil {
		cfg.Run = defaultRunFunc
	}
	if cfg.Logger.GetSink() == nil {
		cfg.Logger = log.Log.WithName("provider-handler-gk")
	}

	return func(ctx context.Context, in *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		if in == nil {
			return nil, errors.New("nil instruction")
		}
		if in.Kind != string(autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig) {
			return nil, fmt.Errorf("unexpected kind %q (want %s)",
				in.Kind, autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig)
		}
		if in.ConsumerClusterID == "" {
			return nil, errors.New("instruction is missing consumerClusterId")
		}

		execCtx, cancel := context.WithTimeout(ctx, cfg.ExecTimeout)
		defer cancel()

		args := []string{"generate", "peering-user", "--consumer-cluster-id", in.ConsumerClusterID}
		cfg.Logger.V(1).Info("running liqoctl",
			"path", cfg.LiqoctlPath, "args", args, "reservationId", in.ReservationID)

		stdout, stderr, err := cfg.Run(execCtx, cfg.LiqoctlPath, args...)
		if err != nil {
			// Surface stderr in the broker-facing error so operators
			// debugging a Failed reservation can see the underlying
			// liqoctl complaint.
			trimmed := strings.TrimSpace(string(stderr))
			if trimmed == "" {
				trimmed = "(no stderr)"
			}
			return nil, fmt.Errorf("liqoctl generate peering-user failed: %w — stderr: %s", err, trimmed)
		}

		kubeconfig := strings.TrimSpace(string(stdout))
		if kubeconfig == "" {
			return nil, errors.New("liqoctl returned empty kubeconfig")
		}

		return &brokerapi.InstructionResultRequest{
			Status: brokerapi.ResultStatusSucceeded,
			Payload: &brokerapi.ResultPayload{
				Kind:       brokerapi.PayloadKindKubeconfig,
				Kubeconfig: kubeconfig,
			},
		}, nil
	}
}
