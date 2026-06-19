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

// Package provider implements the provider-role behaviour of the agent
// (docs/design.md §4.4): a 30 s advertisement publisher that snapshots
// local-cluster Allocatable and posts it to the Broker, plus the three
// instruction handlers — GenerateKubeconfig, Cleanup, Reconcile — the
// Broker queues against this provider via GET /api/v1/instructions.
//
// Run is the package's only entry point: it registers the handlers on
// the shared poller.Registry and spawns the advertisement loop, then
// returns. cmd/agent/main.go blocks on the shared poller's Run; the
// publisher goroutine and the handlers it registers live alongside
// that loop until ctx is cancelled.
package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/health"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/poller"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/provider/advertise"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/provider/instructions"
)

// Options bundles the construction-time settings of the provider role.
// Every field is required — Run validates the lot and returns a wrapped
// error rather than spawning a half-initialised goroutine.
type Options struct {
	// Client is the Broker HTTP client built in cmd/agent/main.go from
	// the agent's mTLS keypair (step 7a/7b).
	Client *agentclient.Client

	// Registry is the shared poller registry created in main.go. The
	// provider populates it with the GenerateKubeconfig / Cleanup /
	// Reconcile handlers (substeps 8d / 8e).
	Registry *poller.Registry

	// LocalClient is the controller-runtime client for the local
	// cluster's API server. Used by the advertisement snapshot
	// (substep 8b) to read Node Allocatable, and by the Cleanup
	// handler (substep 8e) to delete the peering-user
	// ServiceAccount/RoleBinding the GenerateKubeconfig handler created.
	LocalClient client.Client

	// ClusterID is the agent's broker-facing identifier and MUST equal
	// the CN of the mTLS client certificate (docs/design.md §10.3).
	ClusterID string

	// LiqoClusterID is the Liqo-side identifier the broker propagates
	// to consumers when it issues `liqoctl peer` instructions.
	LiqoClusterID string

	// LiqoctlPath overrides the path to the liqoctl binary the
	// instruction handlers shell out to. Empty falls back to "liqoctl"
	// (resolved via $PATH).
	LiqoctlPath string

	// PriceFile is an optional path to this provider's per-resource unit-price
	// file, re-read on every advertisement cycle (see advertise.Options).
	// Empty ⇒ the provider advertises no price.
	PriceFile string

	// CapacityFile is an optional path to this provider's per-resource
	// advertised-capacity percentage file, re-read on every advertisement cycle
	// (see advertise.Options). Empty ⇒ the provider advertises full allocatable.
	CapacityFile string

	// Logger is the structured logger every provider goroutine logs
	// through. Defaults to controller-runtime's logger named "provider".
	Logger logr.Logger

	// Probe is the shared liveness/readiness gate. The advertisement
	// publisher (substep 8c) calls Probe.RecordPoll after every
	// advertisement so a stalled heartbeat reddens /readyz alongside a
	// stalled instruction poll.
	Probe *health.Probe
}

// Run wires the provider-role instruction handlers onto opts.Registry
// and starts the 30 s advertisement publisher in a background
// goroutine. It returns as soon as both are bootstrapped; the
// goroutines outlive the call and exit when ctx is cancelled.
//
// 8a installed the skeleton; 8c added the publisher; 8d/8e add the
// handlers.
func Run(ctx context.Context, opts Options) error {
	if err := validate(opts); err != nil {
		return err
	}
	logger := opts.Logger
	if logger.GetSink() == nil {
		logger = log.Log.WithName("provider")
	}

	opts.Registry.RegisterProvider(
		autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig,
		instructions.NewGenerateKubeconfigHandler(instructions.GenerateKubeconfigConfig{
			LiqoctlPath: opts.LiqoctlPath,
			Logger:      logger.WithName("gk"),
		}),
	)
	opts.Registry.RegisterProvider(
		autoscalingv1alpha1.ProviderInstructionCleanup,
		instructions.NewCleanupHandler(instructions.CleanupConfig{
			LiqoctlPath: opts.LiqoctlPath,
			Logger:      logger.WithName("cleanup"),
		}),
	)
	opts.Registry.RegisterProvider(
		autoscalingv1alpha1.ProviderInstructionReconcile,
		instructions.NewReconcileHandler(instructions.ReconcileConfig{
			LocalClient:   opts.LocalClient,
			ClusterID:     opts.ClusterID,
			LiqoClusterID: opts.LiqoClusterID,
			Logger:        logger.WithName("reconcile"),
		}),
	)

	publisher, err := advertise.New(advertise.Options{
		Client:        opts.Client,
		LocalClient:   opts.LocalClient,
		ClusterID:     opts.ClusterID,
		LiqoClusterID: opts.LiqoClusterID,
		PriceFile:     opts.PriceFile,
		CapacityFile:  opts.CapacityFile,
		Logger:        logger.WithName("advertise"),
		// The probe's poll-staleness gate is a "broker reachability"
		// signal in practice, so a successful advertisement also
		// refreshes it — same semantics as the poller's hook.
		OnPublishResult: opts.Probe.RecordPoll,
	})
	if err != nil {
		return fmt.Errorf("provider: build advertisement publisher: %w", err)
	}
	go publisher.Run(ctx)

	logger.Info("provider role bootstrapped",
		"clusterID", opts.ClusterID,
		"liqoClusterID", opts.LiqoClusterID,
		"handlers", opts.Registry.Len())
	return nil
}

// validate enforces the required-options contract. Returning an error
// early is the right failure mode: a misconfigured agent should refuse
// to start rather than spawn goroutines that crash on first use.
func validate(o Options) error {
	switch {
	case o.Client == nil:
		return errors.New("provider: Client is required")
	case o.Registry == nil:
		return errors.New("provider: Registry is required")
	case o.LocalClient == nil:
		return errors.New("provider: LocalClient is required")
	case o.ClusterID == "":
		return errors.New("provider: ClusterID is required")
	case o.LiqoClusterID == "":
		return errors.New("provider: LiqoClusterID is required")
	case o.Probe == nil:
		return errors.New("provider: Probe is required")
	}
	return nil
}
