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
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/poller"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// Default values used by NewPeerHandler when the corresponding
// PeerConfig field is unset.
const (
	DefaultPeerLiqoctlPath = "liqoctl"
	// DefaultPeerExecTimeout is the wall-clock cap on a single
	// `liqoctl peer` invocation. The command does substantial
	// orchestration work — pulling and starting Liqo gateway Pods on
	// both sides, waiting for the gateway Service to be created, the
	// gateway Pod to be Ready, the WireGuard tunnel to establish, and
	// the foreign cluster's Identity / Tenant CRs to materialize. On
	// constrained CI hosts (CrownLabs, Kind-on-VM) those steps
	// regularly take 3-5 minutes; we used to use 90s and hit
	// `signal: killed` mid-handshake right when "Waiting for gateway
	// server Service to be created" was running. 10 minutes gives
	// realistic headroom without masking a genuine hang.
	DefaultPeerExecTimeout = 10 * time.Minute
)

// PeerConfig configures the Peer handler.
type PeerConfig struct {
	// LocalClient is the consumer-cluster k8s client. Used to persist
	// the kubeconfig Secret and create the Liqo CRs. Required.
	LocalClient ctrlclient.Client

	// Namespace is where the kubeconfig Secret and Liqo CRs are
	// created. Required.
	Namespace string

	// LiqoctlPath overrides the path to the liqoctl binary. Empty
	// falls back to "liqoctl" (resolved via $PATH).
	LiqoctlPath string

	// ExecTimeout caps how long the handler waits for liqoctl peer to
	// exit. Defaults to DefaultPeerExecTimeout.
	ExecTimeout time.Duration

	// Logger is the structured logger the handler logs through.
	Logger logr.Logger

	// Run is the exec hook. Production code leaves this nil so a real
	// os/exec.Cmd is built. Tests inject a fake so they do not spawn
	// liqoctl processes.
	Run RunFunc
}

// NewPeerHandler returns a poller.HandlerFunc that:
//  1. persists the inlined kubeconfig from InstructionView.Kubeconfig
//     to a local Secret kubeconfig-<resv>;
//  2. writes the kubeconfig to a temp file and runs
//     `liqoctl peer --remote-kubeconfig <path>`;
//  3. creates a Liqo ResourceSlice claiming the reservation's resources;
//  4. materialises a VirtualNodeState CR so the gRPC server starts
//     advertising the chunk.
//
// The Liqo NamespaceOffloading is intentionally NOT created here — it
// is a per-K8s-namespace singleton (Liqo's webhook hardcodes the name
// to "offloading") that operators stamp once per workload namespace
// (see deploy/ansible/roles/fa_consumer for the `default` namespace
// bootstrap). Creating it per-Reservation would race with sibling
// Reservations targeting the same namespace.
//
// On success the handler returns a Succeeded result with payload
// {Kind: PeerPayload, ResourceSliceNames: […]}. VirtualNodeNames is
// left empty in v1; the VirtualNodeStateReconciler surfaces those
// names once Liqo finishes setting up the virtual node.
func NewPeerHandler(cfg PeerConfig) poller.HandlerFunc {
	if cfg.LiqoctlPath == "" {
		cfg.LiqoctlPath = DefaultPeerLiqoctlPath
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = DefaultPeerExecTimeout
	}
	if cfg.Run == nil {
		cfg.Run = defaultRunFunc
	}
	if cfg.Logger.GetSink() == nil {
		cfg.Logger = log.Log.WithName("consumer-handler-peer")
	}

	return func(ctx context.Context, in *brokerapi.InstructionView) (*brokerapi.InstructionResultRequest, error) {
		if in == nil {
			return nil, errors.New("nil instruction")
		}
		if in.Kind != string(autoscalingv1alpha1.ReservationInstructionPeer) {
			return nil, fmt.Errorf("unexpected kind %q (want %s)",
				in.Kind, autoscalingv1alpha1.ReservationInstructionPeer)
		}
		if cfg.LocalClient == nil {
			return nil, errors.New("peer handler: LocalClient is nil")
		}
		if cfg.Namespace == "" {
			return nil, errors.New("peer handler: Namespace is required")
		}
		if in.Kubeconfig == "" {
			return nil, errors.New("instruction missing inline kubeconfig")
		}
		if in.ProviderLiqoClusterID == "" {
			return nil, errors.New("instruction missing providerLiqoClusterId")
		}

		logger := cfg.Logger.WithValues(
			"reservationId", in.ReservationID,
			"provider", in.ProviderClusterID)

		// 1. Persist the kubeconfig Secret.
		if err := persistKubeconfig(ctx, cfg.LocalClient, cfg.Namespace, in.ReservationID, in.Kubeconfig); err != nil {
			return nil, fmt.Errorf("persist kubeconfig: %w", err)
		}

		// 2. Write the kubeconfig to a temp file and run `liqoctl peer`.
		kubeconfigPath, cleanup, err := writeKubeconfigToTempFile(in.ReservationID, in.Kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("stage kubeconfig: %w", err)
		}
		defer cleanup()

		execCtx, cancelExec := context.WithTimeout(ctx, cfg.ExecTimeout)
		defer cancelExec()
		// Force NodePort for the provider-side gateway-server Service.
		// Liqo's default is LoadBalancer, which on Kind / on-prem
		// clusters without an LB provisioner leaves the Service's
		// EXTERNAL-IP stuck in <pending> forever. liqoctl peer then
		// hangs at "Waiting for gateway server Service to be created"
		// even though the Service technically exists — it never gets
		// an externalIP and the wait loop times out. NodePort sidesteps
		// the LB entirely; the underlying Pod gets a stable nodePort
		// that the consumer side reaches via the shared kind docker
		// network. See liqoctl peer --gw-server-service-type flag.
		args := []string{
			"peer",
			"--remote-kubeconfig", kubeconfigPath,
			"--gw-server-service-type", "NodePort",
		}
		logger.V(1).Info("running liqoctl", "path", cfg.LiqoctlPath, "args", args)
		_, stderr, err := cfg.Run(execCtx, cfg.LiqoctlPath, args...)
		if err != nil {
			trimmed := strings.TrimSpace(string(stderr))
			if trimmed == "" {
				trimmed = "(no stderr)"
			}
			return nil, fmt.Errorf("liqoctl peer failed: %w — stderr: %s", err, trimmed)
		}

		// 3. Create the ResourceSlice claiming the reservation's chunks.
		sliceName, err := ensureResourceSlice(
			ctx, cfg.LocalClient, cfg.Namespace, in.ReservationID,
			in.ProviderLiqoClusterID, in.ResourceSliceResources)
		if err != nil {
			return nil, err
		}

		// 4. (NamespaceOffloading is intentionally NOT created here.)
		// Liqo NamespaceOffloading is a per-K8s-namespace singleton named
		// literally "offloading" — one per workload namespace, shared by
		// every Reservation that targets that namespace. The operator
		// owns this CR, not the agent: the agent doesn't know which
		// namespaces should be offload-enabled, and creating it from the
		// agent's namespace (federation-autoscaler-system) instead of
		// the workload's namespace would silently disable Pod reflection.
		// See deploy/ansible's fa_consumer role for the bootstrap-time
		// creation of the NSO in `default`; users with workloads in
		// other namespaces stamp their own.

		// 5. Materialise the VirtualNodeState CR. This is what the
		// gRPC server consumes via /local/virtual-nodes; the
		// VirtualNodeStateReconciler then projects the Liqo
		// VirtualNode's status onto it as Liqo materialises the node.
		if err := ensureVirtualNodeState(ctx, cfg.LocalClient, cfg.Namespace, in); err != nil {
			return nil, err
		}

		logger.V(1).Info("peer complete", "resourceSlice", sliceName)
		return &brokerapi.InstructionResultRequest{
			Status: brokerapi.ResultStatusSucceeded,
			Payload: &brokerapi.ResultPayload{
				Kind:               brokerapi.PayloadKindPeer,
				ResourceSliceNames: []string{sliceName},
				// VirtualNodeNames stays empty here: Liqo materialises
				// the VirtualNode asynchronously, and the
				// VirtualNodeStateReconciler is the canonical place
				// where those names show up.
			},
		}, nil
	}
}
