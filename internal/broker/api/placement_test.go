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

package api

import (
	"math"
	"testing"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// Representative coordinates reused across the latency cases.
const (
	montrealLat, montrealLon = 45.6085, -73.5493  // consumer + the "near" provider
	sydneyLat, sydneyLon     = -33.8688, 151.2093 // the "far" provider
)

// stdAdvCarbon is stdAdv with an advertised current carbon intensity (gCO2/kWh).
func stdAdvCarbon(name string, reserved int32, carbon float64) *brokerv1alpha1.ClusterAdvertisement {
	a := stdAdv(name, reserved, nil)
	c := carbon
	a.Spec.CarbonIntensity = &c
	return a
}

// stdAdvAt is stdAdv with advertised coordinates (the latency input).
func stdAdvAt(name string, reserved int32, lat, lon float64) *brokerv1alpha1.ClusterAdvertisement {
	a := stdAdv(name, reserved, nil)
	a.Spec.Topology = &brokerv1alpha1.Topology{Region: name, Latitude: lat, Longitude: lon}
	return a
}

// stdAdvRenewable is stdAdv with the provider's self-declared renewable flag set
// (the standard composite policy's bonus input).
func stdAdvRenewable(name string, reserved int32, renewable bool) *brokerv1alpha1.ClusterAdvertisement {
	a := stdAdv(name, reserved, nil)
	a.Spec.Renewable = renewable
	return a
}

// withStandardPolicy records an explicit Standard-preferring heartbeat.
func withStandardPolicy(s *Server) {
	s.consumers.Touch(consumerCluster, "liqo-c",
		autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyStandard}, "", "", nil, nil)
}

func withEcoPolicy(s *Server) {
	s.consumers.Touch(consumerCluster, "liqo-c",
		autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyEco}, "", "", nil, nil)
}

// withLatencyPolicy records a Latency-preferring heartbeat WITH a consumer location.
func withLatencyPolicy(s *Server, lat, lon float64) {
	la, lo := lat, lon
	s.consumers.Touch(consumerCluster, "liqo-c",
		autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyLatency}, "QC", "", &la, &lo)
}

// withLatencyPolicyNoLocation records a Latency-preferring heartbeat with NO
// consumer location — the no-op case (R2).
func withLatencyPolicyNoLocation(s *Server) {
	s.consumers.Touch(consumerCluster, "liqo-c",
		autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyLatency}, "", "", nil, nil)
}

// TestNodeGroupsEcoPreference mirrors the price matrix: masking happens only
// when an Eco policy is set AND a carbon-bearing provider has capacity; lowest
// carbon wins and carbon-less providers are a last resort.
func TestNodeGroupsEcoPreference(t *testing.T) {
	t.Run("no policy -> Standard composite ignores carbon; most-free grows", func(t *testing.T) {
		// p-green is greener but LESS free; without an Eco policy the composite
		// ignores carbon and grows the more-free (dirtier) provider.
		s := newDashboardTestServer(t, stdAdvCarbon("p-green", 2, 25), stdAdvCarbon("p-dirty", 0, 650))
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-dirty"] == 0 {
			t.Errorf("without an Eco policy carbon must be inert; composite grows the most-free provider; got %+v", hr)
		}
		if hr["p-green"] != 0 {
			t.Errorf("greener-but-less-free provider must be masked; got %+v", hr)
		}
	})

	t.Run("policy, no carbon -> all exposed (no narrowing)", func(t *testing.T) {
		s := newDashboardTestServer(t, stdAdv("p-a", 0, nil), stdAdv("p-b", 0, nil))
		withEcoPolicy(s)
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-a"] != 3 || hr["p-b"] != 3 {
			t.Errorf("no carbon-bearing provider => no narrowing; got %+v", hr)
		}
	})

	t.Run("policy + carbon -> only greenest grows; carbon-less is last resort", func(t *testing.T) {
		s := newDashboardTestServer(t,
			stdAdvCarbon("p-green", 0, 25),
			stdAdvCarbon("p-dirty", 0, 650),
			stdAdv("p-nocarbon", 0, nil))
		withEcoPolicy(s)
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-green"] != 3 {
			t.Errorf("greenest provider must keep head-room; got %+v", hr)
		}
		if hr["p-dirty"] != 0 || hr["p-nocarbon"] != 0 {
			t.Errorf("dirtier and carbon-less providers must be masked; got %+v", hr)
		}
	})

	t.Run("greedy spill: greenest full -> next-greenest grows", func(t *testing.T) {
		s := newDashboardTestServer(t,
			stdAdvCarbon("p-green", 3, 25), // fully reserved
			stdAdvCarbon("p-dirty", 0, 650))
		withEcoPolicy(s)
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-green"] != 0 {
			t.Errorf("exhausted greenest must have no head-room; got %+v", hr)
		}
		if hr["p-dirty"] != 3 {
			t.Errorf("next-greenest must be promoted to growable; got %+v", hr)
		}
	})
}

// TestNodeGroupsLatencyPreference covers the closest-first greedy plus the
// genuinely-new branch: a consumer with no location yields NO masking (R2).
func TestNodeGroupsLatencyPreference(t *testing.T) {
	t.Run("policy + coords -> only closest grows; far + coordless masked", func(t *testing.T) {
		s := newDashboardTestServer(t,
			stdAdvAt("p-near", 0, montrealLat, montrealLon),
			stdAdvAt("p-far", 0, sydneyLat, sydneyLon),
			stdAdv("p-nocoords", 0, nil))
		withLatencyPolicy(s, montrealLat, montrealLon)
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-near"] != 3 {
			t.Errorf("closest provider must keep head-room; got %+v", hr)
		}
		if hr["p-far"] != 0 || hr["p-nocoords"] != 0 {
			t.Errorf("farther and coordless providers must be masked; got %+v", hr)
		}
	})

	t.Run("consumer has NO location -> no masking (all exposed)", func(t *testing.T) {
		s := newDashboardTestServer(t,
			stdAdvAt("p-near", 0, montrealLat, montrealLon),
			stdAdvAt("p-far", 0, sydneyLat, sydneyLon))
		withLatencyPolicyNoLocation(s)
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-near"] != 3 || hr["p-far"] != 3 {
			t.Errorf("no consumer location must leave all providers exposed; got %+v", hr)
		}
	})

	t.Run("greedy spill: closest full -> next-closest grows", func(t *testing.T) {
		s := newDashboardTestServer(t,
			stdAdvAt("p-near", 3, montrealLat, montrealLon), // fully reserved
			stdAdvAt("p-far", 0, sydneyLat, sydneyLon))
		withLatencyPolicy(s, montrealLat, montrealLon)
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-near"] != 0 {
			t.Errorf("exhausted closest must have no head-room; got %+v", hr)
		}
		if hr["p-far"] != 3 {
			t.Errorf("next-closest must be promoted to growable; got %+v", hr)
		}
	})
}

// TestNodeGroupsStandardDefault covers the composite default (no ConsumerPolicy):
// it always narrows to one grower — the most-free provider, with a renewable
// tie-break — and an explicit "Standard" policy behaves identically.
func TestNodeGroupsStandardDefault(t *testing.T) {
	t.Run("most-free provider grows (spread load)", func(t *testing.T) {
		// No Touch → no policy → Standard composite. p-idle is more free.
		s := newDashboardTestServer(t, stdAdv("p-busy", 2, nil), stdAdv("p-idle", 0, nil))
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-idle"] == 0 {
			t.Errorf("most-free provider must grow; got %+v", hr)
		}
		if hr["p-busy"] != 0 {
			t.Errorf("busier provider must be masked; got %+v", hr)
		}
	})

	t.Run("renewable breaks a capacity tie", func(t *testing.T) {
		// Equal free capacity ⇒ the renewable bonus decides.
		s := newDashboardTestServer(t, stdAdvRenewable("p-grey", 0, false), stdAdvRenewable("p-clean", 0, true))
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-clean"] == 0 {
			t.Errorf("renewable provider must win a capacity tie; got %+v", hr)
		}
		if hr["p-grey"] != 0 {
			t.Errorf("non-renewable provider must be masked; got %+v", hr)
		}
	})

	t.Run("more capacity outweighs the renewable bonus", func(t *testing.T) {
		// p-clean is renewable but less free (0.8*1/3+0.2 = 0.467); p-grey is
		// fully free but grey (0.8). Capacity wins.
		s := newDashboardTestServer(t, stdAdvRenewable("p-clean", 2, true), stdAdvRenewable("p-grey", 0, false))
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-grey"] == 0 {
			t.Errorf("much-more-free provider must beat renewable-but-less-free; got %+v", hr)
		}
		if hr["p-clean"] != 0 {
			t.Errorf("renewable-but-less-free provider must be masked; got %+v", hr)
		}
	})

	t.Run("explicit Standard policy behaves like the default", func(t *testing.T) {
		s := newDashboardTestServer(t, stdAdv("p-busy", 2, nil), stdAdv("p-idle", 0, nil))
		withStandardPolicy(s)
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-idle"] == 0 || hr["p-busy"] != 0 {
			t.Errorf("explicit Standard must match the default; got %+v", hr)
		}
	})
}

// stdAdvForecast is a fully-available stdAdv with an advertised hourly carbon
// forecast (the eco weighted-ranking input).
func stdAdvForecast(name string, forecast []float64) *brokerv1alpha1.ClusterAdvertisement {
	a := stdAdv(name, 0, nil)
	a.Spec.CarbonForecast = forecast
	return a
}

func TestEcoWeightedScore(t *testing.T) {
	// Uniform forecast → the weighted average equals the value.
	if v, ok := ecoWeightedScore([]float64{100, 100, 100, 100, 100, 100}); !ok || v != 100 {
		t.Errorf("uniform: got (%v,%v), want (100,true)", v, ok)
	}
	// Known weighting: 0.40·10+0.25·20+0.15·30+0.10·40+0.06·50+0.04·60 = 22.9 (Σw=1).
	if v, ok := ecoWeightedScore([]float64{10, 20, 30, 40, 50, 60}); !ok || math.Abs(v-22.9) > 1e-9 {
		t.Errorf("weighted: got %v, want 22.9", v)
	}
	// A short forecast normalizes by the weights actually used: [10,10] → 10.
	if v, ok := ecoWeightedScore([]float64{10, 10}); !ok || math.Abs(v-10) > 1e-9 {
		t.Errorf("short: got %v, want 10", v)
	}
	// Only the first 6 hours count.
	if v, ok := ecoWeightedScore([]float64{0, 0, 0, 0, 0, 0, 1000, 1000}); !ok || v != 0 {
		t.Errorf("6-hour cap: got %v, want 0", v)
	}
	// Empty forecast has no score.
	if _, ok := ecoWeightedScore(nil); ok {
		t.Error("empty forecast must be !ok")
	}
}

// TestNodeGroupsEcoForecast: with a forecast advertised, the eco ranking uses the
// 6-hour weighted score — not the single current value.
func TestNodeGroupsEcoForecast(t *testing.T) {
	t.Run("greenest weighted forecast grows", func(t *testing.T) {
		s := newDashboardTestServer(t,
			stdAdvForecast("p-green-fc", []float64{10, 10, 10, 10, 10, 10}),
			stdAdvForecast("p-dirty-fc", []float64{500, 500, 500, 500, 500, 500}))
		withEcoPolicy(s)
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-green-fc"] != 3 {
			t.Errorf("greenest forecast must grow; got %+v", hr)
		}
		if hr["p-dirty-fc"] != 0 {
			t.Errorf("dirtier forecast must be masked; got %+v", hr)
		}
	})

	t.Run("forecast overrides a misleading current value", func(t *testing.T) {
		// p-a's current value is the worst (999) but its forecast is the greenest;
		// the ranking must follow the forecast, so p-a wins.
		a := stdAdvForecast("p-a", []float64{10, 10, 10, 10, 10, 10})
		big := 999.0
		a.Spec.CarbonIntensity = &big
		s := newDashboardTestServer(t, a, stdAdvForecast("p-b", []float64{500, 500, 500, 500, 500, 500}))
		withEcoPolicy(s)
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-a"] != 3 || hr["p-b"] != 0 {
			t.Errorf("forecast must drive ranking, not CarbonIntensity; got %+v", hr)
		}
	})

	t.Run("no forecast falls back to the single current value", func(t *testing.T) {
		// p-green has only a (green) single value; p-dirty only a (dirty) single
		// value. Fallback keeps the greenest-first behaviour.
		s := newDashboardTestServer(t, stdAdvCarbon("p-green", 0, 25), stdAdvCarbon("p-dirty", 0, 650))
		withEcoPolicy(s)
		hr := headroomByProvider(callNodeGroups(t, s))
		if hr["p-green"] != 3 || hr["p-dirty"] != 0 {
			t.Errorf("single-value fallback must still rank greenest first; got %+v", hr)
		}
	})
}
