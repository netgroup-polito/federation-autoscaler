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

import "testing"

func bp(v bool) *bool { return &v }

func TestComputeMetrics(t *testing.T) {
	outcomes := []RepetitionOutcome{
		// eco: 3 AI calls, all valid and Peered, choosing p9, p9, p7.
		{Scenario: "eco", Repetition: 1, AICalled: true, AIValid: true, AISelectedID: "p9", Source: sourceAI,
			DecisionLatencyMs: 100, ReservationAttempt: true, Peered: true, PeeringLatencyMs: 40000, CriterionPassed: bp(true)},
		{Scenario: "eco", Repetition: 2, AICalled: true, AIValid: true, AISelectedID: "p9", Source: sourceAI,
			DecisionLatencyMs: 300, ReservationAttempt: true, Peered: true, PeeringLatencyMs: 50000, CriterionPassed: bp(true)},
		{Scenario: "eco", Repetition: 3, AICalled: true, AIValid: true, AISelectedID: "p7", Source: sourceAI,
			DecisionLatencyMs: 200, ReservationAttempt: true, Peered: true, PeeringLatencyMs: 60000, CriterionPassed: bp(true)},
		// balanced: one invalid answer rescued by the fallback (not alignment evidence),
		// one valid answer that misses the criterion and whose reservation failed.
		{Scenario: "balanced", Repetition: 1, AICalled: true, AIValid: false, AISelectedID: "p99", Source: sourceFallback,
			FallbackUsed: true, DecisionLatencyMs: 400, ReservationAttempt: true, Peered: true, PeeringLatencyMs: 45000,
			CriterionPassed: bp(true), FailureCategories: []string{"unknown_provider_id"}},
		{Scenario: "balanced", Repetition: 2, AICalled: true, AIValid: true, AISelectedID: "p8", Source: sourceAI,
			DecisionLatencyMs: 500, ReservationAttempt: true, Peered: false, CriterionPassed: bp(false),
			FailureCategories: []string{"reservation_timeout"}},
	}
	m := computeMetrics(outcomes, []string{"eco", "balanced"})

	check := func(name string, got *float64, want float64) {
		t.Helper()
		if got == nil || *got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("valid-ID rate", m.ValidIDRate, 0.8)                        // 4 of 5
	check("reservation success rate", m.ReservationSuccessRate, 0.75) // 3 Peered of 4 valid
	check("overall reservation success", m.OverallReservationSuccessRate, 0.8)
	check("prompt-alignment rate", m.PromptAlignmentRate, 0.75) // fallback excluded: 3 of 4
	check("fallback rate", m.FallbackRate, 0.2)

	if m.DecisionLatency.N != 5 || *m.DecisionLatency.MeanMs != 300 || *m.DecisionLatency.P95Ms != 500 {
		t.Errorf("decision latency = %+v, want n 5, mean 300, nearest-rank p95 500", m.DecisionLatency)
	}
	if m.PeeringLatency.N != 4 {
		t.Errorf("peering latency counts only Peered reservations: n = %d, want 4", m.PeeringLatency.N)
	}
	if m.FailureBreakdown["unknown_provider_id"] != 1 || m.FailureBreakdown["reservation_timeout"] != 1 {
		t.Errorf("failure breakdown = %v", m.FailureBreakdown)
	}

	eco := m.Scenarios[0]
	if eco.Scenario != "eco" || eco.MostFrequent != "p9" || eco.Repeatability == nil || *eco.Repeatability != 0.6667 {
		t.Errorf("eco repeatability = %+v, want p9 at 2/3", eco)
	}
}

func TestComputeMetrics_EmptyDenominatorsAreNotZero(t *testing.T) {
	m := computeMetrics(nil, []string{"eco"})
	if m.ValidIDRate != nil || m.ReservationSuccessRate != nil || m.PromptAlignmentRate != nil || m.FallbackRate != nil {
		t.Errorf("rates with no samples must be nil (n/a), not 0: %+v", m)
	}
	if m.DecisionLatency.MeanMs != nil {
		t.Error("latency with no samples must have no mean")
	}
}
