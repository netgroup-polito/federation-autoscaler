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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/poller"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// DefaultUnpeerExecTimeout caps `liqoctl unpeer`. Tear-down is cheaper
// than full peer setup but Liqo still needs to drain the virtual node.
const DefaultUnpeerExecTimeout = 60 * time.Second

// UnpeerConfig configures the Unpeer handler.
type UnpeerConfig struct {
	// LocalClient is the consumer-cluster k8s client. Required.
	LocalClient ctrlclient.Client

	// Namespace where the kubeconfig Secret and Liqo CRs live. Required.
	Namespace string

	// LiqoctlPath overrides the path to the liqoctl binary. Empty
	// falls back to "liqoctl" (resolved via $PATH).
	LiqoctlPath string

	// ExecTimeout caps how long the handler waits for liqoctl unpeer
	// to exit. Defaults to DefaultUnpeerExecTimeout.
	ExecTimeout time.Duration

	// Logger is the structured logger the handler logs through.
	Logger logr.Logger

	// Run is the exec hook. Production code leaves this nil so a real
	// os/exec.Cmd is built; tests inject a fake.
	Run RunFunc
}

// NewUnpeerHandler returns a poller.HandlerFunc that tears down what
// the Peer handler set up. Execution order is the reverse of Peer:
//  1. Delete the VirtualNodeState CR (so the gRPC server stops
//     advertising the chunk before Liqo drops the underlying node).
//  2. Delete the Liqo ResourceSlice.
//  3. Run `liqoctl unpeer --remote-kubeconfig <path>` so the cross-
//     cluster tunnel drops.
//  4. On LastChunk=true, also delete the kubeconfig Secret — the
//     reservation is fully released and we should not keep the
//     peering-user credential lying around.
//
// NamespaceOffloading is intentionally NOT touched — it is the per-
// K8s-namespace singleton the operator owns (see peer.go).
//
// All four steps are idempotent on missing so a re-issued Unpeer (or a
// rebooted agent re-fetching the same instruction) does not falsely
// fail the reservation.
func NewUnpeerHandler(cfg UnpeerConfig) poller.HandlerFunc {
	if cfg.LiqoctlPath == "" {
		cfg.LiqoctlPath = DefaultPeerLiqoctlPath
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = DefaultUnpeerExecTimeout
	}
	if cfg.Run == nil {
		cfg.Run = defaultRunFunc
	}
	if cfg.Logger.GetSink() == nil {
		cfg.Logger = log.Log.WithName("consumer-handler-unpeer")
	}

	return func(ctx context.Context, in *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		if in == nil {
			return nil, errors.New("nil instruction")
		}
		if in.Kind != string(autoscalingv1alpha1.ReservationInstructionUnpeer) {
			return nil, fmt.Errorf("unexpected kind %q (want %s)",
				in.Kind, autoscalingv1alpha1.ReservationInstructionUnpeer)
		}
		if cfg.LocalClient == nil {
			return nil, errors.New("unpeer handler: LocalClient is nil")
		}
		if cfg.Namespace == "" {
			return nil, errors.New("unpeer handler: Namespace is required")
		}

		logger := cfg.Logger.WithValues(
			"reservationId", in.ReservationID,
			"lastChunk", in.LastChunk)

		// 1. Delete the VirtualNodeState CR (idempotent on missing).
		// The order matters: tearing the VNS first removes the chunk
		// from the gRPC server's view of the cluster before Liqo
		// drops the underlying virtual node, so CA sees the deletion
		// rather than a Ready→NotReady flap.
		if err := deleteVirtualNodeState(ctx, cfg.LocalClient, cfg.Namespace, in.ReservationID); err != nil {
			return nil, err
		}

		// 2. Delete ResourceSlice (idempotent on missing).
		if err := deleteResourceSlice(ctx, cfg.LocalClient, cfg.Namespace, in.ReservationID); err != nil {
			return nil, err
		}

		// 3. Stage the kubeconfig and run `liqoctl unpeer` — only if
		//    the kubeconfig Secret still exists; without it liqoctl
		//    cannot reach the provider and the cross-cluster tunnel
		//    has presumably already dropped.
		kubeconfig, err := loadKubeconfigFromSecret(ctx, cfg.LocalClient, cfg.Namespace, in.ReservationID)
		switch {
		case err != nil:
			return nil, err
		case kubeconfig == "":
			logger.V(1).Info("no kubeconfig secret present; skipping liqoctl unpeer")
		default:
			kubeconfigPath, cleanup, err := writeKubeconfigToTempFile(in.ReservationID, kubeconfig)
			if err != nil {
				return nil, fmt.Errorf("stage kubeconfig: %w", err)
			}
			defer cleanup()

			execCtx, cancelExec := context.WithTimeout(ctx, cfg.ExecTimeout)
			defer cancelExec()
			args := []string{"unpeer", "--remote-kubeconfig", kubeconfigPath}
			logger.V(1).Info("running liqoctl", "path", cfg.LiqoctlPath, "args", args)
			_, stderr, err := cfg.Run(execCtx, cfg.LiqoctlPath, args...)
			if err != nil {
				trimmed := strings.TrimSpace(string(stderr))
				// "not peered" / "already unpeered" stderr is treated
				// as success — Liqo's exact phrasing varies across
				// versions, so we match the smallest stable substring.
				if !looksAlreadyUnpeered(trimmed) {
					if trimmed == "" {
						trimmed = "(no stderr)"
					}
					return nil, fmt.Errorf("liqoctl unpeer failed: %w — stderr: %s", err, trimmed)
				}
				logger.V(1).Info("liqoctl reports already unpeered — treated as success", "stderr", trimmed)
			}
		}

		// 4. On LastChunk=true, finish tearing down the peering: wipe the
		//    kubeconfig Secret and delete the ForeignCluster shell that
		//    `liqoctl unpeer` leaves behind (otherwise `liqoctl info` keeps
		//    listing the peering with Authentication Healthy indefinitely —
		//    unpeer disables the modules + deletes the Identity/Tenant but
		//    never the ForeignCluster object itself). Gated on LastChunk
		//    because the ForeignCluster is shared by all chunks to the same
		//    provider; v1 always releases the whole reservation at once
		//    (LastChunk hardcoded true), so this is the point at which the
		//    provider is fully released.
		if in.LastChunk {
			if err := deleteKubeconfigSecret(ctx, cfg.LocalClient, cfg.Namespace, in.ReservationID); err != nil {
				return nil, err
			}
			if in.ProviderLiqoClusterID != "" {
				if err := deleteForeignCluster(ctx, cfg.LocalClient, in.ProviderLiqoClusterID); err != nil {
					return nil, err
				}
				logger.V(1).Info("deleted ForeignCluster shell", "foreignCluster", in.ProviderLiqoClusterID)
			}
		}

		return &brokerapi.InstructionResultRequest{
			Status: brokerapi.ResultStatusSucceeded,
			Payload: &brokerapi.ResultPayload{
				Kind:           brokerapi.PayloadKindUnpeer,
				ReleasedChunks: in.ChunkCount,
				TunnelDropped:  in.LastChunk,
			},
		}, nil
	}
}

// loadKubeconfigFromSecret reads the kubeconfig bytes from the Secret
// the Peer handler persisted. Returns ("", nil) when the Secret is
// missing — the Unpeer path treats that as "nothing to do for
// liqoctl".
func loadKubeconfigFromSecret(
	ctx context.Context, c ctrlclient.Client, namespace, reservationID string,
) (string, error) {
	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{
		Name: kubeconfigSecretName(reservationID), Namespace: namespace,
	}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("get kubeconfig secret: %w", err)
	}
	return string(sec.Data[KubeconfigSecretDataKey]), nil
}

// looksAlreadyUnpeered returns true when liqoctl's stderr indicates
// the tear-down was a no-op — used to make Unpeer idempotent on
// repeated invocations.
func looksAlreadyUnpeered(stderr string) bool {
	markers := []string{
		"not peered",
		"already unpeered",
		"no peering",
		"not found",
	}
	for _, m := range markers {
		if strings.Contains(stderr, m) {
			return true
		}
	}
	return false
}
