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
	"bytes"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
	crzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/federation-tests/testlib"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// agentLog is the consumer agent's log as zap's development console encoder
// writes it (time, level, logger, caller, message, fields), with unrelated
// lines around the decisions.
// providerOne is the provider these tests reserve on or rank last.
const providerOne = "provider-1"

const agentLog = "2026-09-15T18:29:58.000000000Z\tINFO\tconsumer.heartbeat\theartbeat/heartbeat.go:240\tbeat\t{}\n" +
	"2026-09-15T18:30:00.100000000Z\tINFO\tconsumer.localapi\tlocalapi/server.go:320\t" +
	"ConsumerChoice selection finished\t{\"source\": \"ai\", \"prompt\": \"close to me\", " +
	"\"ranked\": [\"provider-2\", \"provider-3\", \"provider-1\"], " +
	"\"rawResponse\": \"{\\\"rankedList\\\":[\\\"provider-2\\\",\\\"provider-3\\\"]}\", " +
	"\"errorKind\": \"\", \"error\": \"\", \"durationMs\": 21345}\r\n" +
	"2026-09-15T18:30:05.000000000Z\tINFO\tconsumer.localapi\tlocalapi/server.go:320\t" +
	"ConsumerChoice selection finished\t{\"source\": \"ai\", \"prompt\": \"the greenest\", " +
	"\"ranked\": [\"provider-9\"], \"durationMs\": 900}\n" +
	"2026-09-15T18:40:00.000000000Z\tINFO\tconsumer.localapi\tlocalapi/server.go:320\t" +
	"ConsumerChoice selection finished\t{\"source\": \"fallback\", \"prompt\": \"close to me\", " +
	"\"ranked\": [\"provider-8\"], \"errorKind\": \"timeout\", \"durationMs\": 120000}\n" +
	"a line mentioning ConsumerChoice selection finished but carrying no fields\n"

func TestParseAgentSelections(t *testing.T) {
	got := parseAgentSelections(agentLog, "close to me")
	if len(got) != 2 {
		t.Fatalf("want the 2 decisions for this prompt, got %d: %+v", len(got), got)
	}
	first := got[0]
	if first.Source != localapi.SelectionSourceAI ||
		strings.Join(first.Ranked, ",") != "provider-2,provider-3,provider-1" ||
		first.DurationMs != 21345 || !strings.Contains(first.RawResponse, "rankedList") {
		t.Errorf("first decision parsed wrong: %+v", first)
	}
	if want := time.Date(2026, 9, 15, 18, 30, 0, 100000000, time.UTC); !first.LoggedAt.Equal(want) {
		t.Errorf("loggedAt = %s, want %s", first.LoggedAt, want)
	}
	if got[1].Source != localapi.SelectionSourceFallback || got[1].ErrorKind != "timeout" {
		t.Errorf("second decision parsed wrong: %+v", got[1])
	}
}

// The parser must read what the agent really writes: the same keys the local
// API logs, through the logger the agent builds (controller-runtime zap,
// development mode, rfc3339nano times as its Deployment sets).
func TestParseAgentSelections_RealAgentLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := crzap.New(crzap.UseDevMode(true), crzap.WriteTo(&buf), func(o *crzap.Options) {
		o.TimeEncoder = zapcore.RFC3339NanoTimeEncoder
	}).WithName("consumer").WithName("localapi")

	logger.Info(localapi.SelectionFinishedMessage,
		"source", localapi.SelectionSourceAI,
		"prompt", `close to "me"`,
		"ranked", []string{"provider-2", "provider-3"},
		"rawResponse", `{"rankedList":["provider-2","provider-3"],"providerId":"provider-2"}`,
		"errorKind", "",
		"error", "",
		"durationMs", int64(1234))

	got := parseAgentSelections(buf.String(), `close to "me"`)
	if len(got) != 1 {
		t.Fatalf("the real log line was not parsed:\n%s", buf.String())
	}
	if strings.Join(got[0].Ranked, ",") != "provider-2,provider-3" || got[0].DurationMs != 1234 ||
		got[0].LoggedAt.IsZero() || !strings.Contains(got[0].RawResponse, "providerId") {
		t.Errorf("parsed %+v from:\n%s", got[0], buf.String())
	}
}

// The reservation was placed on the decision made before it held a provider,
// not on a later one made while the reservation was being held.
func TestSelectionBefore(t *testing.T) {
	selections := parseAgentSelections(agentLog, "close to me")
	reserved := time.Date(2026, 9, 15, 18, 31, 0, 0, time.UTC)
	if sel := selectionBefore(selections, reserved); sel == nil || sel.Source != localapi.SelectionSourceAI {
		t.Errorf("want the 18:30 AI decision, got %+v", sel)
	}
	if sel := selectionBefore(selections, time.Time{}); sel == nil || sel.Source != localapi.SelectionSourceFallback {
		t.Errorf("without a reservation time the latest decision applies, got %+v", sel)
	}
	if sel := selectionBefore(nil, reserved); sel != nil {
		t.Errorf("no decisions logged must give none, got %+v", sel)
	}
}

func agentSnapshot() *brokerapi.NodeGroupListResponse {
	ng := func(id string, maxSize, reserved int32) brokerapi.NodeGroupView {
		return brokerapi.NodeGroupView{ID: "ng-" + id, ProviderClusterID: id, Type: brokerv1alpha1.ChunkTypeStandard,
			MaxSize: maxSize, CurrentReserved: reserved}
	}
	return &brokerapi.NodeGroupListResponse{NodeGroups: []brokerapi.NodeGroupView{
		ng(providerOne, 2, 0), ng("provider-2", 2, 2), ng("provider-3", 3, 0),
	}}
}

func TestEvaluateAgentPath(t *testing.T) {
	aiRanking := &AgentSelection{Source: localapi.SelectionSourceAI,
		Ranked: []string{"provider-2", "provider-3", providerOne}}
	ok := func() *AgentPathResult {
		// provider-2 was full, so the agent's masking let provider-3 through.
		return &AgentPathResult{Selection: aiRanking, FinalPhase: manualPhaseActive,
			ReservedProvider: "provider-3", ReleasedAndSettled: true}
	}

	res := ok()
	evaluateAgentPath(res, agentSnapshot())
	if !res.Passed || res.ExpectedProvider != "provider-3" || res.ModelAnswerFailure {
		t.Errorf("the first ranked provider with capacity was reserved: must pass, got %+v", res)
	}

	// The model looped until the timeout: a result about the model. The agent
	// fell back, and must reserve the fallback ranking's first free provider.
	res = ok()
	res.Selection = &AgentSelection{Source: localapi.SelectionSourceFallback, ErrorKind: "timeout",
		Ranked: []string{"provider-2", "provider-3", providerOne}}
	evaluateAgentPath(res, agentSnapshot())
	if !res.Passed || !res.ModelAnswerFailure || res.ExpectedProvider != "provider-3" {
		t.Errorf("a fallback after the model's own failure must pass and be marked, got %+v", res)
	}
	res.ReservedProvider = providerOne
	evaluateAgentPath(res, agentSnapshot())
	if res.Passed || !strings.Contains(strings.Join(res.Failures, ","), failAgentOtherProvider) {
		t.Errorf("after a fallback the reservation must still follow its ranking, got %+v", res)
	}

	for name, tc := range map[string]struct {
		mutate func(*AgentPathResult)
		want   string
	}{
		"no decision logged": {func(r *AgentPathResult) { r.Selection = nil }, failAgentSelectionNotLogged},
		"fallback without Ollama": {func(r *AgentPathResult) {
			r.Selection = &AgentSelection{Source: localapi.SelectionSourceFallback, Ranked: []string{"provider-3"}}
		}, failAgentUsedFallback},
		"fallback, Ollama unreachable": {func(r *AgentPathResult) {
			r.Selection = &AgentSelection{Source: localapi.SelectionSourceFallback, ErrorKind: "unreachable",
				Ranked: []string{"provider-3"}}
		}, failAgentUsedFallback},
		"never became Active": {func(r *AgentPathResult) { r.FinalPhase = "Pending" }, failAgentNotActive},
		"landed elsewhere":    {func(r *AgentPathResult) { r.ReservedProvider = providerOne }, failAgentOtherProvider},
		"not given back":      {func(r *AgentPathResult) { r.ReleasedAndSettled = false }, failAgentReleaseFailed},
	} {
		t.Run(name, func(t *testing.T) {
			res := ok()
			tc.mutate(res)
			evaluateAgentPath(res, agentSnapshot())
			if res.Passed || !strings.Contains(strings.Join(res.Failures, ","), tc.want) {
				t.Errorf("want failure %s, got passed=%v %v", tc.want, res.Passed, res.Failures)
			}
		})
	}
}

func TestFirstNewReservation(t *testing.T) {
	existing := map[string]bool{"rr-old": true}
	for name, tc := range map[string]struct {
		list []testlib.ManualReservation
		want string
	}{
		"nothing listed":     {nil, ""},
		"only the old one":   {[]testlib.ManualReservation{{Name: "rr-old"}}, ""},
		"the new one":        {[]testlib.ManualReservation{{Name: "rr-new"}}, "rr-new"},
		"old and new listed": {[]testlib.ManualReservation{{Name: "rr-old"}, {Name: "rr-new"}}, "rr-new"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := firstNewReservation(tc.list, existing); got != tc.want {
				t.Errorf("firstNewReservation = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRepeatedRequests(t *testing.T) {
	scenarios := []Scenario{
		{Name: "a", UserRequest: "the greenest"},
		{Name: "b", UserRequest: "close to me"},
		{Name: "c", UserRequest: "the greenest"},
		{Name: "d", UserRequest: "the greenest"},
	}
	if got := repeatedRequests(scenarios); len(got) != 1 || got[0] != "the greenest" {
		t.Errorf("repeatedRequests = %v, want the one repeated request", got)
	}
	if got := repeatedRequests(scenarios[:2]); got != nil {
		t.Errorf("distinct requests must give none, got %v", got)
	}
}

func TestAgentPathReasons(t *testing.T) {
	if r := agentPathReasons(false, 4, nil); r != nil {
		t.Errorf("a disabled agent path gives no reasons, got %v", r)
	}
	passed := &AgentPathResult{Scenario: "eco", Passed: true}
	failed := &AgentPathResult{Scenario: "balanced", Failures: []string{failAgentUsedFallback}}
	reasons := strings.Join(agentPathReasons(true, 3, []*AgentPathResult{passed, failed}), "\n")
	if !strings.Contains(reasons, "only 2 of 3 scenarios") || !strings.Contains(reasons, "balanced agent path failed") ||
		strings.Contains(reasons, "eco agent path") {
		t.Errorf("unexpected reasons:\n%s", reasons)
	}
}
