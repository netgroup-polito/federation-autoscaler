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
)

// RepetitionOutcome is the compact, per-repetition record the run-level
// metrics are computed from. Every rate in RunMetrics can be recomputed by
// hand from the list of these in summary.json.
type RepetitionOutcome struct {
	Scenario           string   `json:"scenario"`
	Repetition         int      `json:"repetition"`
	AICalled           bool     `json:"aiCalled"`
	SingleCandidate    bool     `json:"singleCandidate"`
	AIValid            bool     `json:"aiValid"`
	ModelAnswerFailure bool     `json:"modelAnswerFailure"`
	AISelectedID       string   `json:"aiSelectedProviderId,omitempty"`
	Source             string   `json:"source"`
	FinalProviderID    string   `json:"finalProviderId,omitempty"`
	FallbackUsed       bool     `json:"fallbackUsed"`
	DecisionLatencyMs  float64  `json:"decisionLatencyMs"`
	ReservationAttempt bool     `json:"reservationAttempted"`
	Peered             bool     `json:"peered"`
	PeeringLatencyMs   float64  `json:"peeringLatencyMs,omitempty"`
	CriterionPassed    *bool    `json:"criterionPassed"`
	CarbonRank         int      `json:"carbonRank,omitempty"`
	DistanceRank       int      `json:"distanceRank,omitempty"`
	CostRank           int      `json:"costRank,omitempty"`
	ReferenceRank      int      `json:"referenceRank,omitempty"`
	CandidateCount     int      `json:"candidateCount"`
	ReleasedAndSettled bool     `json:"releasedAndSettled"`
	FailureCategories  []string `json:"failureCategories,omitempty"`
}

// LatencyStats summarises a latency sample.
type LatencyStats struct {
	N      int      `json:"n"`
	MeanMs *float64 `json:"meanMs,omitempty"`
	P95Ms  *float64 `json:"p95Ms,omitempty"`
	MaxMs  *float64 `json:"maxMs,omitempty"`
}

// ScenarioMetrics is the repeatability view of one scenario.
type ScenarioMetrics struct {
	Scenario     string         `json:"scenario"`
	Repetitions  int            `json:"repetitions"`
	AICalls      int            `json:"aiCalls"`
	Distribution map[string]int `json:"selectionDistribution"`
	MostFrequent string         `json:"mostFrequentProvider,omitempty"`
	// Repeatability is the share of the model's answers that named the most
	// frequent provider. Its denominator is the AI calls, not the repetitions:
	// a repetition where the model was not asked says nothing about consistency.
	Repeatability   *float64     `json:"repeatability,omitempty"`
	CriterionPassed int          `json:"criterionPassed"`
	CriterionTotal  int          `json:"criterionEvaluated"`
	DecisionLatency LatencyStats `json:"decisionLatency"`
}

// RunMetrics are the rates the suite reports. A rate is nil when its
// denominator is zero: "0 of 0" is not 0%, and printing it as such would claim
// a measurement that never happened.
type RunMetrics struct {
	AICalls     int      `json:"aiCalls"`
	ValidAIIDs  int      `json:"validAiIds"`
	ValidIDRate *float64 `json:"validIdRate"`
	// ModelAnswerFailures are the rejected answers whose every failure was the
	// model's own: results about the model, not failures of the run.
	ModelAnswerFailures           int               `json:"modelAnswerFailures"`
	PeeredFromValidAI             int               `json:"peeredFromValidAiSelections"`
	ReservationSuccessRate        *float64          `json:"reservationSuccessRate"`
	Reservations                  int               `json:"reservationsAttempted"`
	Peered                        int               `json:"reservationsPeered"`
	OverallReservationSuccessRate *float64          `json:"overallReservationSuccessRate"`
	AlignmentEvaluated            int               `json:"alignmentEvaluated"`
	AlignmentPassed               int               `json:"alignmentPassed"`
	PromptAlignmentRate           *float64          `json:"promptAlignmentRate"`
	FallbackActivations           int               `json:"fallbackActivations"`
	FallbackRate                  *float64          `json:"fallbackRate"`
	SingleCandidateShortcuts      int               `json:"singleCandidateShortcuts"`
	DecisionLatency               LatencyStats      `json:"decisionLatency"`
	PeeringLatency                LatencyStats      `json:"peeringLatency"`
	Scenarios                     []ScenarioMetrics `json:"scenarios"`
	FailureBreakdown              map[string]int    `json:"failureBreakdown"`
}

func computeMetrics(outcomes []RepetitionOutcome, scenarioOrder []string) RunMetrics {
	m := RunMetrics{FailureBreakdown: map[string]int{}}
	var decisions, peerings []float64
	perScenario := map[string]*ScenarioMetrics{}
	perScenarioLatency := map[string][]float64{}
	for _, name := range scenarioOrder {
		perScenario[name] = &ScenarioMetrics{Scenario: name, Distribution: map[string]int{}}
	}

	for _, o := range outcomes {
		sm := perScenario[o.Scenario]
		if sm == nil {
			sm = &ScenarioMetrics{Scenario: o.Scenario, Distribution: map[string]int{}}
			perScenario[o.Scenario] = sm
			scenarioOrder = append(scenarioOrder, o.Scenario)
		}
		sm.Repetitions++
		if o.SingleCandidate {
			m.SingleCandidateShortcuts++
		}
		if o.AICalled {
			m.AICalls++
			sm.AICalls++
			decisions = append(decisions, o.DecisionLatencyMs)
			perScenarioLatency[o.Scenario] = append(perScenarioLatency[o.Scenario], o.DecisionLatencyMs)
			if o.AIValid {
				m.ValidAIIDs++
				if o.Peered {
					m.PeeredFromValidAI++
				}
			}
			if o.ModelAnswerFailure {
				m.ModelAnswerFailures++
			}
			if o.FallbackUsed {
				m.FallbackActivations++
			}
		}
		if o.AISelectedID != "" {
			sm.Distribution[o.AISelectedID]++
		}
		if o.ReservationAttempt {
			m.Reservations++
			if o.Peered {
				m.Peered++
				peerings = append(peerings, o.PeeringLatencyMs)
			}
		}
		// Alignment judges the model, so only selections that came from it count.
		if o.Source == sourceAI && o.CriterionPassed != nil {
			m.AlignmentEvaluated++
			sm.CriterionTotal++
			if *o.CriterionPassed {
				m.AlignmentPassed++
				sm.CriterionPassed++
			}
		}
		for _, f := range o.FailureCategories {
			m.FailureBreakdown[f]++
		}
	}

	m.ValidIDRate = ratio(m.ValidAIIDs, m.AICalls)
	m.ReservationSuccessRate = ratio(m.PeeredFromValidAI, m.ValidAIIDs)
	m.OverallReservationSuccessRate = ratio(m.Peered, m.Reservations)
	m.PromptAlignmentRate = ratio(m.AlignmentPassed, m.AlignmentEvaluated)
	m.FallbackRate = ratio(m.FallbackActivations, m.AICalls)
	m.DecisionLatency = latencyStats(decisions)
	m.PeeringLatency = latencyStats(peerings)

	for _, name := range scenarioOrder {
		sm := perScenario[name]
		sm.DecisionLatency = latencyStats(perScenarioLatency[name])
		if best, count := mostFrequent(sm.Distribution); count > 0 {
			sm.MostFrequent = best
			sm.Repeatability = ratio(count, sm.AICalls)
		}
		m.Scenarios = append(m.Scenarios, *sm)
	}
	return m
}

// mostFrequent returns the most selected provider; ties go to the lower ID so
// the report is stable across runs.
func mostFrequent(dist map[string]int) (string, int) {
	ids := make([]string, 0, len(dist))
	for id := range dist {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	best, count := "", 0
	for _, id := range ids {
		if dist[id] > count {
			best, count = id, dist[id]
		}
	}
	return best, count
}

func ratio(num, den int) *float64 {
	if den == 0 {
		return nil
	}
	r := math.Round(float64(num)/float64(den)*1e4) / 1e4
	return &r
}

// latencyStats uses the nearest-rank p95: with few samples it is an observed
// value rather than an interpolation between two of them.
func latencyStats(samples []float64) LatencyStats {
	s := LatencyStats{N: len(samples)}
	if len(samples) == 0 {
		return s
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	sum := 0.0
	for _, v := range sorted {
		sum += v
	}
	mean := math.Round(sum/float64(len(sorted))*10) / 10
	rank := int(math.Ceil(0.95 * float64(len(sorted))))
	p95 := sorted[rank-1]
	maxV := sorted[len(sorted)-1]
	s.MeanMs, s.P95Ms, s.MaxMs = &mean, &p95, &maxV
	return s
}
