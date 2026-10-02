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
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
)

// reasoningSeparator closes every block of reasoning.txt.
const reasoningSeparator = "-------"

// ReasoningEntry is one decision as reasoning.txt states it: what the model was
// asked, what it answered, and why it says it answered that.
//
// Note is empty for the plain case -- the model answered and its choice was
// reserved. It carries the qualification when there is one: the deterministic
// fallback chose instead, or the model was never asked because a single
// candidate left nothing to choose between.
type ReasoningEntry struct {
	Prompt    string
	Provider  string
	Note      string
	Reasoning string
}

// reasoningEntries collects every decision of the run, in the order it was
// made: each scenario's recorded repetitions, then its agent-path decision.
// The agent path is included because it is the same model answering the same
// prompt, only asked by the agent instead of by the harness -- leaving it out
// would drop one decision in six for no reason a reader would expect.
func reasoningEntries(scenarios []Scenario, records []*RepetitionRecord,
	agentPaths []*AgentPathResult) []ReasoningEntry {
	entries := make([]ReasoningEntry, 0, len(records)+len(agentPaths))
	for _, sc := range scenarios {
		for _, rec := range records {
			if rec.Scenario.Name == sc.Name {
				entries = append(entries, recordedEntry(sc, rec))
			}
		}
		for _, res := range agentPaths {
			if res.Scenario == sc.Name {
				entries = append(entries, agentPathEntry(sc, res))
			}
		}
	}
	return entries
}

// recordedEntry turns one harness-driven decision into an entry.
func recordedEntry(sc Scenario, rec *RepetitionRecord) ReasoningEntry {
	e := ReasoningEntry{Prompt: sc.UserRequest, Provider: rec.Decision.FinalProviderID}
	if e.Provider == "" {
		e.Provider = "(none: no provider was reserved)"
	}
	if trace := rec.Decision.Trace; trace != nil && trace.Parsed != nil {
		e.Reasoning = trace.Parsed.Reasoning
	}
	switch rec.Decision.Source {
	case sourceFallback:
		e.Note = fallbackNote(rec.Decision.Trace, rec.Decision.Failures)
	case sourceSingleCandidate:
		e.Note = "the model was not asked: only one provider had capacity"
	}
	return e
}

// agentPathEntry turns the agent's own decision into an entry. The agent logs
// the model's raw answer rather than a parsed one, so the reasoning is read
// back out of it here.
func agentPathEntry(sc Scenario, res *AgentPathResult) ReasoningEntry {
	e := ReasoningEntry{Prompt: sc.UserRequest, Provider: res.ReservedProvider}
	if e.Provider == "" {
		e.Provider = "(none: no provider was reserved)"
	}
	sel := res.Selection
	if sel == nil {
		e.Note = "the agent logged no decision for this prompt"
		return e
	}
	if sel.RawResponse != "" {
		var parsed ollama.SelectionResponse
		if json.Unmarshal([]byte(sel.RawResponse), &parsed) == nil {
			e.Reasoning = parsed.Reasoning
		}
	}
	if sel.Source != sourceAI {
		e.Note = strings.TrimSpace(fmt.Sprintf("the agent used its deterministic fallback %s",
			bracket(sel.ErrorKind)))
	}
	return e
}

// fallbackNote says why the model's answer was not the one reserved.
func fallbackNote(trace *ollama.Trace, failures []string) string {
	kind := ""
	if trace != nil {
		kind = string(trace.ErrorKind)
	}
	if kind == "" && len(failures) > 0 {
		kind = failures[0]
	}
	return strings.TrimSpace("the deterministic fallback chose instead " + bracket(kind))
}

// bracket renders a reason in parentheses, or nothing when there is none.
func bracket(kind string) string {
	if kind == "" {
		return ""
	}
	return "(" + strings.ReplaceAll(kind, "_", " ") + ")"
}

// writeReasoning writes the run's decisions as plain text, one block each:
//
//	Prompt: ...
//	Chosen provider: ...
//	Reasoning: ...
//	-------
//
// It is the one artifact meant to be read straight through rather than queried,
// so it stays flat: no run header, no repetition numbers, nothing to skip past.
// Everything else about a decision is in summary.json and the per-repetition
// model_response.json next to it.
func writeReasoning(path string, entries []ReasoningEntry) error {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "Prompt: %s\n", oneLine(e.Prompt))
		fmt.Fprintf(&b, "Chosen provider: %s", e.Provider)
		if e.Note != "" {
			fmt.Fprintf(&b, " [%s]", e.Note)
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "Reasoning: %s\n", reasoningText(e.Reasoning))
		b.WriteString(reasoningSeparator + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// reasoningText is what the model wrote, or a stated absence. A missing
// reasoning is reported rather than left blank: the field is optional, so an
// empty line would read as an empty answer instead of as no answer.
func reasoningText(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(the model wrote none)"
	}
	return oneLine(s)
}

// oneLine keeps a block to its three lines: a model may answer with newlines in
// the string, and the file is read line by line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
