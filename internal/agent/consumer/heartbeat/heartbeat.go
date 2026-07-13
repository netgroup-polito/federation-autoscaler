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

// Package heartbeat drives the consumer-side 15 s heartbeat loop
// (docs/design.md §7.3.3): every interval a Heartbeater POSTs to
// /api/v1/heartbeat so the Broker's in-memory ConsumerRegistry
// remembers this consumer's liqoClusterID — the broker needs that
// value when it mints provider instructions, since the chain
// `liqoctl generate peering-user --consumer-cluster-id <id>` runs on
// the provider with the consumer's Liqo ID as the only handle.
//
// A stalled heartbeat is a "broker is unreachable" signal of the same
// flavour as a stalled instruction poll, so the Heartbeater plumbs its
// success / failure into health.Probe.RecordPoll alongside the poller.
package heartbeat

import (
	"context"
	"errors"
	"time"

	"github.com/go-logr/logr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/geo"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/nodeip"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// DefaultInterval is the cadence at which the heartbeater POSTs to
// /api/v1/heartbeat when Options.Interval is unset (docs/design.md
// §7.3.3 fixes this at 15 s).
const DefaultInterval = 15 * time.Second

// Options bundles the construction-time settings of a Heartbeater.
type Options struct {
	// Client is the Broker HTTP client built in step 7a/7b. Required.
	Client *agentclient.Client

	// ClusterID and LiqoClusterID are stamped on every
	// HeartbeatRequest. Both required.
	ClusterID     string
	LiqoClusterID string

	// LocalClient reads the consumer-cluster ConsumerPolicy CRD on every
	// heartbeat so a price-preference change propagates to the Broker without
	// a restart. Optional: nil ⇒ the heartbeat carries no policy and the
	// Broker keeps its default (no price preference).
	LocalClient ctrlclient.Client

	// Namespace is where the ConsumerPolicy is read from. Used only when
	// LocalClient is set.
	Namespace string

	// NodeName is the name of the Kubernetes node this agent pod runs on
	// (injected via the NODE_NAME downward-API env). Its IP is auto-discovered
	// from v1.Node and geolocated to derive this consumer's location. Empty ⇒ no
	// location discovery (unless AdvertisedIP is set).
	NodeName string

	// AdvertisedIP optionally overrides the discovered node IP (the --advertised-ip
	// demo/steering lever). When set, the node is not read. Empty ⇒ use NodeName.
	AdvertisedIP string

	// MockGeoURL is the base URL of the geo-IP service (e.g. http://mock-geo:8080).
	// The consumer's node IP is looked up here to derive its region + coordinates,
	// which the Broker uses to rank providers by distance under the latency
	// strategy. Empty ⇒ the consumer pushes no location.
	MockGeoURL string

	// Interval overrides DefaultInterval. Mainly useful in tests.
	Interval time.Duration

	// Logger is the structured logger every heartbeat cycle logs
	// through. Defaults to controller-runtime's logger named "heartbeat".
	Logger logr.Logger

	// OnHeartbeatResult, if non-nil, is invoked after every heartbeat
	// attempt with the success / failure outcome. The consumer role
	// wires this to health.Probe.RecordPoll so a stalled heartbeat
	// reddens /readyz.
	OnHeartbeatResult func(success bool)
}

// Heartbeater drives the 15 s heartbeat loop. A single Run goroutine
// is the only entry point; reflecting the single-replica Recreate
// invariant of agent Deployments, Heartbeater is NOT safe for
// concurrent invocation.
type Heartbeater struct {
	client        *agentclient.Client
	clusterID     string
	liqoClusterID string
	localClient   ctrlclient.Client
	namespace     string
	nodeName      string
	advertisedIP  string
	mockGeoURL    string
	geoClient     *geo.Client
	interval      time.Duration
	log           logr.Logger
	onResult      func(success bool)
}

// New validates opts and returns a Heartbeater ready to Run. It
// performs no I/O.
func New(opts Options) (*Heartbeater, error) {
	switch {
	case opts.Client == nil:
		return nil, errors.New("heartbeat: Client is required")
	case opts.ClusterID == "":
		return nil, errors.New("heartbeat: ClusterID is required")
	case opts.LiqoClusterID == "":
		return nil, errors.New("heartbeat: LiqoClusterID is required")
	}
	logger := opts.Logger
	if logger.GetSink() == nil {
		logger = log.Log.WithName("heartbeat")
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Heartbeater{
		client:        opts.Client,
		clusterID:     opts.ClusterID,
		liqoClusterID: opts.LiqoClusterID,
		localClient:   opts.LocalClient,
		namespace:     opts.Namespace,
		nodeName:      opts.NodeName,
		advertisedIP:  opts.AdvertisedIP,
		mockGeoURL:    opts.MockGeoURL,
		geoClient:     geo.NewClient(),
		interval:      interval,
		log:           logger,
		onResult:      opts.OnHeartbeatResult,
	}, nil
}

// Run blocks until ctx is cancelled. It heartbeats immediately so the
// broker's ConsumerRegistry sees this consumer as soon as the agent
// boots (which the broker needs before it will accept a POST
// /api/v1/reservations from this consumer), then on every interval
// tick.
func (h *Heartbeater) Run(ctx context.Context) {
	h.log.Info("starting heartbeat poster",
		"interval", h.interval, "clusterID", h.clusterID)

	t := time.NewTicker(h.interval)
	defer t.Stop()

	h.beatOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			h.log.Info("heartbeat poster stopped", "reason", ctx.Err())
			return
		case <-t.C:
			h.beatOnce(ctx)
		}
	}
}

// beatOnce executes one POST /api/v1/heartbeat cycle. Errors log at
// V(1) and notify onResult(false); a successful call notifies
// onResult(true).
func (h *Heartbeater) beatOnce(ctx context.Context) {
	req := &brokerapi.HeartbeatRequest{
		ClusterID:     h.clusterID,
		LiqoClusterID: h.liqoClusterID,
		Placement:     h.currentPlacement(ctx),
	}
	if ip, err := nodeip.Resolve(ctx, h.localClient, h.nodeName, h.advertisedIP); err != nil {
		h.log.V(1).Info("node IP discovery failed; heartbeating no location", "err", err.Error())
	} else if loc, ok, err := h.geoClient.Lookup(ctx, h.mockGeoURL, ip); err != nil {
		h.log.V(1).Info("geo lookup failed; heartbeating no location",
			"ip", ip, "err", err.Error())
	} else if ok {
		lat, lon := loc.Lat, loc.Lon
		req.Region = loc.Region
		req.City = loc.City
		req.Latitude, req.Longitude = &lat, &lon
	}
	if _, err := h.client.PostHeartbeat(ctx, req); err != nil {
		if ctx.Err() == nil {
			h.log.V(1).Info("heartbeat failed", "err", err.Error())
		}
		h.notifyResult(false)
		return
	}
	h.notifyResult(true)
}

// currentPlacement reads the consumer-cluster ConsumerPolicy and returns its
// placement policy to push to the Broker. Best-effort by design: a read error,
// or no ConsumerPolicy present, returns nil so the Broker falls back to its
// default (no price preference). Reading here (not at construction) is what
// makes a policy edit take effect within one heartbeat interval, no restart.
func (h *Heartbeater) currentPlacement(ctx context.Context) *autoscalingv1alpha1.PlacementPolicy {
	if h.localClient == nil {
		return nil
	}
	var list autoscalingv1alpha1.ConsumerPolicyList
	if err := h.localClient.List(ctx, &list, ctrlclient.InNamespace(h.namespace)); err != nil {
		if ctx.Err() == nil {
			h.log.V(1).Info("read ConsumerPolicy failed; sending no policy", "err", err.Error())
		}
		return nil
	}
	if len(list.Items) == 0 {
		return nil
	}
	// Deterministic pick if more than one exists (a misconfiguration): lowest
	// name wins, logged so the operator can spot it.
	chosen := &list.Items[0]
	for i := range list.Items {
		if list.Items[i].Name < chosen.Name {
			chosen = &list.Items[i]
		}
	}
	if len(list.Items) > 1 {
		h.log.V(1).Info("multiple ConsumerPolicy objects; using lowest name",
			"chosen", chosen.Name, "count", len(list.Items))
	}
	placement := chosen.Spec.Placement
	return &placement
}

func (h *Heartbeater) notifyResult(success bool) {
	if h.onResult == nil {
		return
	}
	defer func() { _ = recover() }() // never let a misbehaving callback kill the loop
	h.onResult(success)
}
