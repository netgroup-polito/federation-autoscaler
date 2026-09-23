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

	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
)

// verdictOf runs the verdict over outcomes of one scenario, agent path off.
func verdictOf(t *testing.T, outcomes []RepetitionOutcome) (string, string) {
	t.Helper()
	s := &suiteRun{cc: &ChoiceConfig{Repetitions: len(outcomes), Scenarios: []Scenario{{Name: "eco"}},
		AgentPath: AgentPathConfig{Enabled: bp(false)}}}
	verdict, reasons := s.verdict(computeMetrics(outcomes, []string{"eco"}), outcomes, nil)
	return verdict, strings.Join(reasons, "\n")
}

// aiOutcome is repetition 1: a valid answer whose reservation reached Peered.
func aiOutcome() RepetitionOutcome {
	return RepetitionOutcome{Scenario: "eco", Repetition: 1, AICalled: true, AIValid: true, Source: sourceAI,
		ReservationAttempt: true, Peered: true, ReleasedAndSettled: true}
}

// modelFailure is a repetition where the model's answer was unusable through
// its own doing and the fallback reserved instead.
func modelFailure(rep int, category string, peered bool) RepetitionOutcome {
	return RepetitionOutcome{Scenario: "eco", Repetition: rep, AICalled: true, ModelAnswerFailure: true,
		Source: sourceFallback, FallbackUsed: true, ReservationAttempt: true, Peered: peered, ReleasedAndSettled: true,
		FailureCategories: []string{category}}
}

func TestVerdict_UnusableAnswers(t *testing.T) {
	if v, r := verdictOf(t, []RepetitionOutcome{aiOutcome(), modelFailure(2, "timeout", true)}); v != verdictPass {
		t.Errorf("a model loop covered by a Peered fallback is a result about the model, not a failure:\n%s", r)
	}

	unpeered := []RepetitionOutcome{aiOutcome(), modelFailure(2, "invalid_json", false)}
	const unpeeredReason = "eco rep 2: the model's answer was unusable and the fallback's reservation did not reach Peered"
	if v, r := verdictOf(t, unpeered); v != verdictFail || !strings.Contains(r, unpeeredReason) {
		t.Errorf("the fallback must still reserve: got %s\n%s", v, r)
	}

	unreachable := RepetitionOutcome{Scenario: "eco", Repetition: 2, AICalled: true, Source: sourceFallback,
		FallbackUsed: true, ReservationAttempt: true, Peered: true, ReleasedAndSettled: true,
		FailureCategories: []string{"unreachable"}}
	if v, r := verdictOf(t, []RepetitionOutcome{aiOutcome(), unreachable}); v != verdictFail ||
		!strings.Contains(r, "1 of 2 model answers were rejected for a reason outside the model") {
		t.Errorf("an unreachable Ollama is the system's failure: got %s\n%s", v, r)
	}

	allUnusable := []RepetitionOutcome{modelFailure(1, "timeout", true), modelFailure(2, "unknown_provider_id", true)}
	if v, r := verdictOf(t, allUnusable); v != verdictFail || !strings.Contains(r, "never gave a usable answer") {
		t.Errorf("a model that never answers usably points at the setup: got %s\n%s", v, r)
	}
}

func TestNoUsableAnswer(t *testing.T) {
	none := RunMetrics{AICalls: 2}
	agentAI := &AgentPathResult{Selection: &AgentSelection{Source: localapi.SelectionSourceAI}}
	agentFallback := &AgentPathResult{Selection: &AgentSelection{Source: localapi.SelectionSourceFallback,
		ErrorKind: "timeout"}}

	if !noUsableAnswer(none, nil) {
		t.Error("two calls, no usable answer: must be reported")
	}
	if noUsableAnswer(RunMetrics{AICalls: 2, ValidAIIDs: 1}, nil) {
		t.Error("one usable recorded answer is enough")
	}
	if noUsableAnswer(none, []*AgentPathResult{agentFallback, agentAI}) {
		t.Error("a usable answer on the agent path is enough")
	}
	if !noUsableAnswer(RunMetrics{}, []*AgentPathResult{agentFallback}) {
		t.Error("the agent path alone asked and never got a usable answer: must be reported")
	}
	if noUsableAnswer(RunMetrics{}, nil) {
		t.Error("a model never asked says nothing about its answers")
	}
}
