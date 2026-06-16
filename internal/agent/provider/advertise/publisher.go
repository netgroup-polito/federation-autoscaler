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

// Package advertise drives the provider-side 30 s advertisement loop
// (docs/design.md §7.3.1): every interval a Publisher takes a Snapshot
// of the local cluster's Allocatable, packages it as a
// brokerapi.AdvertisementRequest, and POSTs it to /api/v1/advertisements.
// The POST doubles as the provider's heartbeat — see §4.4 — so a
// stalled publisher must surface as a failed-readyz signal alongside a
// stalled instruction poll.
//
// Piggy-backed instructions in the response are intentionally ignored
// here: the shared poller (internal/agent/poller) already polls every
// 5 s and is the single dispatch point — duplicating dispatch from two
// goroutines would race the broker's Status.Enforced filter.
package advertise

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/provider/snapshot"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// DefaultInterval is the cadence at which the publisher POSTs to
// /api/v1/advertisements when Options.Interval is unset (docs/design.md
// §7.3.1 fixes this at 30 s).
const DefaultInterval = 30 * time.Second

// Options bundles the construction-time settings of a Publisher.
type Options struct {
	// Client is the Broker HTTP client built in step 7a/7b. Required.
	Client *agentclient.Client

	// LocalClient reads local Nodes for the snapshot. Required.
	LocalClient ctrlclient.Client

	// ClusterID and LiqoClusterID are stamped on every
	// AdvertisementRequest. Both required.
	ClusterID     string
	LiqoClusterID string

	// PriceFile is an optional path to a YAML/JSON file holding this
	// provider's per-resource unit prices (e.g. {"cpu":"0.03","memory":"4Mi"
	// …}; keys are resource names, values are price-per-unit-per-hour). It is
	// re-read on every publish cycle so an operator can reprice without a
	// restart. Empty/missing/unparseable ⇒ the provider advertises no price.
	PriceFile string

	// Interval overrides DefaultInterval. Mainly useful in tests.
	Interval time.Duration

	// Logger is the structured logger every publish cycle logs through.
	// Defaults to controller-runtime's logger named "advertise".
	Logger logr.Logger

	// OnPublishResult, if non-nil, is invoked after every publish
	// attempt with the success/failure outcome. The provider role wires
	// this to health.Probe.RecordPoll so a stalled advertisement
	// reddens /readyz alongside a stalled instruction poll.
	OnPublishResult func(success bool)
}

// Publisher drives the 30 s advertisement loop. A single Run goroutine
// is the only entry point; reflecting the single-replica Recreate
// invariant of agent Deployments, Publisher is NOT safe for concurrent
// invocation.
type Publisher struct {
	client        *agentclient.Client
	localClient   ctrlclient.Client
	clusterID     string
	liqoClusterID string
	priceFile     string
	interval      time.Duration
	log           logr.Logger
	onResult      func(success bool)
}

// New validates opts and returns a Publisher ready to Run. It performs
// no I/O.
func New(opts Options) (*Publisher, error) {
	switch {
	case opts.Client == nil:
		return nil, errors.New("advertise: Client is required")
	case opts.LocalClient == nil:
		return nil, errors.New("advertise: LocalClient is required")
	case opts.ClusterID == "":
		return nil, errors.New("advertise: ClusterID is required")
	case opts.LiqoClusterID == "":
		return nil, errors.New("advertise: LiqoClusterID is required")
	}
	logger := opts.Logger
	if logger.GetSink() == nil {
		logger = log.Log.WithName("advertise")
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Publisher{
		client:        opts.Client,
		localClient:   opts.LocalClient,
		clusterID:     opts.ClusterID,
		liqoClusterID: opts.LiqoClusterID,
		priceFile:     opts.PriceFile,
		interval:      interval,
		log:           logger,
		onResult:      opts.OnPublishResult,
	}, nil
}

// Run blocks until ctx is cancelled. It publishes immediately so the
// broker sees this provider as soon as the agent boots, then on every
// interval tick.
func (p *Publisher) Run(ctx context.Context) {
	p.log.Info("starting advertisement publisher",
		"interval", p.interval, "clusterID", p.clusterID)

	t := time.NewTicker(p.interval)
	defer t.Stop()

	p.publishOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			p.log.Info("advertisement publisher stopped", "reason", ctx.Err())
			return
		case <-t.C:
			p.publishOnce(ctx)
		}
	}
}

// publishOnce executes one snapshot-then-POST cycle. Any error logs at
// V(1) and notifies onResult(false); a successful publish notifies
// onResult(true) and logs the chunk count returned by the broker for
// debugging.
func (p *Publisher) publishOnce(ctx context.Context) {
	snap, err := snapshot.Take(ctx, p.localClient)
	if err != nil {
		if ctx.Err() == nil {
			p.log.V(1).Info("snapshot failed", "err", err.Error())
		}
		p.notifyResult(false)
		return
	}

	req := &brokerapi.AdvertisementRequest{
		ClusterID:     p.clusterID,
		LiqoClusterID: p.liqoClusterID,
		Resources:     snap.Allocatable,
		UnitPrices:    p.loadUnitPrices(),
	}

	resp, err := p.client.PostAdvertisement(ctx, req)
	if err != nil {
		if ctx.Err() == nil {
			p.log.V(1).Info("advertisement post failed", "err", err.Error())
		}
		p.notifyResult(false)
		return
	}

	p.notifyResult(true)
	p.log.V(1).Info("advertisement published",
		"chunkCount", resp.ChunkCount,
		"countedNodes", snap.CountedNodes,
		"pricedResources", len(req.UnitPrices),
		"piggybackedInstructions", len(resp.Instructions))
}

// loadUnitPrices reads and parses the per-resource unit-price file (if
// configured). It is intentionally best-effort: a missing, empty, or
// unparseable file yields nil, so the provider simply advertises no price
// rather than failing the publish cycle. Re-reading here (not at construction)
// is what makes live repricing work — kubelet refreshes the projected
// ConfigMap file and the next cycle picks it up.
func (p *Publisher) loadUnitPrices() corev1.ResourceList {
	if p.priceFile == "" {
		return nil
	}
	data, err := os.ReadFile(p.priceFile)
	if err != nil {
		p.log.V(1).Info("price file unreadable; advertising no price",
			"path", p.priceFile, "err", err.Error())
		return nil
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil
	}
	var prices corev1.ResourceList
	if err := yaml.Unmarshal(data, &prices); err != nil {
		p.log.V(1).Info("price file unparseable; advertising no price",
			"path", p.priceFile, "err", err.Error())
		return nil
	}
	if len(prices) == 0 {
		return nil
	}
	return prices
}

func (p *Publisher) notifyResult(success bool) {
	if p.onResult == nil {
		return
	}
	defer func() { _ = recover() }() // never let a misbehaving callback kill the loop
	p.onResult(success)
}
