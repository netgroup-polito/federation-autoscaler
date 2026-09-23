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

// The agent-path check exercises ConsumerChoice exactly as a deployed consumer
// runs it, with nothing driven by the harness but the request for capacity:
//
//	console (manual reservation) -> ResourceRequest controller -> agent local API
//	  -> Broker (unmasked list) -> the agent's own LLM call -> masked list
//	  -> reservation on the chosen provider -> Liqo virtual node (phase Active)
//
// The agent logs every decision (localapi.SelectionFinishedMessage); the check
// reads that line to learn what the model chose and verifies the reservation
// landed there.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/federation-tests/testlib"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

const (
	agentPathPoll = 5 * time.Second
	// Manual reservation phases (api/autoscaling/v1alpha1 ResourceRequestPhase).
	manualPhaseReserved = "Reserved"
	manualPhaseActive   = "Active"
	manualPhaseFailed   = "Failed"

	failAgentSelectionNotLogged = "agent_selection_not_logged"
	failAgentUsedFallback       = "agent_used_fallback"
	failAgentNotActive          = "agent_reservation_not_active"
	failAgentOtherProvider      = "agent_reserved_other_provider"
	failAgentReleaseFailed      = "agent_release_or_settle_failed"
)

// AgentSelection is one ConsumerChoice decision as the consumer agent logged it.
type AgentSelection struct {
	LoggedAt    time.Time `json:"loggedAt"`
	Source      string    `json:"source"`
	Prompt      string    `json:"prompt"`
	Ranked      []string  `json:"ranked"`
	RawResponse string    `json:"rawResponse,omitempty"`
	ErrorKind   string    `json:"errorKind,omitempty"`
	Error       string    `json:"error,omitempty"`
	DurationMs  int64     `json:"durationMs"`
	Line        string    `json:"line"`
}

// ManualPhase is one observed phase of the manual reservation.
type ManualPhase struct {
	Phase    string    `json:"phase"`
	Provider string    `json:"provider,omitempty"`
	Message  string    `json:"message,omitempty"`
	SeenAt   time.Time `json:"seenAt"`
}

// AgentPathResult is the record of one agent-path check.
type AgentPathResult struct {
	Scenario         string          `json:"scenario"`
	UserRequest      string          `json:"userRequest"`
	StartedAt        time.Time       `json:"startedAt"`
	ReservationName  string          `json:"reservationName,omitempty"`
	Transitions      []ManualPhase   `json:"transitions"`
	FinalPhase       string          `json:"finalPhase,omitempty"`
	ReservedProvider string          `json:"reservedProvider,omitempty"`
	ActiveAfterMs    float64         `json:"activeAfterMs,omitempty"`
	Selection        *AgentSelection `json:"selection,omitempty"`
	// ExpectedProvider is the first provider of the agent's ranking that had
	// free capacity when the request was made: the one the agent must reserve.
	ExpectedProvider string `json:"expectedProvider,omitempty"`
	// ModelAnswerFailure is set when the agent fell back because the model's
	// answer was unusable for a reason of its own (see isModelAnswerKind): a
	// result about the model. The agent must still reserve the fallback's choice.
	ModelAnswerFailure bool            `json:"modelAnswerFailure,omitempty"`
	Criterion          CriterionResult `json:"criterion"`
	ReleasedAndSettled bool            `json:"releasedAndSettled"`
	Errors             []string        `json:"errors,omitempty"`
	Failures           []string        `json:"failures,omitempty"`
	Passed             bool            `json:"passed"`
}

// configureAgent points the consumer agent at this run's Ollama, with the same
// model and timeout the recorded decisions use.
func (s *suiteRun) configureAgent(ctx context.Context) error {
	url, err := s.rt.agentURL(ctx, s.orch.ConsumerContainerName(1))
	if err != nil {
		return fmt.Errorf("agent path: %w", err)
	}
	log.Printf("[ollama] agent will reach Ollama at %s", redactURL(url))
	o := s.cc.Ollama
	if err := testlib.SetConsumerOllama(ctx, s.orch.Specs[1].Kubeconfig, url, o.Model, o.Timeout); err != nil {
		return fmt.Errorf("agent path: configure the consumer agent's Ollama: %w", err)
	}
	s.agentOllamaURL = url
	return s.waitForConsole(ctx)
}

// waitForConsole waits until the restarted agent's console answers. The rollout
// is complete once the pod is Ready, but the NodePort can take a few more
// seconds to route to it, and the calls that follow do not retry.
func (s *suiteRun) waitForConsole(ctx context.Context) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, err := s.console.State(ctx)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("agent path: the consumer console did not answer within 2m of the agent restart: %w", err)
		}
		if err := testlib.SleepCtx(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

// runAgentPath places one manual reservation and checks that the agent's own
// LLM decision chose where it landed. It returns an error only for conditions
// that make continuing meaningless (a capacity leak, cancellation).
func (s *suiteRun) runAgentPath(ctx context.Context, sc Scenario) (*AgentPathResult, error) {
	res := &AgentPathResult{Scenario: sc.Name, UserRequest: sc.UserRequest, StartedAt: time.Now()}
	log.Printf("--- %s agent path: manual reservation decided by the agent's own LLM call ---", sc.Name)

	snapshot, err := s.broker.GetNodeGroups(ctx)
	if err != nil {
		return res, fmt.Errorf("agent path: read Broker node groups: %w", err)
	}
	before, err := s.console.State(ctx)
	if err != nil {
		return res, fmt.Errorf("agent path: read consumer state: %w", err)
	}
	existing := map[string]bool{}
	for _, r := range before.ManualReservations {
		existing[r.Name] = true
	}

	if err := s.console.ApplyManualReservation(ctx, s.cc.AgentPath.CPU, s.cc.AgentPath.Memory); err != nil {
		res.Errors = append(res.Errors, "apply manual reservation: "+err.Error())
	} else {
		s.trackManualReservation(ctx, res, existing)
	}

	selections, logErr := s.agentSelections(ctx, sc.UserRequest)
	if logErr != nil {
		res.Errors = append(res.Errors, "read agent log: "+logErr.Error())
	}
	res.Selection = selectionBefore(selections, reservedAt(res.Transitions))

	fatal := s.releaseAgentPath(ctx, res, existing)
	evaluateAgentPath(res, snapshot)

	candidates := buildCandidateMetrics(eligibleProviderInfos(snapshot), s.location, sc.ReferenceWeights)
	if res.ReservedProvider != "" {
		res.Criterion = evaluateCriterion(sc.Criterion, candidates, res.ReservedProvider)
	} else {
		res.Criterion = CriterionResult{Type: sc.Criterion.Type, Details: []string{"nothing was reserved"}}
	}
	log.Printf("[agent-path] source %s, agent's choice %q, reserved %q (%s), passed %v %v",
		selectionSource(res.Selection), res.ExpectedProvider, res.ReservedProvider, res.FinalPhase,
		res.Passed, res.Failures)
	if err := writeAgentPath(filepath.Join(s.orch.OutputDir, "scenarios", sc.Name, "agent-path"), res); err != nil {
		s.warnings = append(s.warnings, fmt.Sprintf("write agent-path artifacts for %s: %v", sc.Name, err))
	}
	return res, fatal
}

// trackManualReservation polls the console until the new manual reservation is
// Active or Failed, recording each phase it passes through.
func (s *suiteRun) trackManualReservation(ctx context.Context, res *AgentPathResult, existing map[string]bool) {
	deadline := time.Now().Add(s.cc.AgentPath.Timeout)
	for {
		if state, err := s.console.State(ctx); err == nil {
			if res.ReservationName == "" {
				res.ReservationName = firstNewReservation(state.ManualReservations, existing)
			}
			for _, r := range state.ManualReservations {
				if r.Name != res.ReservationName {
					continue
				}
				if n := len(res.Transitions); n == 0 || res.Transitions[n-1].Phase != r.Phase {
					res.Transitions = append(res.Transitions,
						ManualPhase{Phase: r.Phase, Provider: r.Provider, Message: r.Message, SeenAt: time.Now()})
					log.Printf("[agent-path] %s -> %s %s %s", r.Name, r.Phase, r.Provider, r.Message)
				}
				res.FinalPhase, res.ReservedProvider = r.Phase, r.Provider
			}
		}
		switch res.FinalPhase {
		case manualPhaseActive:
			res.ActiveAfterMs = ms(time.Since(res.StartedAt))
			return
		case manualPhaseFailed:
			return
		}
		if time.Now().After(deadline) {
			res.Errors = append(res.Errors, fmt.Sprintf("manual reservation not Active within %s (last phase %q)",
				s.cc.AgentPath.Timeout, res.FinalPhase))
			return
		}
		if testlib.SleepCtx(ctx, agentPathPoll) != nil {
			return
		}
	}
}

// repeatedRequests lists the user requests more than one scenario asks. It
// matters only for the agent path: the agent reuses its decision for the same
// request for up to a minute, so the second scenario's reservation may be
// placed on the first scenario's decision, and the check would read that older
// log line as if it were the new one.
func repeatedRequests(scenarios []Scenario) []string {
	seen, dup := map[string]bool{}, map[string]bool{}
	var out []string
	for _, sc := range scenarios {
		switch {
		case dup[sc.UserRequest]:
		case seen[sc.UserRequest]:
			dup[sc.UserRequest] = true
			out = append(out, sc.UserRequest)
		default:
			seen[sc.UserRequest] = true
		}
	}
	return out
}

// firstNewReservation is the console-managed reservation that was not there
// before the check started: the one it has just asked for.
func firstNewReservation(list []testlib.ManualReservation, existing map[string]bool) string {
	for _, r := range list {
		if !existing[r.Name] {
			return r.Name
		}
	}
	return ""
}

// releaseAgentPath gives the manual reservation back and waits for the
// federation to return to its baseline capacity. existing is what the console
// listed before the check, so a reservation whose name was never learnt (every
// state read failed, or the wait timed out) is still found and released rather
// than left holding a provider for the rest of the run.
func (s *suiteRun) releaseAgentPath(ctx context.Context, res *AgentPathResult, existing map[string]bool) error {
	if res.ReservationName == "" {
		if state, err := s.console.State(context.Background()); err == nil {
			if name := firstNewReservation(state.ManualReservations, existing); name != "" {
				res.ReservationName = name
				res.Errors = append(res.Errors, "reservation name learnt only at release time: "+name)
			}
		}
	}
	if res.ReservationName != "" {
		relCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := s.console.ReleaseManualReservation(relCtx, res.ReservationName); err != nil {
			res.Errors = append(res.Errors, "release manual reservation: "+err.Error())
		}
	}
	_, err := testlib.WaitForFederationCapacity(ctx, s.broker, s.baseline, s.orch.Config.Experiment.ReservationPoll,
		testlib.FederationSettleTimeout+2*time.Minute) // unpeering a Liqo node takes longer than a bare release
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("run interrupted during the %s agent path: %w", res.Scenario, ctx.Err())
	case err != nil:
		res.Errors = append(res.Errors, "capacity did not return to baseline: "+err.Error())
		return fmt.Errorf("agent path %s: capacity did not return to baseline: %w", res.Scenario, err)
	}
	res.ReleasedAndSettled = true
	return nil
}

// agentSelections reads the consumer agent's decisions for prompt since the run
// started, oldest first.
func (s *suiteRun) agentSelections(ctx context.Context, prompt string) ([]AgentSelection, error) {
	logCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(logCtx, "kubectl", "--kubeconfig", s.orch.Specs[1].Kubeconfig,
		"-n", agentNamespace, "logs", "deploy/agent", "--since-time="+s.started.UTC().Format(time.RFC3339)).Output()
	if err != nil {
		return nil, err
	}
	return parseAgentSelections(string(out), prompt), nil
}

// parseAgentSelections extracts, oldest first, the agent's selection lines for
// prompt from its log. The agent logs with zap's console encoder:
//
//	<RFC 3339 time>\tINFO\t<logger>\tConsumerChoice selection finished\t{"source": "ai", ...}
func parseAgentSelections(logText, prompt string) []AgentSelection {
	// Not pre-allocated on purpose: an agent log is thousands of lines and
	// only a handful of them are selections, so sizing to the log would
	// allocate far more than this ever holds.
	var out []AgentSelection //nolint:prealloc // see above
	for _, line := range strings.Split(logText, "\n") {
		line = strings.TrimRight(line, "\r")
		at := strings.Index(line, localapi.SelectionFinishedMessage)
		if at < 0 {
			continue
		}
		brace := strings.Index(line[at:], "{")
		if brace < 0 {
			continue
		}
		var sel AgentSelection
		if json.Unmarshal([]byte(line[at+brace:]), &sel) != nil || sel.Prompt != prompt {
			continue
		}
		if ts, _, ok := strings.Cut(line, "\t"); ok {
			sel.LoggedAt, _ = time.Parse(time.RFC3339Nano, strings.TrimSpace(ts))
		}
		sel.Line = line
		out = append(out, sel)
	}
	return out
}

// reservedAt is when the manual reservation was first seen holding a provider.
func reservedAt(transitions []ManualPhase) time.Time {
	for _, t := range transitions {
		if t.Phase == manualPhaseReserved || t.Phase == manualPhaseActive {
			return t.SeenAt
		}
	}
	return time.Time{}
}

// selectionBefore is the decision the reservation was placed on: the latest one
// logged before the reservation was seen holding a provider. Later decisions
// (the controller re-reads the list while it holds the reservation) did not
// place it. With no reservation, or no timestamps, it is the latest one.
func selectionBefore(selections []AgentSelection, at time.Time) *AgentSelection {
	var chosen *AgentSelection
	for i := range selections {
		sel := &selections[i]
		if !at.IsZero() && !sel.LoggedAt.IsZero() && sel.LoggedAt.After(at) {
			break
		}
		chosen = sel
	}
	return chosen
}

// evaluateAgentPath decides whether the agent path worked: the agent asked the
// model, the reservation became Active on the provider the model ranked first
// among those with capacity, and it was given back cleanly.
func evaluateAgentPath(res *AgentPathResult, snapshot *brokerapi.NodeGroupListResponse) {
	res.Failures = nil
	switch {
	case res.Selection == nil:
		res.Failures = append(res.Failures, failAgentSelectionNotLogged)
	case res.Selection.Source != localapi.SelectionSourceAI && !isModelAnswerKind(res.Selection.ErrorKind):
		// The model was not reached or could not be used for a reason of the
		// system's: no Ollama configured, unreachable, or answering with an error.
		res.Failures = append(res.Failures, failAgentUsedFallback)
	default:
		res.ModelAnswerFailure = res.Selection.Source != localapi.SelectionSourceAI
		res.ExpectedProvider = firstWithCapacity(res.Selection.Ranked, snapshot)
	}
	if res.FinalPhase != manualPhaseActive {
		res.Failures = append(res.Failures, failAgentNotActive)
	}
	if res.ExpectedProvider != "" && res.ReservedProvider != res.ExpectedProvider {
		res.Failures = append(res.Failures, failAgentOtherProvider)
	}
	if !res.ReleasedAndSettled {
		res.Failures = append(res.Failures, failAgentReleaseFailed)
	}
	res.Passed = len(res.Failures) == 0
}

// agentPathReasons are the verdict's reasons to fail on account of the agent
// path: a check that did not run for every scenario, or did not pass.
func agentPathReasons(enabled bool, scenarios int, results []*AgentPathResult) []string {
	if !enabled {
		return nil
	}
	var reasons []string
	if len(results) < scenarios {
		reasons = append(reasons, fmt.Sprintf("the agent path ran for only %d of %d scenarios", len(results), scenarios))
	}
	for _, r := range results {
		if r != nil && !r.Passed {
			reasons = append(reasons, fmt.Sprintf("%s agent path failed: %s", r.Scenario, strings.Join(r.Failures, ", ")))
		}
	}
	return reasons
}

// firstWithCapacity is the first provider of ranked whose standard node group
// had free capacity in snapshot -- what the agent's masking lets through.
func firstWithCapacity(ranked []string, snapshot *brokerapi.NodeGroupListResponse) string {
	for _, id := range ranked {
		if ng := standardGroup(snapshot.NodeGroups, id); ng != nil && ng.MaxSize > ng.CurrentReserved {
			return id
		}
	}
	return ""
}

func eligibleProviderInfos(snapshot *brokerapi.NodeGroupListResponse) []ollama.ProviderInfo {
	var out []ollama.ProviderInfo
	for _, ng := range snapshot.NodeGroups {
		if ng.Type == brokerv1alpha1.ChunkTypeStandard && ng.MaxSize > ng.CurrentReserved {
			out = append(out, ollama.NodeGroupViewToProviderInfo(ng))
		}
	}
	return out
}

func selectionSource(sel *AgentSelection) string {
	if sel == nil {
		return "none logged"
	}
	if sel.ErrorKind != "" {
		return sel.Source + " (" + sel.ErrorKind + ")"
	}
	return sel.Source
}

func writeAgentPath(dir string, res *AgentPathResult) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := testlib.WriteJSONFile(dir, "resource_request.json", res); err != nil {
		return err
	}
	decision := map[string]any{"selection": res.Selection, "expectedProvider": res.ExpectedProvider,
		"note": "the consumer agent's own LLM decision, from its log; the harness did not call the model here"}
	return testlib.WriteJSONFile(dir, "agent_decision.json", decision)
}
