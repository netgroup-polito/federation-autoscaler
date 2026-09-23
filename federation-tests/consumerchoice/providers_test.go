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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/federation-tests/testlib"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// advertisedCatalogue is the Broker view once every provider of the default
// config advertises its profile, unmasked (as under ConsumerChoice).
func advertisedCatalogue(t *testing.T) (*ChoiceConfig, *brokerapi.NodeGroupListResponse) {
	t.Helper()
	_, cc, err := loadConfig("configs/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resp := &brokerapi.NodeGroupListResponse{}
	for i, p := range cc.ProviderProfiles {
		chunks, _ := p.Capacity.Chunks()
		carbon := float64(p.CarbonIntensity)
		resp.NodeGroups = append(resp.NodeGroups, brokerapi.NodeGroupView{
			ID:                "ng-" + providerID(i) + "-standard",
			ProviderClusterID: providerID(i),
			Type:              brokerv1alpha1.ChunkTypeStandard,
			MaxSize:           chunks,
			Cost:              resource.NewMilliQuantity(50, resource.DecimalSI),
			CarbonIntensity:   &carbon,
			Topology:          &brokerv1alpha1.Topology{Latitude: 45, Longitude: 9},
		})
	}
	return cc, resp
}

func TestMetadataMismatches_AdvertisedCatalogueIsComplete(t *testing.T) {
	cc, resp := advertisedCatalogue(t)
	if pending, _ := metadataMismatches(resp, cc); len(pending) != 0 {
		t.Errorf("a fully advertised catalogue must have nothing pending, got %v", pending)
	}
}

// Under any policy but ConsumerChoice the Broker masks all but one provider,
// which hides their capacity. That must be named as masking -- and recognised
// as not worth waiting for -- rather than reported as a slow advertisement.
func TestMetadataMismatches_SingleWinnerMaskingIsRecognised(t *testing.T) {
	cc, resp := advertisedCatalogue(t)
	for i := 1; i < len(resp.NodeGroups); i++ {
		resp.NodeGroups[i].MaxSize = resp.NodeGroups[i].CurrentReserved
	}

	pending, onlyMasked := metadataMismatches(resp, cc)
	if len(pending) != len(cc.ProviderProfiles)-1 || !onlyMasked {
		t.Fatalf("want %d masked providers and onlyMasked, got %v (onlyMasked %v)",
			len(cc.ProviderProfiles)-1, pending, onlyMasked)
	}
	if !strings.Contains(pending[0], maskedByBroker) {
		t.Errorf("problem must say the provider is masked: %q", pending[0])
	}

	// A provider that is also still missing its cost is a genuine wait.
	resp.NodeGroups[1].Cost = nil
	if _, onlyMasked := metadataMismatches(resp, cc); onlyMasked {
		t.Error("a missing cost is not masking: onlyMasked must be false")
	}
}

// assertUnmasked is the guard that keeps the suite from ever measuring another
// policy under ConsumerChoice's name.
func TestAssertUnmasked(t *testing.T) {
	baseline := testlib.FederationCapacity{ReservedBy: map[string]int32{}}

	cc, resp := advertisedCatalogue(t)
	resp.AppliedPlacement = autoscalingv1alpha1.PlacementStrategyConsumerChoice
	if err := assertUnmasked(resp, cc, baseline); err != nil {
		t.Errorf("an unmasked ConsumerChoice list must pass: %v", err)
	}

	// Single-winner masking (what Standard does) while the Broker claims ConsumerChoice.
	_, masked := advertisedCatalogue(t)
	masked.AppliedPlacement = autoscalingv1alpha1.PlacementStrategyConsumerChoice
	for i := 1; i < len(masked.NodeGroups); i++ {
		masked.NodeGroups[i].MaxSize = masked.NodeGroups[i].CurrentReserved
	}
	err := assertUnmasked(masked, cc, baseline)
	if err == nil || !strings.Contains(err.Error(), "ConsumerChoice unavailable") {
		t.Errorf("a masked list must be reported as ConsumerChoice unavailable, got %v", err)
	}

	// Another policy applied at all.
	_, other := advertisedCatalogue(t)
	other.AppliedPlacement = autoscalingv1alpha1.PlacementStrategyStandard
	err = assertUnmasked(other, cc, baseline)
	if err == nil || !strings.Contains(err.Error(), "ConsumerChoice unavailable") {
		t.Errorf("another applied policy must be refused, got %v", err)
	}

	// Only one provider with room: there is no choice to exercise. Every other
	// provider is genuinely full at baseline, so this is not masking.
	_, full := advertisedCatalogue(t)
	full.AppliedPlacement = autoscalingv1alpha1.PlacementStrategyConsumerChoice
	fullBaseline := testlib.FederationCapacity{ReservedBy: map[string]int32{}}
	for i := 1; i < len(full.NodeGroups); i++ {
		full.NodeGroups[i].CurrentReserved = full.NodeGroups[i].MaxSize
		fullBaseline.ReservedBy[full.NodeGroups[i].ID] = full.NodeGroups[i].MaxSize
	}
	if err := assertUnmasked(full, cc, fullBaseline); err == nil || !strings.Contains(err.Error(), "no choice to make") {
		t.Errorf("a single growable provider must be refused, got %v", err)
	}
}

// The reservation ID becomes a label value and, prefixed with "rs-", the
// virtual node's name (and so its kubernetes.io/hostname label): both are
// capped at 63 characters, whatever the scenario is called.
func TestReservationIDFitsKubernetesLimits(t *testing.T) {
	runID := testlib.RunID()
	for _, scenarioIdx := range []int{0, 8, 98} {
		for _, rep := range []int{1, 99} {
			id := reservationID(runID, scenarioIdx, rep)
			if errs := validation.IsDNS1123Subdomain(id); len(errs) > 0 {
				t.Errorf("%s is not a DNS-1123 subdomain: %v", id, errs)
			}
			for _, value := range []string{id, "rs-" + id} {
				if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
					t.Errorf("%s (%d chars) is not a valid label value: %v", value, len(value), errs)
				}
			}
		}
	}
}
