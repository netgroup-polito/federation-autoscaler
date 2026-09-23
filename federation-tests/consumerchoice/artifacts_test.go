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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
)

func candidate(id, region string, carbon float64) CandidateMetrics {
	cost, distance := 0.052, 518.1
	return CandidateMetrics{ProviderID: id, NodeGroupID: id + "-standard", Region: region, Latitude: 50.1109,
		Longitude: 8.6821, AvailableChunks: 2, AvailableCPUMillicores: 4000, AvailableMemoryMiB: 8192,
		CostPerChunk: &cost, CarbonIntensity: &carbon, DistanceKm: &distance, CarbonRank: 7, DistanceRank: 3, CostRank: 2}
}

func record(scenario string, rep int, candidates ...CandidateMetrics) *RepetitionRecord {
	return &RepetitionRecord{Scenario: Scenario{Name: scenario}, Repetition: rep, Candidates: candidates}
}

// providers.csv is the federation once: one row per provider, every column of
// the header filled from it, the city resolved from the region code.
func TestProviderRows(t *testing.T) {
	choices := map[string]string{"provider-4": "eco-oriented (1/6)"}
	rows := providerRows([]CandidateMetrics{candidate("provider-4", "HE", 350)}, choices)
	if len(rows) != 1 || len(rows[0]) != len(providersHeader()) {
		t.Fatalf("rows %v do not match the header %v", rows, providersHeader())
	}
	got := strings.Join(rows[0], ",")
	const want = "provider-4,HE,Frankfurt,50.1109,8.6821,518.1,3,350,7,0.052,2,2,4000,8192,eco-oriented (1/6)"
	if got != want {
		t.Errorf("row = %s\nwant  %s", got, want)
	}
}

// Where the model chose each provider: every scenario it was picked in, in
// scenario order, out of all that scenario's decisions -- recorded repetitions
// and agent path together. A fallback or a single candidate is not the model's
// choice, but its decision still counts in the total.
func TestModelChoices(t *testing.T) {
	decision := func(scenario string, rep int, source, provider string) *RepetitionRecord {
		rec := record(scenario, rep)
		rec.Outcome = RepetitionOutcome{Source: source, FinalProviderID: provider}
		return rec
	}
	records := []*RepetitionRecord{
		decision("eco-oriented", 1, sourceAI, "provider-9"),
		decision("eco-oriented", 2, sourceAI, "provider-8"),
		decision("eco-oriented", 3, sourceAI, "provider-9"),
		decision("ambiguous", 1, sourceAI, "provider-8"),
		decision("ambiguous", 2, sourceFallback, "provider-8"),
		decision("ambiguous", 3, sourceSingleCandidate, "provider-8"),
	}
	agentPaths := []*AgentPathResult{
		{Scenario: "ambiguous", ExpectedProvider: "provider-2",
			Selection: &AgentSelection{Source: localapi.SelectionSourceAI}},
		{Scenario: "eco-oriented", ExpectedProvider: "provider-2",
			Selection: &AgentSelection{Source: localapi.SelectionSourceAI}},
		{Scenario: "balanced", ExpectedProvider: "provider-8",
			Selection: &AgentSelection{Source: localapi.SelectionSourceFallback, ErrorKind: "timeout"}},
	}

	// eco-oriented: 3 recorded + 1 agent path; balanced: its agent path only;
	// ambiguous: 3 recorded + 1 agent path.
	got := modelChoices([]string{"eco-oriented", "balanced", "ambiguous"}, records, agentPaths)
	want := map[string]string{
		"provider-9": "eco-oriented (2/4)",
		"provider-8": "eco-oriented (1/4) - ambiguous (1/4)",
		"provider-2": "eco-oriented (1/4) - ambiguous (1/4)",
	}
	if len(got) != len(want) {
		t.Errorf("choices = %v, want exactly %v", got, want)
	}
	for provider, w := range want {
		if got[provider] != w {
			t.Errorf("model chose %s in %q, want %q", provider, got[provider], w)
		}
	}
}

// The federation is shown once only because the run holds it fixed; any
// decision that saw different provider data must be named.
func TestFederationDrift(t *testing.T) {
	a, b := candidate("provider-1", "LOM", 520), candidate("provider-2", "CH", 45)
	same := []*RepetitionRecord{record("eco", 1, a, b), record("eco", 2, a, b), record("balanced", 1, b, a)}
	if drift := federationDrift(same); drift != nil {
		t.Errorf("the same data in any order is no drift, got %v", drift)
	}

	greener := candidate("provider-1", "LOM", 300)
	changed := []*RepetitionRecord{record("eco", 1, a, b), record("eco", 2, greener, b), record("balanced", 3, a)}
	got := strings.Join(federationDrift(changed), "; ")
	if !strings.Contains(got, "eco rep 2 (provider-1)") ||
		!strings.Contains(got, "balanced rep 3 (1 providers instead of 2)") {
		t.Errorf("drift = %q", got)
	}

	moved := b
	moved.DistanceRank = 9 // derived, not advertised: not a change in the data
	if drift := federationDrift([]*RepetitionRecord{record("eco", 1, a, b), record("eco", 2, a, moved)}); drift != nil {
		t.Errorf("a rank is derived from the data, not part of it, got %v", drift)
	}
}

// The prompt is the same for every repetition of a scenario, so prompt.txt is
// written once. A repetition whose prompt differed must be named.
func TestPromptDrift(t *testing.T) {
	asked := func(scenario string, rep int, system, user string) *RepetitionRecord {
		rec := record(scenario, rep)
		rec.Decision = &Decision{Trace: &ollama.Trace{SystemPrompt: system, UserPrompt: user}}
		return rec
	}
	same := []*RepetitionRecord{
		asked("eco", 1, "rules", "greenest + providers"),
		asked("eco", 2, "rules", "greenest + providers"),
		asked("balanced", 1, "rules", "balance + providers"), // another scenario, another request
		asked("balanced", 2, "rules", "balance + providers"),
	}
	if drift := promptDrift(same); drift != nil {
		t.Errorf("one prompt per scenario is the point, not one per run: %v", drift)
	}

	changed := append(append([]*RepetitionRecord(nil), same...),
		asked("eco", 3, "rules", "greenest + fewer providers"),
		asked("balanced", 3, "other rules", "balance + providers"))
	if got := strings.Join(promptDrift(changed), "; "); got != "eco rep 3; balanced rep 3" {
		t.Errorf("drift = %q, want both repetitions named", got)
	}

	noTrace := []*RepetitionRecord{asked("eco", 1, "rules", "greenest"), record("eco", 2)}
	if drift := promptDrift(noTrace); drift != nil {
		t.Errorf("a repetition that never reached the model has no prompt to compare: %v", drift)
	}
}

// A repetition keeps what the model answered and what the reservation did; the
// prompt and the Broker's raw list are kept once per scenario.
func TestWriteRepetition_Files(t *testing.T) {
	dir := t.TempDir()
	rt := &ollamaRuntime{cfg: OllamaConfig{Model: "llama3.2"}}
	for rep := 1; rep <= 2; rep++ {
		rec := record("eco", rep)
		rec.Dir = filepath.Join(dir, "eco", fmt.Sprintf("rep-%02d", rep))
		rec.Decision = &Decision{Trace: answerTrace(nil, nil, nil)}
		if err := writeRepetition(rec, rt); err != nil {
			t.Fatal(err)
		}
	}

	var files []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	want := "eco/broker_nodegroups.json eco/prompt.txt " +
		"eco/rep-01/model_response.json eco/rep-01/reservation_result.json " +
		"eco/rep-02/model_response.json eco/rep-02/reservation_result.json"
	if got := strings.Join(files, " "); got != want {
		t.Errorf("files:\n%s\nwant\n%s", got, want)
	}
}
