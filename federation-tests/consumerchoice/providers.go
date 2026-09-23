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
	"log"
	"math"
	"sort"
	"strings"
	"time"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/federation-tests/testlib"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// providerID is the cluster ID testlib gives the i-th provider (0-based).
func providerID(i int) string { return fmt.Sprintf("provider-%d", i+1) }

// applyProviderProfiles makes every provider advertise what its profile says:
// carbon through the controllable mock-eco (per region), prices and capacity
// through the agent's ConfigMaps. The provider agents then re-advertise on
// their own cycle -- nothing here talks to the Broker directly.
func applyProviderProfiles(ctx context.Context, orch *testlib.Orchestrator, cc *ChoiceConfig,
	mockEco *testlib.MockEcoClient) error {
	log.Println("=== APPLY PROVIDER PROFILES ===")
	for i, p := range cc.ProviderProfiles {
		region := orch.Config.ProviderRegions[i]
		spec := orch.Specs[1+orch.Config.Consumers+i]
		chunks, _ := p.Capacity.Chunks() // validated at load time
		log.Printf("  %s (%s): carbon %d gCO2eq/kWh, prices %v, capacity %s/%s (%d chunks)",
			providerID(i), region, p.CarbonIntensity, p.Prices, p.Capacity.CPU, p.Capacity.Memory, chunks)

		// A nil forecast makes mock-eco serve a flat one, so the Broker's
		// forecast-weighted carbon and the current value the consumer sees agree.
		if err := mockEco.SetCarbon(ctx, region, p.CarbonIntensity, nil); err != nil {
			return fmt.Errorf("set carbon for %s (%s): %w", providerID(i), region, err)
		}
		if err := testlib.SetProviderPrices(ctx, spec.Kubeconfig, p.Prices); err != nil {
			return fmt.Errorf("set prices for %s: %w", providerID(i), err)
		}
		if err := testlib.SetProviderCapacity(ctx, spec.Kubeconfig, p.Capacity.CPU, p.Capacity.Memory); err != nil {
			return fmt.Errorf("set capacity for %s: %w", providerID(i), err)
		}
	}
	return nil
}

// Poll intervals of the preparation waits.
const (
	metadataPoll = 10 * time.Second
	policyPoll   = 5 * time.Second
)

// maskedByBroker is the problem reported for a provider listed without
// head-room although its profile gives it capacity.
const maskedByBroker = "masked by the Broker"

// waitForProviderMetadata polls the Broker until every provider advertises the
// carbon, cost and capacity its profile set. The decision must not start before
// then: a model choosing among half-updated advertisements would be judged
// against numbers it never saw.
//
// It must run with ConsumerChoice already applied: any other policy masks all
// but one provider (MaxSize = CurrentReserved), which hides the advertised
// capacity this wait checks.
func waitForProviderMetadata(ctx context.Context, broker *testlib.BrokerClient, cc *ChoiceConfig,
	timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		resp, err := broker.GetNodeGroups(ctx)
		var pending []string
		onlyMasked := false
		if err != nil {
			pending = []string{fmt.Sprintf("broker: %v", err)}
		} else {
			pending, onlyMasked = metadataMismatches(resp, cc)
		}
		if len(pending) == 0 {
			log.Printf("[providers] all %d providers advertise their profile", len(cc.ProviderProfiles))
			return nil
		}
		if time.Now().After(deadline) {
			if onlyMasked {
				return fmt.Errorf("ConsumerChoice unavailable: the Broker masks providers that have capacity:\n  %s",
					strings.Join(pending, "\n  "))
			}
			return fmt.Errorf("providers did not advertise their profiles within %s:\n  %s",
				timeout, strings.Join(pending, "\n  "))
		}
		log.Printf("[providers] waiting for %d advertisement(s): %s", len(pending), strings.Join(pending, "; "))
		if err := testlib.SleepCtx(ctx, metadataPoll); err != nil {
			return err
		}
	}
}

// metadataMismatches lists, per provider, what the Broker view still lacks from
// the provider's profile. onlyMasked is true when every remaining problem is a
// provider masked by the Broker, which no amount of waiting will fix.
func metadataMismatches(resp *brokerapi.NodeGroupListResponse, cc *ChoiceConfig) (pending []string, onlyMasked bool) {
	byProvider := map[string]brokerapi.NodeGroupView{}
	for _, ng := range resp.NodeGroups {
		if ng.Type == brokerv1alpha1.ChunkTypeStandard {
			byProvider[ng.ProviderClusterID] = ng
		}
	}
	masked := 0
	for i, p := range cc.ProviderProfiles {
		id := providerID(i)
		ng, ok := byProvider[id]
		if !ok {
			pending = append(pending, id+": not listed")
			continue
		}
		var problems []string
		if ng.CarbonIntensity == nil || math.Abs(*ng.CarbonIntensity-float64(p.CarbonIntensity)) > 0.5 {
			problems = append(problems,
				fmt.Sprintf("carbon %s want %d", fmtFloatPtr(ng.CarbonIntensity), p.CarbonIntensity))
		}
		if ng.Cost == nil {
			problems = append(problems, "no cost yet")
		}
		want, _ := p.Capacity.Chunks()
		switch {
		case ng.MaxSize == ng.CurrentReserved && want > ng.CurrentReserved:
			problems = append(problems, fmt.Sprintf("%s (maxSize %d = reserved)", maskedByBroker, ng.MaxSize))
		case ng.MaxSize != want:
			problems = append(problems, fmt.Sprintf("%d chunks want %d", ng.MaxSize, want))
		}
		if ng.Topology == nil || (ng.Topology.Latitude == 0 && ng.Topology.Longitude == 0) {
			problems = append(problems, "no location")
		}
		if len(problems) == 1 && strings.HasPrefix(problems[0], maskedByBroker) {
			masked++
		}
		if len(problems) > 0 {
			pending = append(pending, id+": "+strings.Join(problems, ", "))
		}
	}
	return pending, len(pending) > 0 && masked == len(pending)
}

// activateConsumerChoice sets the scenario's policy and request on the
// consumer and waits until the Broker itself reports ConsumerChoice for this
// consumer -- the heartbeat, not the console write, is what the Broker acts on.
func activateConsumerChoice(ctx context.Context, console *testlib.ConsoleClient, broker *testlib.BrokerClient,
	sc Scenario, timeout time.Duration) error {
	policy := string(autoscalingv1alpha1.PlacementStrategyConsumerChoice)
	if err := console.SetPolicyWithPrompt(ctx, policy, sc.UserRequest); err != nil {
		return fmt.Errorf("set ConsumerChoice policy: %w", err)
	}
	state, err := console.State(ctx)
	if err != nil {
		return err
	}
	if state.Policy != policy || state.UserPrompt != sc.UserRequest {
		return fmt.Errorf("console did not store the policy: policy %q, prompt %q", state.Policy, state.UserPrompt)
	}

	deadline := time.Now().Add(timeout)
	for {
		resp, err := broker.GetNodeGroups(ctx)
		if err == nil && resp.AppliedPlacement == autoscalingv1alpha1.PlacementStrategyConsumerChoice {
			log.Printf("[policy] Broker applies ConsumerChoice for %s", broker.ClusterID)
			return nil
		}
		if time.Now().After(deadline) {
			got := "unreachable"
			if err == nil {
				got = string(resp.AppliedPlacement)
			}
			return fmt.Errorf("ConsumerChoice unavailable: the Broker still reports placement %q after %s", got, timeout)
		}
		if err := testlib.SleepCtx(ctx, policyPoll); err != nil {
			return err
		}
	}
}

// assertUnmasked fails the run when the Broker narrowed the list. Under
// ConsumerChoice every provider with free capacity must be growable; if some
// are not, the Broker is applying a single-winner policy and the model would be
// "choosing" among whatever the Broker already chose. That is exactly the
// defect this suite exists to catch, so it aborts rather than carrying on.
func assertUnmasked(resp *brokerapi.NodeGroupListResponse, cc *ChoiceConfig,
	baseline testlib.FederationCapacity) error {
	if resp.AppliedPlacement != autoscalingv1alpha1.PlacementStrategyConsumerChoice {
		return fmt.Errorf("ConsumerChoice unavailable: Broker applied placement %q", resp.AppliedPlacement)
	}
	var masked []string
	growable := 0
	for i, p := range cc.ProviderProfiles {
		id := providerID(i)
		want, _ := p.Capacity.Chunks()
		for _, ng := range resp.NodeGroups {
			if ng.ProviderClusterID != id || ng.Type != brokerv1alpha1.ChunkTypeStandard {
				continue
			}
			reservedAtBaseline := baseline.ReservedBy[ng.ID]
			switch {
			case ng.MaxSize > ng.CurrentReserved:
				growable++
			case want > reservedAtBaseline:
				masked = append(masked, fmt.Sprintf("%s (maxSize %d, reserved %d)",
					id, ng.MaxSize, ng.CurrentReserved))
			}
		}
	}
	if len(masked) > 0 {
		sort.Strings(masked)
		return fmt.Errorf("ConsumerChoice unavailable: the Broker masked providers that have free capacity "+
			"(%d growable): %s", growable, strings.Join(masked, ", "))
	}
	if growable < 2 {
		return fmt.Errorf("ConsumerChoice cannot be exercised: only %d provider is growable, "+
			"so there is no choice to make", growable)
	}
	return nil
}

func fmtFloatPtr(v *float64) string {
	if v == nil {
		return "none"
	}
	return fmt.Sprintf("%g", *v)
}
