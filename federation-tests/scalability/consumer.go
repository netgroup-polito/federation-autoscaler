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
	"math/rand"
	"time"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// consumerPayload builds a deterministic-per-index synthetic
// HeartbeatRequest. Placement.Type is fixed to PlacementStrategyRandom per
// the experiment's requirement to exercise the "random" policy
// (api/autoscaling/v1alpha1/consumerpolicy_types.go) — the Broker reads
// this back off the ConsumerRegistry on every GET /api/v1/nodegroups this
// consumer subsequently issues (internal/broker/api/nodegroups.go).
func consumerPayload(clusterID string, index int, cfg *Config) *brokerapi.HeartbeatRequest {
	rng := rand.New(rand.NewSource(cfg.Seed + int64(index) + 1_000_000)) // offset so consumer/provider streams never collide
	region := providerRegions[index%len(providerRegions)]
	lat := region.Lat + (rng.Float64()-0.5)*0.5
	lon := region.Lon + (rng.Float64()-0.5)*0.5

	return &brokerapi.HeartbeatRequest{
		ClusterID:     clusterID,
		LiqoClusterID: clusterID + "-liqo",
		Placement:     &autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyRandom},
		Region:        region.Region,
		City:          region.City,
		Latitude:      &lat,
		Longitude:     &lon,
	}
}

// runConsumer drives one logical Consumer's traffic for the whole run: a
// retried warm-up heartbeat, then the steady-state heartbeat and
// (optionally) instruction-poll loops immediately, and the evaluation loop
// only once evalGate is closed by the orchestrator (main.go) — i.e. once
// every consumer's warm-up heartbeat has landed, per the required lifecycle
// (README "Warm-up Phase"). genCtx and reqCtx are as in runProvider.
func runConsumer(genCtx, reqCtx context.Context, cfg *Config, id agentIdentity, index int, collector *Collector,
	warmupCh chan<- warmupOutcome, evalGate <-chan struct{}) {
	client, err := newAgentClient(cfg, id, brokerCAFile)
	if err != nil {
		warmupCh <- warmupOutcome{ClusterID: id.ClusterID, OK: false, Err: err}
		return
	}

	req := consumerPayload(id.ClusterID, index, cfg)

	warmupCtx, cancel := context.WithTimeout(genCtx, cfg.WarmupTimeout)
	ok := retryUntilSuccess(warmupCtx, func() error {
		return doHeartbeat(reqCtx, client, id, req, collector, PhaseWarmup)
	})
	cancel()
	warmupCh <- warmupOutcome{ClusterID: id.ClusterID, OK: ok, Err: genCtx.Err()}
	if !ok || genCtx.Err() != nil {
		return
	}

	hbC := staggeredTicker(genCtx, cfg.HeartbeatInterval,
		startOffset(cfg.Seed, "consumer", index, OpHeartbeat, cfg.HeartbeatInterval))
	var instrC <-chan time.Time
	if cfg.InstructionPoll {
		instrC = staggeredTicker(genCtx, cfg.InstructionPollInterval,
			startOffset(cfg.Seed, "consumer", index, OpInstructions, cfg.InstructionPollInterval))
	}

	// The evaluation loop only starts once every consumer has completed its
	// warm-up heartbeat, so GET /api/v1/nodegroups traffic never begins while
	// some consumers are still unregistered — but a consumer's own
	// heartbeat/instruction-poll traffic starts immediately, matching a real
	// Consumer Agent that heartbeats on its own cadence regardless of what
	// other clusters are doing. Its offset counts from the gate, so the
	// consumers' evaluations spread over the first interval of the
	// measurement instead of all landing on its first instant.
	var evalC <-chan time.Time
	localEvalGate := evalGate
	for {
		select {
		case <-genCtx.Done():
			return
		case <-localEvalGate:
			localEvalGate = nil // consume once
			evalC = staggeredTicker(genCtx, cfg.ConsumerEvalInterval,
				startOffset(cfg.Seed, "consumer", index, OpEvaluation, cfg.ConsumerEvalInterval))
		case <-hbC:
			if genCtx.Err() != nil { // the tick and the stop can arrive together
				return
			}
			_ = doHeartbeat(reqCtx, client, id, req, collector, PhaseMeasurement)
		case <-instrC:
			if genCtx.Err() != nil {
				return
			}
			_ = doInstructionPoll(reqCtx, client, "consumer", id, collector, PhaseMeasurement)
		case <-evalC:
			if genCtx.Err() != nil {
				return
			}
			_ = doEvaluation(reqCtx, client, id, collector, PhaseMeasurement)
		}
	}
}

func doHeartbeat(ctx context.Context, client *agentclient.Client, id agentIdentity, req *brokerapi.HeartbeatRequest, collector *Collector, phase Phase) error {
	start := time.Now()
	_, err := client.PostHeartbeat(ctx, req)
	latency := time.Since(start)
	collector.Add(recordFor("consumer", id.ClusterID, OpHeartbeat, start, latency, err, phase))
	return err
}

func doEvaluation(ctx context.Context, client *agentclient.Client, id agentIdentity, collector *Collector, phase Phase) error {
	start := time.Now()
	_, err := client.GetNodeGroups(ctx)
	latency := time.Since(start)
	collector.Add(recordFor("consumer", id.ClusterID, OpEvaluation, start, latency, err, phase))
	return err
}
