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

// Command consumerchoice validates the ConsumerChoice placement policy end to
// end: real provider agents advertise, the Broker hands the consumer every
// eligible provider, the consumer's own selector (internal/agent/ollama) asks a
// local LLM to choose one for a natural-language request, the choice is
// validated as untrusted input, and one real reservation is driven to Peered
// through the normal Broker/agent/Liqo workflow. Everything is recorded for
// later audit.
//
// Usage:
//
//	./federation-tests/consumerchoice/run-consumerchoice.sh --config federation-tests/consumerchoice/configs/default.yaml
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/netgroup-polito/federation-autoscaler/federation-tests/testlib"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
)

const consumerID = "consumer-1"

// The two verdicts a run can end with, as written in summary.json/summary.md
// and reflected in the process exit status.
const (
	verdictPass = "PASS"
	verdictFail = "FAIL"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run holds the program body so every deferred cleanup runs on every return
// path: log.Fatal in main calls os.Exit, which would skip them.
func run() error {
	f := parseFlags()
	cfg, cc, err := loadConfig(f.configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	keep := f.keepClusters || (cfg.Cleanup != nil && !*cfg.Cleanup)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	orch, err := testlib.NewOrchestrator(cfg, testType, keep, f.skipBuild, f.runID)
	if err != nil {
		return fmt.Errorf("orchestrator: %w", err)
	}
	defer orch.Teardown(context.Background())

	// Start the model runtime before the clusters so a first-run model download
	// overlaps the (much longer) cluster creation.
	rt, err := startOllama(ctx, cc.Ollama, orch.RunID)
	if err != nil {
		return fmt.Errorf("ollama: %w", err)
	}
	defer rt.stop(context.Background(), keep)

	// A model that cannot be served is known long before the clusters are up:
	// its failure cancels the setup instead of waiting for it to finish.
	setupCtx, cancelSetup := context.WithCancel(ctx)
	defer cancelSetup()
	rt.beginPreparation(ctx, cancelSetup)

	if err := orch.Setup(setupCtx); err != nil {
		if perr := rt.preparationFailure(); perr != nil {
			return fmt.Errorf("ollama: %w", perr)
		}
		return fmt.Errorf("setup failed: %w", err)
	}
	return (&suiteRun{orch: orch, cc: cc, rt: rt, keep: keep}).execute(ctx)
}

type suiteRun struct {
	orch     *testlib.Orchestrator
	cc       *ChoiceConfig
	rt       *ollamaRuntime
	keep     bool
	started  time.Time
	broker   *testlib.BrokerClient
	console  *testlib.ConsoleClient
	consumer *testlib.ConsumerLocation
	location *ollama.Location
	baseline testlib.FederationCapacity
	records  []*RepetitionRecord
	warnings []string
	// agentOllamaURL is where the consumer agent reaches Ollama; agentPaths
	// are the agent-path checks, one per scenario that got that far.
	agentOllamaURL string
	agentPaths     []*AgentPathResult
}

func (s *suiteRun) execute(ctx context.Context) error {
	s.started = time.Now()
	out := s.orch.OutputDir
	log.Printf("=== CONSUMERCHOICE RUN %s (results in %s) ===", s.orch.RunID, out)
	defer collectLogs(s.orch, s.rt, out)

	if err := s.writeConfiguration(); err != nil {
		return err
	}
	fatal := s.prepare(ctx)
	if fatal == nil {
		fatal = s.runScenarios(ctx)
	}
	return s.finish(fatal)
}

// prepare brings the federation into the state every scenario starts from.
func (s *suiteRun) prepare(ctx context.Context) error {
	clients := s.orch.Clients
	s.console = clients.Consoles[consumerID]
	s.broker = clients.BrokerFor(consumerID)
	if s.console == nil || s.broker == nil {
		return fmt.Errorf("no console or Broker client for %s", consumerID)
	}
	// The model must be ready before the Ollama container joins the Kind network
	// below: joining can move the port the background preparation talks to, and
	// nothing may still be using it when that happens.
	log.Printf("[ollama] waiting for model %s", s.cc.Ollama.Model)
	if err := s.rt.awaitPrepared(ctx); err != nil {
		return fmt.Errorf("ollama: %w", err)
	}
	// The consumer agent restarts to pick up its Ollama settings, so this comes
	// before anything that depends on the agent (policy, location).
	if s.cc.AgentPath.IsEnabled() {
		for _, request := range repeatedRequests(s.cc.Scenarios) {
			s.warnings = append(s.warnings, fmt.Sprintf(
				"more than one scenario asks %q: the agent reuses its decision for the same request "+
					"for up to 60s, so an agent-path check may be judged on the earlier decision", request))
		}
		if err := s.configureAgent(ctx); err != nil {
			return err
		}
	}
	// ConsumerChoice goes first. It also replaces any stale policy, and it is the
	// only policy under which the Broker lists every provider's real capacity:
	// any other one masks all but one, and the metadata wait below could never
	// see the capacity it checks.
	if err := activateConsumerChoice(ctx, s.console, s.broker, s.cc.Scenarios[0], s.cc.PolicyTimeout); err != nil {
		return err
	}

	mockEcoURL, err := s.orch.MockEcoURL(ctx)
	if err != nil {
		return fmt.Errorf("mock-eco URL: %w", err)
	}
	if err := applyProviderProfiles(ctx, s.orch, s.cc, testlib.NewMockEcoClient(mockEcoURL)); err != nil {
		return err
	}
	if err := waitForProviderMetadata(ctx, s.broker, s.cc, s.cc.MetadataTimeout); err != nil {
		return err
	}

	// Again, just before the first decision: the model may have been unloaded
	// while the clusters were being created.
	if err := s.rt.warmUp(ctx, true); err != nil {
		return fmt.Errorf("ollama: %w", err)
	}
	s.warnings = append(s.warnings, s.rt.takeWarnings()...)
	if err := s.writeConfiguration(); err != nil { // now with the server version
		return err
	}

	state, err := s.console.State(ctx)
	if err != nil {
		return fmt.Errorf("read consumer state: %w", err)
	}
	s.consumer = state.Location
	if s.consumer.HasCoordinates() {
		s.location = &ollama.Location{Latitude: s.consumer.Lat, Longitude: s.consumer.Lon, Region: s.consumer.Region}
		log.Printf("[consumer] %s located in %s (%s), lat %.4f lon %.4f",
			consumerID, s.consumer.City, s.consumer.Region, s.consumer.Lat, s.consumer.Lon)
	} else {
		for _, sc := range s.cc.Scenarios {
			if sc.NeedsDistance() {
				return fmt.Errorf("scenario %s needs distances but %s has no discovered location",
					sc.Name, consumerID)
			}
		}
		s.warnings = append(s.warnings, "consumer location unknown: the model was sent no consumer location")
	}

	s.baseline, err = testlib.ReadFederationCapacity(ctx, s.broker)
	if err != nil {
		return fmt.Errorf("baseline capacity: %w", err)
	}
	return nil
}

func (s *suiteRun) runScenarios(ctx context.Context) error {
	for idx, sc := range s.cc.Scenarios {
		log.Printf("=== SCENARIO %s: %q ===", sc.Name, sc.UserRequest)
		if err := activateConsumerChoice(ctx, s.console, s.broker, sc, s.cc.PolicyTimeout); err != nil {
			return err
		}
		for rep := 1; rep <= s.cc.Repetitions; rep++ {
			rec, err := s.runRepetition(ctx, idx, sc, rep)
			if rec != nil {
				s.records = append(s.records, rec)
			}
			if err != nil {
				return err
			}
		}
		if s.cc.AgentPath.IsEnabled() {
			res, err := s.runAgentPath(ctx, sc)
			s.agentPaths = append(s.agentPaths, res)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// reservationID names the reservation of one repetition. It identifies the
// scenario by position, not by name: the ID reaches Kubernetes as a label value
// and, as "rs-<id>", as the virtual node's name, both capped at 63 characters,
// and a scenario name would push it past that.
func reservationID(runID string, scenarioIdx, rep int) string {
	return testlib.MakeReservationID(runID, fmt.Sprintf("cc%d", scenarioIdx+1), consumerID, rep)
}

// runRepetition is one decision and at most one reservation. It returns an
// error only for conditions that make continuing meaningless (the Broker stops
// honouring ConsumerChoice, capacity leaks, the run is cancelled); an invalid
// model answer or a failed reservation is a recorded result, not a stop.
func (s *suiteRun) runRepetition(ctx context.Context, scenarioIdx int, sc Scenario,
	rep int) (*RepetitionRecord, error) {
	exp := s.orch.Config.Experiment
	rec := &RepetitionRecord{
		Scenario:   sc,
		Repetition: rep,
		Dir:        filepath.Join(s.orch.OutputDir, "scenarios", sc.Name, fmt.Sprintf("rep-%02d", rep)),
		Consumer:   s.consumer,
	}
	log.Printf("--- %s repetition %d/%d ---", sc.Name, rep, s.cc.Repetitions)

	snapshot, err := s.broker.GetNodeGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("read Broker node groups: %w", err)
	}
	if err := assertUnmasked(snapshot, s.cc, s.baseline); err != nil {
		return nil, err
	}
	rec.BrokerSnapshot = snapshot
	candidates := eligibleCandidates(snapshot)
	log.Printf("[decision] asking %s to choose among %d eligible providers", s.cc.Ollama.Model, len(candidates))

	d := decide(ctx, s.rt, s.broker, s.cc, sc, candidates, s.location)
	rec.Decision = d
	rec.Candidates = buildCandidateMetrics(d.Trace.Providers, s.location, sc.ReferenceWeights)
	if d.FinalProviderID != "" {
		rec.Criterion = evaluateCriterion(sc.Criterion, rec.Candidates, d.FinalProviderID)
	} else {
		rec.Criterion = CriterionResult{Type: sc.Criterion.Type, Details: []string{"no provider was selected"}}
	}
	log.Printf("[decision] model chose %q, final %q (source %s, valid %v, %s), criterion %s",
		d.Validation.SelectedProviderID, d.FinalProviderID, d.Source, d.Validation.Valid,
		d.DecisionLatency.Round(time.Millisecond), criterionString(rec.Criterion.Passed))

	o := RepetitionOutcome{
		Scenario:           sc.Name,
		Repetition:         rep,
		AICalled:           d.AICalled(),
		SingleCandidate:    d.Trace.SingleCandidate,
		AIValid:            d.AICalled() && d.Validation.Valid,
		ModelAnswerFailure: d.ModelAnswerFailure(),
		Source:             d.Source,
		FinalProviderID:    d.FinalProviderID,
		FallbackUsed:       d.Validation.FallbackApplied,
		DecisionLatencyMs:  ms(d.DecisionLatency),
		CriterionPassed:    rec.Criterion.Passed,
		CandidateCount:     len(candidates),
		FailureCategories:  append([]string(nil), d.Failures...),
	}
	if o.AICalled {
		o.AISelectedID = d.Validation.SelectedProviderID
	}
	for _, c := range rec.Candidates {
		if c.ProviderID == d.FinalProviderID {
			o.CarbonRank, o.DistanceRank = c.CarbonRank, c.DistanceRank
			o.CostRank, o.ReferenceRank = c.CostRank, c.ReferenceRank
		}
	}

	var fatal error
	if d.FinalNodeGroup != nil {
		resID := reservationID(s.orch.RunID, scenarioIdx, rep)
		res := reserveAndTrack(ctx, s.broker, resID, d.FinalNodeGroup, exp.ReservationPoll, exp.ReservationTimeout)
		rec.Reservation = res
		o.ReservationAttempt = true
		o.Peered = res.Peered
		o.PeeringLatencyMs = res.PeeringDurationMs
		if res.FailureCategory != "" {
			o.FailureCategories = append(o.FailureCategories, res.FailureCategory)
		}
		if res.LastResponse != nil { // the Broker holds it: give the capacity back
			release(s.broker, res, exp.ReservationPoll)
			if res.ReleaseError != "" {
				o.FailureCategories = append(o.FailureCategories, failReleaseError)
			}
		}
		_, err := testlib.WaitForFederationCapacity(ctx, s.broker, s.baseline, exp.ReservationPoll,
			testlib.FederationSettleTimeout)
		switch {
		case ctx.Err() != nil:
			// Interrupted: the release above already ran on its own context, and
			// an unfinished settle wait is not evidence of a leak.
			fatal = fmt.Errorf("run interrupted during %s: %w", resID, ctx.Err())
		case err != nil:
			o.FailureCategories = append(o.FailureCategories, failCapacityLeak)
			fatal = fmt.Errorf("capacity did not return to baseline after %s: %w", resID, err)
		default:
			o.ReleasedAndSettled = true
		}
	}
	rec.Outcome = o

	if err := writeRepetition(rec, s.rt); err != nil {
		s.warnings = append(s.warnings, fmt.Sprintf("write artifacts for %s rep %d: %v", sc.Name, rep, err))
	}
	return rec, fatal
}

// finish writes the run-level files and turns the results into the exit status.
func (s *suiteRun) finish(fatal error) error {
	outcomes := make([]RepetitionOutcome, 0, len(s.records))
	var rows [][]string
	for _, rec := range s.records {
		outcomes = append(outcomes, rec.Outcome)
		rows = append(rows, comparisonRows(rec)...)
	}
	if drift := federationDrift(s.records); len(drift) > 0 {
		s.warnings = append(s.warnings, fmt.Sprintf("provider data changed during the run, so providers.csv (the "+
			"first decision's data) does not hold for: %s", strings.Join(drift, "; ")))
	}
	if drift := promptDrift(s.records); len(drift) > 0 {
		s.warnings = append(s.warnings, fmt.Sprintf("the prompt sent differs from its scenario's prompt.txt "+
			"(written from repetition 1) in: %s", strings.Join(drift, "; ")))
	}
	names := make([]string, len(s.cc.Scenarios))
	for i, sc := range s.cc.Scenarios {
		names[i] = sc.Name
	}
	metrics := computeMetrics(outcomes, names)
	verdict, reasons := s.verdict(metrics, outcomes, fatal)

	summary := RunSummary{
		RunID:       s.orch.RunID,
		StartedAt:   s.started,
		FinishedAt:  time.Now(),
		Model:       s.cc.Ollama.Model,
		Verdict:     verdict,
		Reasons:     reasons,
		Metrics:     metrics,
		Repetitions: outcomes,
		AgentPath:   s.agentPaths,
		Warnings:    s.warnings,
	}
	out := s.orch.OutputDir
	var writeErrs []error
	if err := writeCSV(filepath.Join(out, "federation.csv"), federationHeader(), rows); err != nil {
		writeErrs = append(writeErrs, err)
	}
	if len(s.records) > 0 {
		providers := providerRows(s.records[0].Candidates, modelChoices(names, s.records, s.agentPaths))
		if err := writeCSV(filepath.Join(out, "providers.csv"), providersHeader(), providers); err != nil {
			writeErrs = append(writeErrs, err)
		}
	}
	if err := testlib.WriteJSONFile(out, "summary.json", summary); err != nil {
		writeErrs = append(writeErrs, err)
	}
	if err := writeSummaryMarkdown(filepath.Join(out, "summary.md"), summary, s.cc, s.records); err != nil {
		writeErrs = append(writeErrs, err)
	}
	log.Printf("=== VERDICT: %s ===", verdict)
	for _, r := range reasons {
		log.Printf("  - %s", r)
	}
	log.Printf("results: %s", out)

	if fatal != nil {
		return errors.Join(append([]error{fatal}, writeErrs...)...)
	}
	if verdict != verdictPass {
		failed := fmt.Errorf("ConsumerChoice validation failed; see %s", filepath.Join(out, "summary.md"))
		return errors.Join(append([]error{failed}, writeErrs...)...)
	}
	return errors.Join(writeErrs...)
}

// verdict is PASS only when every repetition shows the full chain working: the
// model was actually asked, its answer was checked, and the reservation for the
// resulting choice reached Peered and was released cleanly -- and, when
// enabled, every agent-path check passed.
//
// Criterion misses are results about the model, reported but not failing the
// functional validation. So are unusable answers that are the model's own doing
// (a timeout, broken JSON, no or an invented provider), as long as the fallback's
// reservation still reached Peered. A run where the model never once answered
// usably does fail: that points at the setup, not at the model's judgement.
func (s *suiteRun) verdict(m RunMetrics, outcomes []RepetitionOutcome, fatal error) (string, []string) {
	var reasons []string
	if fatal != nil {
		reasons = append(reasons, "run stopped early: "+fatal.Error())
	}
	reasons = append(reasons, agentPathReasons(s.cc.AgentPath.IsEnabled(), len(s.cc.Scenarios), s.agentPaths)...)
	expected := len(s.cc.Scenarios) * s.cc.Repetitions
	if len(outcomes) < expected {
		reasons = append(reasons, fmt.Sprintf("only %d of %d repetitions ran", len(outcomes), expected))
	}
	if m.SingleCandidateShortcuts > 0 {
		reasons = append(reasons, fmt.Sprintf("%d decision(s) had a single candidate, so the model was not exercised",
			m.SingleCandidateShortcuts))
	}
	if rejected := m.AICalls - m.ValidAIIDs - m.ModelAnswerFailures; rejected > 0 {
		reasons = append(reasons, fmt.Sprintf("%d of %d model answers were rejected for a reason outside the model "+
			"(see the failure breakdown)", rejected, m.AICalls))
	}
	if noUsableAnswer(m, s.agentPaths) {
		reasons = append(reasons, "the model never gave a usable answer, in the recorded decisions or on the agent "+
			"path: check the model, the prompt and Ollama")
	}
	if m.PeeredFromValidAI < m.ValidAIIDs {
		reasons = append(reasons, fmt.Sprintf("%d of %d valid AI selections did not reach Peered",
			m.ValidAIIDs-m.PeeredFromValidAI, m.ValidAIIDs))
	}
	for _, o := range outcomes {
		if o.ModelAnswerFailure && o.FallbackUsed && !o.Peered {
			reasons = append(reasons, fmt.Sprintf("%s rep %d: the model's answer was unusable and the fallback's "+
				"reservation did not reach Peered", o.Scenario, o.Repetition))
		}
		if o.ReservationAttempt && !o.ReleasedAndSettled {
			reasons = append(reasons, fmt.Sprintf("%s rep %d: capacity was not returned cleanly",
				o.Scenario, o.Repetition))
		}
	}
	if len(outcomes) == 0 && fatal == nil {
		reasons = append(reasons, "no repetition ran")
	}
	if len(reasons) > 0 {
		return verdictFail, reasons
	}
	return verdictPass, nil
}

// noUsableAnswer reports a run in which the model was asked and never answered
// usably, neither in the recorded decisions nor on the agent path.
func noUsableAnswer(m RunMetrics, agentPaths []*AgentPathResult) bool {
	if m.ValidAIIDs > 0 {
		return false
	}
	asked := m.AICalls
	for _, r := range agentPaths {
		if r == nil || r.Selection == nil {
			continue
		}
		if r.Selection.Source == localapi.SelectionSourceAI {
			return false
		}
		asked++
	}
	return asked > 0
}

// writeConfiguration records everything needed to reproduce the run. It holds
// no kubeconfig path, certificate, key or token.
func (s *suiteRun) writeConfiguration() error {
	cfg := s.orch.Config
	o := s.cc.Ollama
	scenarios := make([]map[string]any, len(s.cc.Scenarios))
	for i, sc := range s.cc.Scenarios {
		scenarios[i] = map[string]any{"name": sc.Name, "userRequest": sc.UserRequest,
			"criterion": sc.Criterion, "referenceWeights": sc.ReferenceWeights}
	}
	providers := make([]map[string]any, len(s.cc.ProviderProfiles))
	for i, p := range s.cc.ProviderProfiles {
		city, _, _, _ := testlib.RegionLocation(cfg.ProviderRegions[i])
		chunks, _ := p.Capacity.Chunks()
		providers[i] = map[string]any{
			"providerId": providerID(i), "region": cfg.ProviderRegions[i], "city": city,
			"carbonIntensity": p.CarbonIntensity, "prices": p.Prices, "capacity": p.Capacity, "chunks": chunks,
		}
	}
	commit, dirty := gitRevision(s.orch.RepoRoot)
	conf := map[string]any{
		"runId":            s.orch.RunID,
		"generatedAt":      time.Now().UTC(),
		"testType":         testType,
		"mode":             s.cc.Mode,
		"gitCommit":        commit,
		"gitDirty":         dirty,
		"goVersion":        runtime.Version(),
		"consumers":        cfg.Consumers,
		"providers":        cfg.Providers,
		"consumerRegion":   cfg.ProviderRegions[0],
		"providerProfiles": providers,
		"scenarios":        scenarios,
		"repetitions":      s.cc.Repetitions,
		"fallback":         s.cc.Fallback,
		"ollama": map[string]any{
			"managed": o.IsManaged(), "image": o.Image, "model": o.Model,
			"baseUrl": redactURL(s.rt.baseURL), "agentBaseUrl": redactURL(s.agentOllamaURL),
			"serverVersion": s.rt.serverVersion(), "timeout": o.Timeout.String(),
			"request": "JSON mode, server-default sampling: exactly as the consumer agent sends it",
			"gpu":     o.GPU, "pullIfMissing": *o.PullIfMissing,
		},
		"agentPath": map[string]any{
			"enabled": s.cc.AgentPath.IsEnabled(), "cpu": s.cc.AgentPath.CPU, "memory": s.cc.AgentPath.Memory,
			"timeout": s.cc.AgentPath.Timeout.String(),
		},
		"timeouts": map[string]string{
			"metadata": s.cc.MetadataTimeout.String(), "policy": s.cc.PolicyTimeout.String(),
			"reservationPoll": cfg.Experiment.ReservationPoll.String(),
			"reservation":     cfg.Experiment.ReservationTimeout.String(),
		},
		"cleanup": !s.keep,
		"clusterAutoscaler": "disabled: recorded decisions are reserved by the harness, " +
			"the agent-path check by the consumer's manual-reservation controller",
		"agentSystemPrompt": "see prompt.txt in each repetition directory",
	}
	return testlib.WriteJSONFile(s.orch.OutputDir, "configuration.json", conf)
}

func gitRevision(repoRoot string) (string, bool) {
	rev, err := exec.Command("git", "-C", repoRoot, "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown", false
	}
	status, _ := exec.Command("git", "-C", repoRoot, "status", "--porcelain", "--untracked-files=no").Output()
	return strings.TrimSpace(string(rev)), len(strings.TrimSpace(string(status))) > 0
}
