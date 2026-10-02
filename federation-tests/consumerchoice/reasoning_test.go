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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
)

// anyRequest is the user request of the scenarios below, where what was asked
// does not matter -- only what came back.
const anyRequest = "Choose one."

// aiDecision is one recorded repetition the model answered.
func aiDecision(scenario, request, provider, reasoning string) *RepetitionRecord {
	rec := record(scenario, 1)
	rec.Scenario.UserRequest = request
	rec.Decision = &Decision{
		Source:          sourceAI,
		FinalProviderID: provider,
		Trace:           &ollama.Trace{Parsed: &ollama.SelectionResponse{Reasoning: reasoning}},
	}
	return rec
}

// Every decision of the run appears once, in the order it was made: a
// scenario's repetitions, then its agent path, then the next scenario.
func TestReasoningEntries_Order(t *testing.T) {
	scenarios := []Scenario{
		{Name: "eco-oriented", UserRequest: "Prioritize the greenest provider."},
		{Name: "ambiguous", UserRequest: "Choose the best provider for my workload."},
	}
	records := []*RepetitionRecord{
		aiDecision("eco-oriented", scenarios[0].UserRequest, "provider-7", "I chose provider-7 because 35 is lowest."),
		aiDecision("eco-oriented", scenarios[0].UserRequest, "provider-9", "I chose provider-9 because 25 is lowest."),
		aiDecision("ambiguous", scenarios[1].UserRequest, "provider-8", "I chose provider-8 because it is cheapest."),
	}
	agentPaths := []*AgentPathResult{
		{Scenario: "eco-oriented", ReservedProvider: "provider-7", Selection: &AgentSelection{
			Source: sourceAI, RawResponse: `{"providerId":"provider-7","reasoning":"I chose provider-7, the greenest."}`,
		}},
		{Scenario: "ambiguous", ReservedProvider: "provider-8", Selection: &AgentSelection{
			Source: sourceAI, RawResponse: `{"providerId":"provider-8","reasoning":"I chose provider-8, the cheapest."}`,
		}},
	}

	entries := reasoningEntries(scenarios, records, agentPaths)
	want := []string{"provider-7", "provider-9", "provider-7", "provider-8", "provider-8"}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d", len(entries), len(want))
	}
	for i, provider := range want {
		if entries[i].Provider != provider {
			t.Errorf("entry %d is %s, want %s", i, entries[i].Provider, provider)
		}
		if entries[i].Reasoning == "" {
			t.Errorf("entry %d (%s) lost its reasoning", i, entries[i].Provider)
		}
	}
	if entries[2].Prompt != scenarios[0].UserRequest || entries[3].Prompt != scenarios[1].UserRequest {
		t.Errorf("the agent path did not keep its scenario's prompt: %q then %q",
			entries[2].Prompt, entries[3].Prompt)
	}
}

// A choice the model did not make is still recorded, and says so: the file
// would otherwise credit the model with the fallback's provider.
func TestReasoningEntries_NotTheModelsChoice(t *testing.T) {
	scenarios := []Scenario{{Name: "s", UserRequest: anyRequest}}

	fallback := record("s", 1)
	fallback.Scenario.UserRequest = anyRequest
	fallback.Decision = &Decision{
		Source:          sourceFallback,
		FinalProviderID: "provider-1",
		Trace:           &ollama.Trace{ErrorKind: ollama.ErrKindInvalidJSON},
	}
	single := record("s", 2)
	single.Scenario.UserRequest = anyRequest
	single.Decision = &Decision{
		Source:          sourceSingleCandidate,
		FinalProviderID: "provider-2",
		Trace:           &ollama.Trace{SingleCandidate: true},
	}
	agentPaths := []*AgentPathResult{{Scenario: "s", ReservedProvider: "provider-3",
		Selection: &AgentSelection{Source: sourceFallback, ErrorKind: string(ollama.ErrKindTimeout)}}}

	entries := reasoningEntries(scenarios, []*RepetitionRecord{fallback, single}, agentPaths)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	for i, want := range []string{"fallback", "only one provider had capacity", "fallback"} {
		if !strings.Contains(entries[i].Note, want) {
			t.Errorf("entry %d note = %q, want it to mention %q", i, entries[i].Note, want)
		}
	}
	if !strings.Contains(entries[0].Note, "invalid") {
		t.Errorf("the fallback note %q does not say why the answer was rejected", entries[0].Note)
	}
	if !strings.Contains(entries[2].Note, "timeout") {
		t.Errorf("the agent-path note %q does not say why the agent fell back", entries[2].Note)
	}
}

// The file is written at the very end of a run that takes the better part of an
// hour, so a decision that produced almost nothing -- no trace, no selection,
// no provider -- must still yield a block rather than end the run in a panic.
func TestReasoningEntries_EmptyDecision(t *testing.T) {
	scenarios := []Scenario{{Name: "s", UserRequest: anyRequest}}
	bare := record("s", 1)
	bare.Scenario.UserRequest = anyRequest
	bare.Decision = &Decision{Source: sourceNone}

	entries := reasoningEntries(scenarios, []*RepetitionRecord{bare},
		[]*AgentPathResult{{Scenario: "s"}})
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	for i, e := range entries {
		if !strings.Contains(e.Provider, "none") {
			t.Errorf("entry %d should say no provider was reserved, got %q", i, e.Provider)
		}
	}
	if err := writeReasoning(filepath.Join(t.TempDir(), "reasoning.txt"), entries); err != nil {
		t.Fatalf("writeReasoning: %v", err)
	}
}

// The file is three lines and a separator per decision, whatever the model
// wrote: a reasoning with newlines in it must not break the shape, and a
// missing one must read as missing rather than as blank.
func TestWriteReasoning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reasoning.txt")
	entries := []ReasoningEntry{
		{Prompt: "Prioritize the greenest provider.", Provider: "provider-9",
			Reasoning: "I chose provider-9\nbecause its  carbon intensity is 25."},
		{Prompt: anyRequest, Provider: "provider-1", Note: "the deterministic fallback chose instead"},
	}
	if err := writeReasoning(path, entries); err != nil {
		t.Fatalf("writeReasoning: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	const want = "Prompt: Prioritize the greenest provider.\n" +
		"Chosen provider: provider-9\n" +
		"Reasoning: I chose provider-9 because its carbon intensity is 25.\n" +
		"-------\n" +
		"Prompt: Choose one.\n" +
		"Chosen provider: provider-1 [the deterministic fallback chose instead]\n" +
		"Reasoning: (the model wrote none)\n" +
		"-------\n"
	if got := string(body); got != want {
		t.Errorf("reasoning.txt =\n%s\nwant\n%s", got, want)
	}
}
