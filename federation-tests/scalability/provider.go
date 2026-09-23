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

package main

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// providerRegion is a plausible (region, city, lat, lon) tuple used to seed
// synthetic Topology data. Values are approximate real-world coordinates;
// they exist only to give the Broker's latency/eco placement strategies
// realistic-shaped input, not to represent an actual deployment.
type providerRegion struct {
	Region, Zone, City string
	Lat, Lon           float64
}

var providerRegions = []providerRegion{
	{"eu-west-1", "eu-west-1a", "Dublin", 53.35, -6.26},
	{"eu-south-1", "eu-south-1a", "Milan", 45.46, 9.19},
	{"eu-north-1", "eu-north-1a", "Stockholm", 59.33, 18.06},
	{"us-east-1", "us-east-1a", "Ashburn", 39.04, -77.49},
	{"us-west-2", "us-west-2a", "Portland", 45.52, -122.68},
	{"ap-south-1", "ap-south-1a", "Mumbai", 19.08, 72.88},
}

// warmupOutcome is sent once per agent from its first (retried) call, so
// the orchestrator in main.go can gate the next lifecycle phase on it.
type warmupOutcome struct {
	ClusterID string
	OK        bool
	Err       error
}

// providerPayload builds a deterministic-per-index synthetic
// AdvertisementRequest. Every optional field the design report's evidence
// enumerates (docs/design.md §7.3.1 via internal/broker/api/types.go) is
// populated so the harness exercises the full schema the real chunk sizer,
// pricing, carbon-ranking, and topology-masking code paths read — not just
// the minimum required fields.
func providerPayload(clusterID string, index int, cfg *Config) *brokerapi.AdvertisementRequest {
	rng := rand.New(rand.NewSource(cfg.Seed + int64(index)))
	region := providerRegions[index%len(providerRegions)]

	cpuPrice := 0.02 + rng.Float64()*0.08   // per core-hour
	memPrice := 0.002 + rng.Float64()*0.008 // per GiB-hour
	carbon := 50 + rng.Float64()*450        // gCO2eq/kWh
	forecast := make([]float64, 6)
	for i := range forecast {
		forecast[i] = carbon + (rng.Float64()-0.5)*80
		if forecast[i] < 0 {
			forecast[i] = 0
		}
	}

	return &brokerapi.AdvertisementRequest{
		ClusterID:     clusterID,
		LiqoClusterID: clusterID + "-liqo",
		Resources: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cfg.ProviderCPU),
			corev1.ResourceMemory: resource.MustParse(cfg.ProviderMemory),
		},
		Topology: &brokerv1alpha1.Topology{
			Zone:      region.Zone,
			Region:    region.Region,
			City:      region.City,
			Latitude:  region.Lat + (rng.Float64()-0.5)*0.5,
			Longitude: region.Lon + (rng.Float64()-0.5)*0.5,
		},
		UnitPrices: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%.4f", cpuPrice)),
			corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%.4f", memPrice)),
		},
		CarbonIntensity: &carbon,
		CarbonForecast:  forecast,
		Renewable:       rng.Float64() < 0.3,
		// TEST-NET-3 (RFC 5737): guaranteed non-routable, so this never
		// resolves to a real host even though the field is wire-realistic.
		ProbeEndpoint: fmt.Sprintf("203.0.113.%d:9000", (index%254)+1),
	}
}

// runProvider drives one logical Provider's traffic for the whole run: a
// retried warm-up advertisement, then the steady-state advertisement and
// (optionally) instruction-poll loops.
//
// Two contexts, because stopping and cancelling are different things. genCtx
// ends when the measurement does: no new request starts after it. reqCtx is
// what requests run under, and ends only on Ctrl+C/SIGTERM, so a request
// already in flight when the measurement ends completes normally instead of
// being cut off and recorded as a Broker error.
func runProvider(genCtx, reqCtx context.Context, cfg *Config, id agentIdentity, index int, collector *Collector,
	warmupCh chan<- warmupOutcome) {
	client, err := newAgentClient(cfg, id, brokerCAFile)
	if err != nil {
		warmupCh <- warmupOutcome{ClusterID: id.ClusterID, OK: false, Err: err}
		return
	}

	req := providerPayload(id.ClusterID, index, cfg)

	warmupCtx, cancel := context.WithTimeout(genCtx, cfg.WarmupTimeout)
	ok := retryUntilSuccess(warmupCtx, func() error {
		return doAdvertisement(reqCtx, client, id, req, collector, PhaseWarmup)
	})
	cancel()
	warmupCh <- warmupOutcome{ClusterID: id.ClusterID, OK: ok, Err: genCtx.Err()}
	if !ok || genCtx.Err() != nil {
		return
	}

	advC := staggeredTicker(genCtx, cfg.AdvertisementInterval,
		startOffset(cfg.Seed, "provider", index, OpAdvertisement, cfg.AdvertisementInterval))
	var instrC <-chan time.Time
	if cfg.InstructionPoll {
		instrC = staggeredTicker(genCtx, cfg.InstructionPollInterval,
			startOffset(cfg.Seed, "provider", index, OpInstructions, cfg.InstructionPollInterval))
	}

	for {
		select {
		case <-genCtx.Done():
			return
		case <-advC:
			if genCtx.Err() != nil { // the tick and the stop can arrive together
				return
			}
			_ = doAdvertisement(reqCtx, client, id, req, collector, PhaseMeasurement)
		case <-instrC:
			if genCtx.Err() != nil {
				return
			}
			_ = doInstructionPoll(reqCtx, client, "provider", id, collector, PhaseMeasurement)
		}
	}
}

func doAdvertisement(ctx context.Context, client *agentclient.Client, id agentIdentity, req *brokerapi.AdvertisementRequest, collector *Collector, phase Phase) error {
	start := time.Now()
	_, err := client.PostAdvertisement(ctx, req)
	latency := time.Since(start)
	collector.Add(recordFor("provider", id.ClusterID, OpAdvertisement, start, latency, err, phase))
	return err
}

func doInstructionPoll(ctx context.Context, client *agentclient.Client, role string, id agentIdentity, collector *Collector, phase Phase) error {
	start := time.Now()
	_, err := client.GetInstructions(ctx)
	latency := time.Since(start)
	collector.Add(recordFor(role, id.ClusterID, OpInstructions, start, latency, err, phase))
	return err
}

// retryUntilSuccess calls fn repeatedly with a short fixed backoff until it
// returns nil or ctx is done (deadline or cancellation). It is deliberately
// simpler than internal/agent/client's exponential backoff: this is the
// harness's own outer warm-up gate, layered on top of whatever
// --client-max-retries already does inside a single fn() call.
func retryUntilSuccess(ctx context.Context, fn func() error) bool {
	const backoff = 500 * time.Millisecond
	for {
		if fn() == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
		}
	}
}
