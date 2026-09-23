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
	"errors"
	"strings"
	"testing"

	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
)

// answerTrace is a selector trace over provider-1 and provider-2 in which the
// model answered parsed; ranked and unknown are what the selector made of it.
func answerTrace(parsed *ollama.SelectionResponse, ranked, unknown []string) *ollama.Trace {
	return &ollama.Trace{
		Providers:  []ollama.ProviderInfo{{ProviderID: "provider-1"}, {ProviderID: "provider-2"}},
		Parsed:     parsed,
		Ranked:     ranked,
		UnknownIDs: unknown,
	}
}

func TestValidateAnswer_InventedIDInFirstPlaceIsInvalid(t *testing.T) {
	// The selector drops provider-99 and would act on provider-2, but the model
	// chose provider-99: that choice is what gets judged, and it is invalid.
	trace := answerTrace(&ollama.SelectionResponse{RankedList: []string{"provider-99", "provider-2"}},
		[]string{"provider-2"}, []string{"provider-99"})

	v, failures := validateAnswer(trace, nil)
	if v.SelectedProviderID != "provider-99" || v.InCandidateList {
		t.Errorf("selection = %q (in list %v), want the invented provider-99, not in the list",
			v.SelectedProviderID, v.InCandidateList)
	}
	if got := strings.Join(failures, ","); got != string(ollama.ErrKindUnknownProviderID) {
		t.Errorf("failures = %q, want unknown_provider_id", got)
	}
}

func TestValidateAnswer_InventedIDFurtherDownIsRecordedOnly(t *testing.T) {
	answer := &ollama.SelectionResponse{RankedList: []string{"provider-2", "provider-99"}, ProviderID: "provider-2"}
	trace := answerTrace(answer, []string{"provider-2"}, []string{"provider-99"})

	v, failures := validateAnswer(trace, nil)
	if v.SelectedProviderID != "provider-2" || !v.InCandidateList || len(failures) != 0 {
		t.Errorf("a valid first choice must pass: selection %q, in list %v, failures %v",
			v.SelectedProviderID, v.InCandidateList, failures)
	}
	if got := strings.Join(v.UnknownIDsInRanking, ","); got != "provider-99" {
		t.Errorf("unknownIdsInRanking = %q, want provider-99", got)
	}
	if v.RankingConsistent == nil || !*v.RankingConsistent {
		t.Error("providerId equals the ranking head, so the answer is consistent")
	}
}

func TestValidateAnswer_ProviderIDDisagreeingWithRanking(t *testing.T) {
	answer := &ollama.SelectionResponse{RankedList: []string{"provider-1", "provider-2"}, ProviderID: "provider-2"}
	trace := answerTrace(answer, []string{"provider-1", "provider-2"}, nil)

	v, _ := validateAnswer(trace, nil)
	if v.SelectedProviderID != "provider-1" {
		t.Errorf("selection = %q, want the ranking head provider-1 (what the agent acts on)", v.SelectedProviderID)
	}
	if v.RankingConsistent == nil || *v.RankingConsistent {
		t.Error("providerId differs from the ranking head: consistency must be recorded as false")
	}
}

func TestValidateAnswer_EmptyFirstEntry(t *testing.T) {
	trace := answerTrace(&ollama.SelectionResponse{RankedList: []string{"", "provider-2"}},
		[]string{"provider-2"}, []string{""})

	v, failures := validateAnswer(trace, nil)
	if v.NonEmpty || strings.Join(failures, ",") != string(ollama.ErrKindEmptyProviderID) {
		t.Errorf("an empty first entry must fail as empty_provider_id: nonEmpty %v, failures %v", v.NonEmpty, failures)
	}
}

func TestValidateAnswer_SelectorErrorKeepsItsKind(t *testing.T) {
	trace := answerTrace(nil, nil, nil)
	trace.ErrorKind = ollama.ErrKindInvalidJSON

	v, failures := validateAnswer(trace, errors.New("parse LLM selection JSON"))
	if v.ValidJSON || v.NonEmpty || strings.Join(failures, ",") != string(ollama.ErrKindInvalidJSON) {
		t.Errorf("validJson %v, nonEmpty %v, failures %v; want false, false, [invalid_json]",
			v.ValidJSON, v.NonEmpty, failures)
	}
}

func TestValidateAnswer_SingleCandidate(t *testing.T) {
	trace := &ollama.Trace{
		Providers:       []ollama.ProviderInfo{{ProviderID: "provider-1"}},
		Ranked:          []string{"provider-1"},
		SingleCandidate: true,
	}
	v, failures := validateAnswer(trace, nil)
	if v.SelectedProviderID != "provider-1" || !v.InCandidateList || len(failures) != 0 {
		t.Errorf("single candidate: selection %q, in list %v, failures %v", v.SelectedProviderID, v.InCandidateList, failures)
	}
}

// An answer that failed only through the model's own doing is a result about
// the model; anything the system caused, alone or alongside, is not.
func TestDecision_ModelAnswerFailure(t *testing.T) {
	decision := func(valid bool, failures ...string) *Decision {
		return &Decision{Trace: answerTrace(nil, nil, nil), Validation: Validation{Valid: valid}, Failures: failures}
	}
	for name, tc := range map[string]struct {
		d    *Decision
		want bool
	}{
		"loop, cut by the timeout": {decision(false, string(ollama.ErrKindTimeout)), true},
		"broken JSON":              {decision(false, string(ollama.ErrKindInvalidJSON)), true},
		"no provider named":        {decision(false, string(ollama.ErrKindEmptyProviderID)), true},
		"invented provider":        {decision(false, string(ollama.ErrKindUnknownProviderID)), true},
		"Ollama unreachable":       {decision(false, string(ollama.ErrKindUnreachable)), false},
		"Ollama HTTP error":        {decision(false, string(ollama.ErrKindHTTPStatus)), false},
		"Broker changed the policy": {decision(false, string(ollama.ErrKindInvalidJSON), failPolicyChanged),
			false},
		"fallback found nothing": {decision(false, string(ollama.ErrKindTimeout), failFallbackNoCandidate), false},
		"valid answer":           {decision(true), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.d.ModelAnswerFailure(); got != tc.want {
				t.Errorf("ModelAnswerFailure(%v) = %v, want %v", tc.d.Failures, got, tc.want)
			}
		})
	}

	single := decision(false, string(ollama.ErrKindTimeout))
	single.Trace.SingleCandidate = true
	if single.ModelAnswerFailure() {
		t.Error("a model that was never asked cannot have answered badly")
	}
}
