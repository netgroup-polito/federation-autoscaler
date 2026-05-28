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

// Package consumer implements the consumer-role behaviour of the agent
// (docs/design.md §4.3): a 15 s heartbeat poster, a loopback REST
// server consumed by the co-located gRPC server, and the four
// instruction handlers — Peer, Unpeer, Cleanup, Reconcile — the
// Broker queues against this consumer via GET /api/v1/instructions.
//
// Run is the package's only entry point: it registers the handlers on
// the shared poller.Registry, spawns the heartbeat loop and the
// loopback REST server, then returns. cmd/agent/main.go blocks on the
// shared poller's Run; the goroutines this package owns live alongside
// that loop until ctx is cancelled.
package consumer

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/heartbeat"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/instructions"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/health"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/poller"
)

// Options bundles the construction-time settings of the consumer role.
// Every field is required — Run validates the lot and returns a wrapped
// error rather than spawning a half-initialised goroutine.
type Options struct {
	// Client is the Broker HTTP client built in cmd/agent/main.go from
	// the agent's mTLS keypair (step 7a/7b).
	Client *agentclient.Client

	// Registry is the shared poller registry created in main.go. The
	// consumer populates it with the Peer / Unpeer / Cleanup /
	// Reconcile handlers (substeps 9d / 9e).
	Registry *poller.Registry

	// LocalClient is the controller-runtime client for the local
	// cluster's API server. Used by the Peer / Unpeer handlers
	// (substep 9d) to write Liqo ResourceSlice and NamespaceOffloading
	// CRs, and by the local-API server (substep 9c) to read
	// VirtualNodeState CRs.
	LocalClient client.Client

	// ClusterID is the agent's broker-facing identifier and MUST equal
	// the CN of the mTLS client certificate (docs/design.md §10.3).
	ClusterID string

	// LiqoClusterID is the Liqo-side identifier passed to liqoctl peer
	// when the consumer joins a provider.
	LiqoClusterID string

	// LiqoctlPath overrides the path to the liqoctl binary the
	// Peer / Unpeer handlers shell out to. Empty falls back to
	// "liqoctl" (resolved via $PATH).
	LiqoctlPath string

	// LocalAPIAddr is the address the loopback REST server (substep
	// 9c) binds to. The gRPC server in step 10 dials this directly;
	// production wiring keeps it on 127.0.0.1 so the listener is
	// exposed to the co-located pod only.
	LocalAPIAddr string

	// Namespace is where the consumer agent creates the kubeconfig
	// Secret, the Liqo ResourceSlice, and the VirtualNodeState CR, and
	// where the loopback API lists VirtualNodeState from. It is the
	// agent's own deployment namespace (federation-autoscaler-system),
	// injected via --namespace / POD_NAMESPACE. Defaults to
	// "federation-autoscaler-system" when unset.
	//
	// NOTE: this is deliberately NOT "liqo". The ResourceSlice works in
	// any namespace (Liqo's controller watches cluster-wide and our spec
	// sets providerClusterID, so the tenant-namespace derivation is
	// skipped); keeping the federation-autoscaler-owned VirtualNodeState
	// out of the liqo namespace avoids confusing it with Liqo's own CRs
	// during triage.
	Namespace string

	// Logger is the structured logger every consumer goroutine logs
	// through. Defaults to controller-runtime's logger named "consumer".
	Logger logr.Logger

	// Probe is the shared liveness/readiness gate. The heartbeat poster
	// (substep 9b) calls Probe.RecordPoll after every heartbeat so a
	// stalled heartbeat reddens /readyz alongside a stalled instruction
	// poll.
	Probe *health.Probe
}

// Run wires the consumer-role instruction handlers onto opts.Registry
// and starts the heartbeat poster (and, in subsequent substeps, the
// loopback REST server) in background goroutines. It returns as soon
// as everything is bootstrapped; the goroutines outlive the call and
// exit when ctx is cancelled.
//
// 9a installed the skeleton; 9b added the heartbeat poster; 9c will
// add the loopback REST server; 9d/9e the handlers.
func Run(ctx context.Context, opts Options) error {
	if err := validate(opts); err != nil {
		return err
	}
	logger := opts.Logger
	if logger.GetSink() == nil {
		logger = log.Log.WithName("consumer")
	}
	namespace := opts.Namespace
	if namespace == "" {
		namespace = "federation-autoscaler-system"
	}

	opts.Registry.RegisterReservation(
		autoscalingv1alpha1.ReservationInstructionPeer,
		instructions.NewPeerHandler(instructions.PeerConfig{
			LocalClient: opts.LocalClient,
			Namespace:   namespace,
			LiqoctlPath: opts.LiqoctlPath,
			Logger:      logger.WithName("peer"),
		}),
	)
	opts.Registry.RegisterReservation(
		autoscalingv1alpha1.ReservationInstructionUnpeer,
		instructions.NewUnpeerHandler(instructions.UnpeerConfig{
			LocalClient: opts.LocalClient,
			Namespace:   namespace,
			LiqoctlPath: opts.LiqoctlPath,
			Logger:      logger.WithName("unpeer"),
		}),
	)
	opts.Registry.RegisterReservation(
		autoscalingv1alpha1.ReservationInstructionCleanup,
		instructions.NewCleanupHandler(instructions.CleanupConfig{
			LocalClient: opts.LocalClient,
			Namespace:   namespace,
			Logger:      logger.WithName("cleanup"),
		}),
	)
	opts.Registry.RegisterReservation(
		autoscalingv1alpha1.ReservationInstructionReconcile,
		instructions.NewReconcileHandler(instructions.ReconcileConfig{
			LocalClient: opts.LocalClient,
			Namespace:   namespace,
			Logger:      logger.WithName("reconcile"),
		}),
	)

	beater, err := heartbeat.New(heartbeat.Options{
		Client:        opts.Client,
		ClusterID:     opts.ClusterID,
		LiqoClusterID: opts.LiqoClusterID,
		Logger:        logger.WithName("heartbeat"),
		// Same semantics as the provider's advertisement publisher:
		// any successful broker contact refreshes the readiness gate.
		OnHeartbeatResult: opts.Probe.RecordPoll,
	})
	if err != nil {
		return fmt.Errorf("consumer: build heartbeat poster: %w", err)
	}
	go beater.Run(ctx)

	localServer, err := localapi.New(localapi.Options{
		BindAddress: opts.LocalAPIAddr,
		Client:      opts.Client,
		LocalClient: opts.LocalClient,
		Namespace:   namespace,
		Logger:      logger.WithName("localapi"),
	})
	if err != nil {
		return fmt.Errorf("consumer: build loopback REST server: %w", err)
	}
	go func() {
		if err := localServer.Run(ctx); err != nil {
			logger.Error(err, "loopback REST server stopped with error")
		}
	}()

	logger.Info("consumer role bootstrapped",
		"clusterID", opts.ClusterID,
		"liqoClusterID", opts.LiqoClusterID,
		"localAPIAddr", opts.LocalAPIAddr,
		"handlers", opts.Registry.Len())
	return nil
}

// validate enforces the required-options contract. Returning an error
// early is the right failure mode: a misconfigured agent should refuse
// to start rather than spawn goroutines that crash on first use.
func validate(o Options) error {
	switch {
	case o.Client == nil:
		return errors.New("consumer: Client is required")
	case o.Registry == nil:
		return errors.New("consumer: Registry is required")
	case o.LocalClient == nil:
		return errors.New("consumer: LocalClient is required")
	case o.ClusterID == "":
		return errors.New("consumer: ClusterID is required")
	case o.LiqoClusterID == "":
		return errors.New("consumer: LiqoClusterID is required")
	case o.LocalAPIAddr == "":
		return errors.New("consumer: LocalAPIAddr is required")
	case o.Probe == nil:
		return errors.New("consumer: Probe is required")
	}
	return nil
}
