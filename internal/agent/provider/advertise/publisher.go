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
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/eco"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/geo"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/nodeip"
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

	// CapacityFile is an optional path to a YAML/JSON file holding this
	// provider's per-resource advertised-capacity caps (e.g.
	// {"cpu":"80%","memory":"8Gi"}; keys are resource names). It is re-read on
	// every publish cycle so an operator can re-cap without a restart. Each
	// value is EITHER a percentage of allocatable — a trailing '%' or a bare
	// integer (e.g. 80 or "80%") — OR a fixed Kubernetes quantity with a unit
	// (e.g. "8Gi", "4000m"), which caps the resource at min(fixed, allocatable).
	// A percentage in (0,100) advertises that fraction, 0 advertises none, and
	// 100 / >100 / unset ⇒ the full allocatable. A cap only ever LOWERS what is
	// advertised. Empty/missing/unparseable ⇒ the provider advertises full
	// allocatable.
	CapacityFile string

	// RenewableFile is an optional path to a YAML/JSON file holding this
	// provider's self-declared renewable-energy flag (e.g. {"renewable":true}).
	// Re-read on every publish cycle so an operator can toggle it without a
	// restart. Empty/missing/unparseable or {"renewable":false} ⇒ the provider
	// advertises no renewable bonus. Honour-system: the Broker does not verify it.
	RenewableFile string

	// NodeName is the name of the Kubernetes node this agent pod runs on
	// (injected via the NODE_NAME downward-API env). Its IP is auto-discovered
	// from v1.Node and geolocated to derive this provider's location. Empty ⇒ no
	// location discovery (unless AdvertisedIP is set).
	NodeName string

	// AdvertisedIP optionally overrides the discovered node IP (the --advertised-ip
	// demo/steering lever). When set, the node is not read. Empty ⇒ use NodeName.
	AdvertisedIP string

	// MockEcoURL is the base URL of the carbon-intensity service (e.g.
	// http://mock-eco:8081). Empty ⇒ the provider advertises no carbon intensity.
	MockEcoURL string

	// MockGeoURL is the base URL of the geo-IP service (e.g. http://mock-geo:8080).
	// The provider's node IP is looked up here to derive its region + coordinates.
	// Empty ⇒ the provider advertises no location (neither eco nor latency).
	MockGeoURL string

	// ProbeUDPPort is the always-on UDP NodePort the udpecho responder is exposed
	// on (the agent-probe Service). The provider advertises <nodeIP>:ProbeUDPPort
	// as its measured-latency probe endpoint. 0 ⇒ advertise no probe endpoint.
	ProbeUDPPort int

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
	capacityFile  string
	renewableFile string
	nodeName      string
	advertisedIP  string
	mockEcoURL    string
	mockGeoURL    string
	probeUDPPort  int
	ecoClient     *eco.Client
	geoClient     *geo.Client
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
		capacityFile:  opts.CapacityFile,
		renewableFile: opts.RenewableFile,
		nodeName:      opts.NodeName,
		advertisedIP:  opts.AdvertisedIP,
		mockEcoURL:    opts.MockEcoURL,
		mockGeoURL:    opts.MockGeoURL,
		probeUDPPort:  opts.ProbeUDPPort,
		ecoClient:     eco.NewClient(),
		geoClient:     geo.NewClient(),
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

	scaled, pctCustom, fixedCustom := p.applyCapacityScaling(snap.Allocatable)

	// Resolve the node IP once and reuse it for BOTH geo discovery and the
	// measured-latency probe endpoint (<nodeIP>:probeUDPPort). The probe endpoint
	// is advertised whenever the IP is known even if geolocation fails — the
	// consumer only needs a reachable UDP address to measure RTT.
	ip, err := nodeip.Resolve(ctx, p.localClient, p.nodeName, p.advertisedIP)
	if err != nil {
		p.log.V(1).Info("node IP discovery failed; advertising no location/probe", "err", err.Error())
		ip = ""
	}
	topology, carbon, carbonForecast := p.loadPlacementInputs(ctx, ip)

	probeEndpoint := ""
	if ip != "" && p.probeUDPPort > 0 {
		probeEndpoint = net.JoinHostPort(ip, strconv.Itoa(p.probeUDPPort))
	}

	req := &brokerapi.AdvertisementRequest{
		ClusterID:            p.clusterID,
		LiqoClusterID:        p.liqoClusterID,
		Resources:            scaled,
		Topology:             topology,
		UnitPrices:           p.loadUnitPrices(),
		CarbonIntensity:      carbon,
		CarbonForecast:       carbonForecast,
		CapacityScalePercent: pctCustom,
		CapacityFixed:        fixedCustom,
		Renewable:            p.loadRenewable(),
		ProbeEndpoint:        probeEndpoint,
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
		"percentCappedResources", len(pctCustom),
		"fixedCappedResources", len(fixedCustom),
		"piggybackedInstructions", len(resp.Instructions))
}

// applyCapacityScaling caps each allocatable resource to the operator's
// configured percentage or fixed amount and returns
// (scaledResources, percentCaps, fixedCaps). percentCaps / fixedCaps hold only
// the resources actually lowered (so the dashboard can flag them); each is nil
// when nothing of that kind was customized. A cap only ever LOWERS what is
// advertised. The rules, per resource:
//
//	percent P: 0 ≤ P < 100 → advertise P% of allocatable (recorded; 0 = none)
//	           P == 100 / P > 100 → full allocatable (not recorded)
//	           P < 0 → bad value ⇒ full allocatable, logged (not recorded)
//	fixed F:   advertise min(F, allocatable); recorded only when F < allocatable
//	unset:     full allocatable (not recorded)
//
// A configured key absent from allocatable is ignored (logged at V(1)).
func (p *Publisher) applyCapacityScaling(alloc corev1.ResourceList) (corev1.ResourceList, map[corev1.ResourceName]int32, corev1.ResourceList) {
	caps := p.loadCapacityCaps()
	if len(caps) == 0 {
		return alloc, nil, nil
	}

	scaled := make(corev1.ResourceList, len(alloc))
	var pctCaps map[corev1.ResourceName]int32
	var fixedCaps corev1.ResourceList
	for name, qty := range alloc {
		rule, ok := caps[name]
		switch {
		case !ok:
			scaled[name] = qty.DeepCopy()
		case rule.isPercent:
			switch {
			case rule.percent < 0:
				p.log.Info("invalid capacity percent (< 0); advertising full allocatable for this resource",
					"resource", name, "percent", rule.percent)
				scaled[name] = qty.DeepCopy()
			case rule.percent >= 100:
				scaled[name] = qty.DeepCopy()
			default: // 0..99 — an explicit 0 advertises none
				scaled[name] = scaleQuantity(qty, int64(rule.percent))
				if pctCaps == nil {
					pctCaps = make(map[corev1.ResourceName]int32)
				}
				pctCaps[name] = rule.percent
			}
		default: // fixed absolute cap — only ever lowers
			if rule.fixed.Cmp(qty) < 0 {
				scaled[name] = rule.fixed.DeepCopy()
				if fixedCaps == nil {
					fixedCaps = corev1.ResourceList{}
				}
				fixedCaps[name] = rule.fixed.DeepCopy()
			} else {
				scaled[name] = qty.DeepCopy()
			}
		}
	}

	for name := range caps {
		if _, ok := alloc[name]; !ok {
			p.log.V(1).Info("capacity cap set for a resource the provider does not advertise; ignored",
				"resource", name)
		}
	}
	return scaled, pctCaps, fixedCaps
}

// scaleQuantity returns q scaled to percent% of its value, preserving q's
// format. percent is assumed in (0,100). Milli-precision keeps fractional CPU
// honest (e.g. 50% of 3 cores → 1500m); for byte-scale resources like memory
// the milli intermediate stays well within int64 for any realistic cluster.
func scaleQuantity(q resource.Quantity, percent int64) resource.Quantity {
	scaledMilli := q.MilliValue() * percent / 100
	return *resource.NewMilliQuantity(scaledMilli, q.Format)
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

// loadPlacementInputs geolocates the provider's already-resolved node ip
// (mock-geo) and, when discovered, looks up the region's carbon intensity
// (mock-eco). It returns the Topology to advertise (nil when no location) and the
// carbon signal (nil when unavailable or no mock-eco URL). Every lookup is
// best-effort: a failure logs at V(1) and the corresponding field is omitted,
// never failing the publish cycle. The discovered region CODE (e.g. "QC") is the
// carbon join key, so an unset region means no carbon either. The caller resolves
// ip (nodeip.Resolve) once and reuses it for the probe endpoint too; an empty ip
// means location discovery is off.
func (p *Publisher) loadPlacementInputs(ctx context.Context, ip string) (*brokerv1alpha1.Topology, *float64, []float64) {
	if ip == "" {
		return nil, nil, nil
	}
	loc, ok, err := p.geoClient.Lookup(ctx, p.mockGeoURL, ip)
	if err != nil {
		p.log.V(1).Info("geo lookup failed; advertising no location",
			"ip", ip, "err", err.Error())
		return nil, nil, nil
	}
	if !ok {
		return nil, nil, nil
	}
	topology := &brokerv1alpha1.Topology{
		Region:    loc.Region,
		City:      loc.City,
		Latitude:  loc.Lat,
		Longitude: loc.Lon,
	}
	carbon, forecast := p.loadCarbon(ctx, loc.Region)
	return topology, carbon, forecast
}

// loadCarbon fetches the region's carbon signal. It prefers the hourly forecast
// (using forecast[0] as the current value) and falls back to the single
// current-value endpoint when the forecast is unavailable — so an older carbon
// service without /carbon/forecast still works. Every failure is best-effort:
// it logs at V(1) and advertises no carbon rather than failing the publish cycle.
func (p *Publisher) loadCarbon(ctx context.Context, region string) (*float64, []float64) {
	forecast, err := p.ecoClient.Forecast(ctx, p.mockEcoURL, region)
	if err != nil {
		p.log.V(1).Info("carbon forecast fetch failed; falling back to current value",
			"region", region, "err", err.Error())
	} else if len(forecast) > 0 {
		cur := forecast[0]
		return &cur, forecast
	}
	cur, err := p.ecoClient.CurrentCarbon(ctx, p.mockEcoURL, region)
	if err != nil {
		p.log.V(1).Info("carbon fetch failed; advertising no carbon intensity",
			"region", region, "err", err.Error())
	}
	return cur, nil
}

// capRule is one resource's advertised-capacity cap parsed from the capacity
// file. Exactly one form applies, distinguished by the value's syntax:
//
//	"60%" or bare 60  → percentage of allocatable  (isPercent, percent∈[0,100])
//	"8Gi", "4000m"    → fixed absolute quantity     (fixed; only ever lowers)
//
// Rule: a trailing '%' or a plain integer (no unit) is a percentage — keeping
// today's bare-integer files working — while any other Kubernetes quantity
// (i.e. one carrying a unit/suffix) is a fixed cap.
type capRule struct {
	isPercent bool
	percent   int32
	fixed     resource.Quantity
}

// loadRenewable reads and parses the optional renewable-energy file (if
// configured). Like loadUnitPrices it is best-effort: a missing, empty, or
// unparseable file yields false so the provider simply advertises no renewable
// bonus. Re-reading here (not at construction) lets an operator toggle it live.
func (p *Publisher) loadRenewable() bool {
	if p.renewableFile == "" {
		return false
	}
	data, err := os.ReadFile(p.renewableFile)
	if err != nil {
		p.log.V(1).Info("renewable file unreadable; advertising no renewable bonus",
			"path", p.renewableFile, "err", err.Error())
		return false
	}
	if strings.TrimSpace(string(data)) == "" {
		return false
	}
	var doc struct {
		Renewable bool `json:"renewable"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		p.log.V(1).Info("renewable file unparseable; advertising no renewable bonus",
			"path", p.renewableFile, "err", err.Error())
		return false
	}
	return doc.Renewable
}

// loadCapacityCaps reads and parses the per-resource advertised-capacity cap
// file (if configured). Like loadUnitPrices it is best-effort: a missing,
// empty, or unparseable file yields nil so the provider simply advertises its
// full allocatable rather than failing the publish cycle. Re-reading here (not
// at construction) is what lets an operator re-cap live — kubelet refreshes the
// projected ConfigMap file and the next cycle picks it up. Values are read
// leniently via IntOrString (so a bare int, a quoted int, or a quantity string
// all parse); percent-vs-fixed classification happens per value.
func (p *Publisher) loadCapacityCaps() map[corev1.ResourceName]capRule {
	if p.capacityFile == "" {
		return nil
	}
	data, err := os.ReadFile(p.capacityFile)
	if err != nil {
		p.log.V(1).Info("capacity file unreadable; advertising full allocatable",
			"path", p.capacityFile, "err", err.Error())
		return nil
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil
	}
	var raw map[corev1.ResourceName]intstr.IntOrString
	if err := yaml.Unmarshal(data, &raw); err != nil {
		p.log.V(1).Info("capacity file unparseable; advertising full allocatable",
			"path", p.capacityFile, "err", err.Error())
		return nil
	}
	if len(raw) == 0 {
		return nil
	}
	out := make(map[corev1.ResourceName]capRule, len(raw))
	for name, v := range raw {
		s := strings.TrimSpace(v.String())
		if s == "" {
			continue
		}
		if pct, ok := parsePercent(s); ok {
			out[name] = capRule{isPercent: true, percent: pct}
			continue
		}
		q, err := resource.ParseQuantity(s)
		if err != nil {
			p.log.V(1).Info("capacity value is neither a percentage nor a valid quantity; ignored",
				"resource", name, "value", s, "err", err.Error())
			continue
		}
		out[name] = capRule{fixed: q}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parsePercent recognises a percentage cap: an explicit "N%" or a bare integer
// with no unit (the historical form). Returns the integer percent and true on a
// match; false means the value should be treated as a fixed quantity instead.
func parsePercent(s string) (int32, bool) {
	if t, ok := strings.CutSuffix(s, "%"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, false
		}
		return int32(n), true
	}
	if n, err := strconv.Atoi(s); err == nil {
		return int32(n), true
	}
	return 0, false
}

func (p *Publisher) notifyResult(success bool) {
	if p.onResult == nil {
		return
	}
	defer func() { _ = recover() }() // never let a misbehaving callback kill the loop
	p.onResult(success)
}
