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
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/federation-tests/testlib"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

func fp(v float64) *float64 { return &v }

func TestRankAscending(t *testing.T) {
	got := rankAscending([]*float64{fp(30), nil, fp(10), fp(30), fp(20)})
	want := []int{3, 0, 1, 3, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ranks = %v, want %v (ties share the lower rank, nil is unranked)", got, want)
		}
	}
}

func TestInWorstQuantile(t *testing.T) {
	// n = 9, q = 0.25 -> ceil(2.25) = 3 worst ranks: 7, 8, 9.
	for rank, want := range map[int]bool{0: false, 1: false, 6: false, 7: true, 9: true} {
		if got := inWorstQuantile(rank, 9, 0.25); got != want {
			t.Errorf("inWorstQuantile(%d, 9, 0.25) = %v, want %v", rank, got, want)
		}
	}
}

func TestReferenceScores(t *testing.T) {
	scores := referenceScores(
		[]*float64{fp(10), fp(20), fp(30)},
		[]*float64{fp(300), fp(200), nil},
		[]*float64{fp(1), fp(1), fp(1)},
		Weights{Carbon: 1, Distance: 1})
	// carbon norm 0, .5, 1; distance norm 1, 0, 1 (missing scores worst); cost ignored.
	want := []float64{0.5, 0.25, 1}
	for i, w := range want {
		if math.Abs(*scores[i]-w) > 1e-9 {
			t.Errorf("score[%d] = %v, want %v", i, *scores[i], w)
		}
	}
}

// catalogueCandidates turns a shipped config into the candidate list the model
// would receive, using the same region coordinates mock-geo reports and the
// same per-chunk cost the Broker computes (2 CPU + 4 GiB at the unit prices),
// and evaluates it from the consumer's location (providerRegions[0]).
func catalogueCandidates(t *testing.T, path string) (*ChoiceConfig, []CandidateMetrics) {
	t.Helper()
	shared, cc, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	_, clat, clon, _ := testlib.RegionLocation(shared.ProviderRegions[0])
	consumer := &ollama.Location{Latitude: clat, Longitude: clon, Region: shared.ProviderRegions[0]}

	infos := make([]ollama.ProviderInfo, 0, len(cc.ProviderProfiles))
	for i, p := range cc.ProviderProfiles {
		_, lat, lon, _ := testlib.RegionLocation(shared.ProviderRegions[i])
		cpu, _ := strconv.ParseFloat(p.Prices["cpu"], 64)
		mem, _ := strconv.ParseFloat(p.Prices["memory"], 64)
		cost := resource.NewMilliQuantity(int64(math.Round((2*cpu+4*mem)*1000)), resource.DecimalSI)
		chunks, _ := p.Capacity.Chunks()
		carbon := float64(p.CarbonIntensity)
		ng := brokerapi.NodeGroupView{
			ID:                "ng-" + providerID(i) + "-standard",
			ProviderClusterID: providerID(i),
			Type:              brokerv1alpha1.ChunkTypeStandard,
			MaxSize:           chunks,
			ChunkResources: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
			Cost:            cost,
			CarbonIntensity: &carbon,
			Topology:        &brokerv1alpha1.Topology{Region: shared.ProviderRegions[i], Latitude: lat, Longitude: lon},
		}
		infos = append(infos, ollama.NodeGroupViewToProviderInfo(ng))
	}
	return cc, buildCandidateMetrics(infos, consumer, nil)
}

// The configs document, per scenario, which providers satisfy its criterion.
// This pins those claims to the actual catalogue and evaluation code, so a
// tweak to either cannot silently change what a pass means.
func TestShippedCatalogue_PassingSets(t *testing.T) {
	cc, candidates := catalogueCandidates(t, "configs/all-scenarios.yaml")
	want := map[string]string{
		"eco-oriented":       "provider-7,provider-9",                       // Helsinki 35, Montreal 25
		"proximity-oriented": "provider-2",                                  // Zurich: 2nd nearest and green
		"balanced":           "provider-2,provider-3,provider-5,provider-6", // Zurich, Paris, Vienna, London
		"ambiguous": "provider-1,provider-2,provider-3,provider-4,provider-5," +
			"provider-6,provider-7,provider-8,provider-9",
	}
	for _, sc := range cc.Scenarios {
		var passing []string
		for _, c := range candidates {
			if r := evaluateCriterion(sc.Criterion, candidates, c.ProviderID); r.Passed != nil && *r.Passed {
				passing = append(passing, c.ProviderID)
			}
		}
		sort.Strings(passing)
		if got := strings.Join(passing, ","); got != want[sc.Name] {
			t.Errorf("%s: passing providers = %s, want %s", sc.Name, got, want[sc.Name])
		}
	}
}

func TestShippedCatalogue_TradeOffsAreReal(t *testing.T) {
	_, candidates := catalogueCandidates(t, "configs/default.yaml")
	by := map[string]CandidateMetrics{}
	for _, c := range candidates {
		by[c.ProviderID] = c
	}
	// The co-located provider must be the nearest but not green, the greenest
	// must be far, and the cheapest must be dirty -- otherwise one provider wins
	// every scenario and the suite cannot tell requests apart.
	if c := by["provider-1"]; c.DistanceRank != 1 || c.CarbonRank < 7 {
		t.Errorf("provider-1 should be nearest and dirty: distance rank %d, carbon rank %d", c.DistanceRank, c.CarbonRank)
	}
	if c := by["provider-9"]; c.CarbonRank != 1 || c.DistanceRank < 8 {
		t.Errorf("provider-9 should be greenest and far: carbon rank %d, distance rank %d", c.CarbonRank, c.DistanceRank)
	}
	if c := by["provider-8"]; c.CostRank != 1 || c.CarbonRank != 9 {
		t.Errorf("provider-8 should be cheapest and dirtiest: cost rank %d, carbon rank %d", c.CostRank, c.CarbonRank)
	}
	if d := by["provider-1"].DistanceKm; d == nil || *d > 1 {
		t.Errorf("provider-1 shares the consumer's region; distance = %v", d)
	}
}

func TestEvaluateCriterion_NotEvaluableWithoutMetric(t *testing.T) {
	// No consumer location: nothing to measure distances from.
	candidates := buildCandidateMetrics([]ollama.ProviderInfo{
		{ProviderID: "a", CarbonIntensity: fp(10), Latitude: 48.85, Longitude: 2.35},
		{ProviderID: "b", CarbonIntensity: fp(20), Latitude: 47.37, Longitude: 8.54},
	}, nil, nil)
	r := evaluateCriterion(Criterion{Type: criterionProximity, MaxDistanceRank: 3, WorstQuantile: 0.25}, candidates, "a")
	if r.Passed != nil {
		t.Errorf("proximity without distances must be not evaluable, got passed=%v", *r.Passed)
	}
	r = evaluateCriterion(Criterion{Type: criterionEco, MaxCarbonRank: 1}, candidates, "unknown")
	if r.Passed == nil || *r.Passed {
		t.Errorf("a provider that was not a candidate must fail, got %+v", r)
	}
}

func TestDistancesFrom(t *testing.T) {
	milan := &ollama.Location{Latitude: 45.4642, Longitude: 9.19}
	providers := []ollama.ProviderInfo{
		{ProviderID: "paris", Latitude: 48.8566, Longitude: 2.3522},
		{ProviderID: "unset"}, // (0,0): no position advertised
	}
	d := distancesFrom(milan, providers)
	if d[0] == nil || math.Abs(*d[0]-640) > 10 || d[1] != nil {
		t.Errorf("distances = %v, %v; want ~640 km to Paris and none for an unset position", d[0], d[1])
	}
	if d := distancesFrom(nil, providers); d[0] != nil {
		t.Error("without a consumer location no distance can be measured")
	}
}

func TestProximityOutlier_OnlyWhenCleanerNearbyExists(t *testing.T) {
	// Every nearby provider is dirty: picking the nearest is the trade-off the
	// request asked for, not an outlier.
	candidates := candidateMetrics([]ollama.ProviderInfo{
		{ProviderID: "near-dirty", CarbonIntensity: fp(700)},
		{ProviderID: "near-dirty2", CarbonIntensity: fp(650)},
		{ProviderID: "far-green", CarbonIntensity: fp(20)},
		{ProviderID: "far-green2", CarbonIntensity: fp(30)},
	}, []*float64{fp(10), fp(20), fp(9000), fp(8000)}, nil)
	c := Criterion{Type: criterionProximity, MaxDistanceRank: 2, WorstQuantile: 0.25}
	if r := evaluateCriterion(c, candidates, "near-dirty2"); r.Passed == nil || !*r.Passed {
		t.Errorf("no cleaner nearby alternative: near-dirty2 must pass, got %+v", r)
	}
}

func TestPickLowestAndFallbackModes(t *testing.T) {
	ng := func(id string, cost float64, carbon *float64) brokerapi.NodeGroupView {
		return brokerapi.NodeGroupView{
			ID: "ng-" + id + "-standard", ProviderClusterID: id, Type: brokerv1alpha1.ChunkTypeStandard, MaxSize: 2,
			Cost: resource.NewMilliQuantity(int64(cost*1000), resource.DecimalSI), CarbonIntensity: carbon,
		}
	}
	fresh := &brokerapi.NodeGroupListResponse{
		AppliedPlacement: "ConsumerChoice",
		NodeGroups:       []brokerapi.NodeGroupView{ng("p1", 0.05, fp(400)), ng("p2", 0.09, fp(30)), ng("p3", 0.05, fp(100))},
	}
	for mode, want := range map[string]string{
		fallbackCheapest:     "p3", // p1 and p3 tie on cost; lower carbon breaks it
		fallbackLowestCarbon: "p2",
		fallbackAgentDefault: "p3", // DeterministicFallback: cost, then carbon
	} {
		d := &Decision{FreshSnapshot: fresh, Validation: Validation{FallbackMode: mode}}
		applyFallback(d, mode)
		if d.FinalProviderID != want || d.Source != sourceFallback || !d.Validation.FallbackApplied {
			t.Errorf("%s: final %q source %q applied %v, want %q",
				mode, d.FinalProviderID, d.Source, d.Validation.FallbackApplied, want)
		}
	}

	d := &Decision{FreshSnapshot: fresh, Source: sourceNone}
	applyFallback(d, fallbackFail)
	if d.FinalProviderID != "" || d.Validation.FallbackApplied {
		t.Errorf("fail mode must not pick anything: %+v", d)
	}
}
