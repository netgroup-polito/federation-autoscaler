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
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/netgroup-polito/federation-autoscaler/federation-tests/testlib"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// RepetitionRecord is everything one repetition produced, kept in memory so
// both the per-repetition files and the run-level federation.csv are written
// from the same data.
type RepetitionRecord struct {
	Scenario       Scenario
	Repetition     int
	Dir            string
	Consumer       *testlib.ConsumerLocation
	BrokerSnapshot *brokerapi.NodeGroupListResponse
	Decision       *Decision
	Candidates     []CandidateMetrics
	Criterion      CriterionResult
	Reservation    *ReservationResult
	Outcome        RepetitionOutcome
}

// writeRepetition writes the per-repetition audit files: what the model
// answered and what happened to the reservation. Everything else about a
// repetition is in summary.json and federation.csv. What the model was asked
// (prompt.txt) and the Broker's raw list are written once per scenario, from
// its first repetition: the federation is held fixed, so the later ones would
// repeat them. promptDrift checks that they did.
func writeRepetition(rec *RepetitionRecord, rt *ollamaRuntime) error {
	if err := os.MkdirAll(rec.Dir, 0o755); err != nil {
		return err
	}
	files := map[string]any{"model_response.json": modelResponse(rec.Decision.Trace, rt, rec.Decision)}
	if rec.Reservation != nil {
		files["reservation_result.json"] = rec.Reservation
	} else {
		files["reservation_result.json"] = map[string]any{
			"reservationCreated": false,
			"reason":             "no valid selection to reserve (see failureCategories in summary.json)",
		}
	}
	for name, v := range files {
		if err := testlib.WriteJSONFile(rec.Dir, name, v); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	if rec.Repetition == 1 {
		scenarioDir := filepath.Dir(rec.Dir)
		if err := os.WriteFile(filepath.Join(scenarioDir, "prompt.txt"), []byte(renderPrompt(rec)), 0o644); err != nil {
			return err
		}
		if err := testlib.WriteJSONFile(scenarioDir, "broker_nodegroups.json", rec.BrokerSnapshot); err != nil {
			return fmt.Errorf("write broker_nodegroups.json: %w", err)
		}
	}
	return nil
}

func renderPrompt(rec *RepetitionRecord) string {
	trace := rec.Decision.Trace
	var b strings.Builder
	fmt.Fprintf(&b, "Scenario: %s (the same prompt for every repetition)\nModel: %s\n", rec.Scenario.Name, trace.Model)
	b.WriteString("Request: Ollama JSON mode, the server's default sampling -- as the consumer agent sends it.\n\n")
	b.WriteString("=== SYSTEM PROMPT ===\n")
	b.WriteString(trace.SystemPrompt)
	b.WriteString("\n\n=== USER PROMPT (user request + eligible providers) ===\n")
	if trace.SingleCandidate {
		b.WriteString("(not sent: only one provider had capacity, so the model was not called)\n")
	} else {
		b.WriteString(trace.UserPrompt)
	}
	fmt.Fprintf(&b, "\n\n=== OUTPUT FORMAT (sent as Ollama `format`) ===\n%q\n", trace.Format)
	if rec.Consumer.HasCoordinates() {
		fmt.Fprintf(&b, "\nConsumer location sent to the model (CONSUMER LOCATION above): %s (%s) lat %.4f lon %.4f\n"+
			"Distances in providers.csv and federation.csv are computed by the harness to judge the choice; "+
			"the model received none.\n",
			rec.Consumer.City, rec.Consumer.Region, rec.Consumer.Lat, rec.Consumer.Lon)
	}
	return b.String()
}

func modelResponse(trace *ollama.Trace, rt *ollamaRuntime, d *Decision) map[string]any {
	out := map[string]any{
		"model":             trace.Model,
		"ollamaBaseUrl":     redactURL(rt.baseURL),
		"ollamaVersion":     rt.serverVersion(),
		"requestStartedAt":  trace.StartedAt,
		"responseAt":        trace.FinishedAt,
		"selectorLatencyMs": ms(trace.Duration()),
		"decisionLatencyMs": ms(d.DecisionLatency),
		"singleCandidate":   trace.SingleCandidate,
		"httpStatus":        trace.HTTPStatus,
		"rawResponse":       trace.RawResponse,
		"parsedResponse":    trace.Parsed,
		"rankedValidIds":    trace.Ranked,
		"unknownIds":        trace.UnknownIDs,
		"envelope":          trace.Envelope,
		"errorKind":         trace.ErrorKind,
		"error":             trace.Error,
		"note":              "reason and confidence are the model's own account and were not used to judge the choice",
	}
	if trace.RawErrorBody != "" {
		out["rawErrorBody"] = trace.RawErrorBody
	}
	return out
}

func comparisonHeader() []string {
	return []string{
		"provider_id", "node_group_id", "chunk_type", "region", "latitude", "longitude",
		"available_chunks", "available_cpu_millicores", "available_memory_mib",
		"cost_per_chunk", "carbon_intensity_gco2eq_kwh", "distance_km",
		"carbon_rank", "distance_rank", "cost_rank", "reference_score", "reference_rank",
		"is_ai_selected", "is_final_selected",
	}
}

// comparisonRows are federation.csv's rows for one repetition: one per
// candidate, with the scenario, repetition and outcome around it.
func comparisonRows(rec *RepetitionRecord) [][]string {
	d := rec.Decision
	consumerRegion := ""
	if rec.Consumer != nil {
		consumerRegion = rec.Consumer.Region
	}
	rows := make([][]string, 0, len(rec.Candidates))
	for _, c := range rec.Candidates {
		peered, finalPhase, resID := "false", "", ""
		if r := rec.Reservation; r != nil && c.ProviderID == d.FinalProviderID {
			peered, finalPhase, resID = strconv.FormatBool(r.Peered), r.FinalPhase, r.ReservationID
		}
		rows = append(rows, []string{
			rec.Scenario.Name, strconv.Itoa(rec.Repetition), consumerRegion, d.Source,
			c.ProviderID, c.NodeGroupID, c.ChunkType, c.Region, fmtF(c.Latitude), fmtF(c.Longitude),
			strconv.Itoa(int(c.AvailableChunks)), strconv.FormatInt(c.AvailableCPUMillicores, 10),
			strconv.FormatInt(c.AvailableMemoryMiB, 10),
			fmtP(c.CostPerChunk), fmtP(c.CarbonIntensity), fmtP(c.DistanceKm),
			fmtRank(c.CarbonRank), fmtRank(c.DistanceRank), fmtRank(c.CostRank), fmtP(c.Reference), fmtRank(c.ReferenceRank),
			strconv.FormatBool(c.ProviderID == d.Validation.SelectedProviderID),
			strconv.FormatBool(c.ProviderID == d.FinalProviderID),
			resID, finalPhase, peered, criterionString(rec.Criterion.Passed),
		})
	}
	return rows
}

// providersHeader and providerRows make providers.csv: the federation once, one
// row per provider, with its ranks among the nine and where the model chose it.
// Only data that is the same in every scenario goes in; the scenario-weighted
// reference score does not.
func providersHeader() []string {
	return []string{
		"provider_id", "region", "city", "latitude", "longitude", "distance_km", "distance_rank",
		"carbon_intensity_gco2eq_kwh", "carbon_rank", "cost_per_chunk", "cost_rank",
		"available_chunks", "available_cpu_millicores", "available_memory_mib",
		"model_chose_in",
	}
}

func providerRows(candidates []CandidateMetrics, choices map[string]string) [][]string {
	rows := make([][]string, 0, len(candidates))
	for _, c := range candidates {
		city, _, _, _ := testlib.RegionLocation(c.Region)
		rows = append(rows, []string{
			c.ProviderID, c.Region, city, fmtF(c.Latitude), fmtF(c.Longitude), fmtP(c.DistanceKm), fmtRank(c.DistanceRank),
			fmtP(c.CarbonIntensity), fmtRank(c.CarbonRank), fmtP(c.CostPerChunk), fmtRank(c.CostRank),
			strconv.Itoa(int(c.AvailableChunks)), strconv.FormatInt(c.AvailableCPUMillicores, 10),
			strconv.FormatInt(c.AvailableMemoryMiB, 10),
			choices[c.ProviderID],
		})
	}
	return rows
}

// modelChoices says, per provider, in which scenarios the model chose it, as
// "eco-oriented (3/6) - ambiguous (4/6)": times chosen out of the decisions the
// scenario ran, its recorded repetitions and its agent path together. Both are
// the same model answering the same request on the same data; the agent path
// is one more of them, asked by the agent itself. A fallback pick is not the
// model's choice and does not count as chosen, though its decision counts in
// the total; neither does a single candidate, where the model was not asked.
func modelChoices(scenarios []string, records []*RepetitionRecord, agentPaths []*AgentPathResult) map[string]string {
	decisions := map[string]int{}         // scenario -> decisions run
	chosen := map[string]map[string]int{} // provider -> scenario -> times chosen
	choose := func(provider, scenario string) {
		if chosen[provider] == nil {
			chosen[provider] = map[string]int{}
		}
		chosen[provider][scenario]++
	}
	for _, rec := range records {
		decisions[rec.Scenario.Name]++
		if o := rec.Outcome; o.Source == sourceAI && o.FinalProviderID != "" {
			choose(o.FinalProviderID, rec.Scenario.Name)
		}
	}
	for _, r := range agentPaths {
		if r == nil {
			continue
		}
		decisions[r.Scenario]++
		if r.Selection != nil && r.Selection.Source == localapi.SelectionSourceAI && r.ExpectedProvider != "" {
			choose(r.ExpectedProvider, r.Scenario)
		}
	}

	out := make(map[string]string, len(chosen))
	for provider, times := range chosen {
		var parts []string
		for _, sc := range scenarios {
			if n := times[sc]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s (%d/%d)", sc, n, decisions[sc]))
			}
		}
		out[provider] = strings.Join(parts, " - ")
	}
	return out
}

// federationDrift lists the decisions that saw provider data different from
// the first decision's. The run holds every profile fixed, so it should be
// empty: providers.csv shows the federation once on the strength of that, and
// a non-empty list says where it does not hold.
func federationDrift(records []*RepetitionRecord) []string {
	if len(records) == 0 {
		return nil
	}
	first := make(map[string]CandidateMetrics, len(records[0].Candidates))
	for _, c := range records[0].Candidates {
		first[c.ProviderID] = c
	}
	var drift []string
	for _, rec := range records[1:] {
		var changed []string
		if len(rec.Candidates) != len(first) {
			changed = append(changed, fmt.Sprintf("%d providers instead of %d", len(rec.Candidates), len(first)))
		}
		for _, c := range rec.Candidates {
			if f, ok := first[c.ProviderID]; !ok || !sameProviderData(f, c) {
				changed = append(changed, c.ProviderID)
			}
		}
		if len(changed) > 0 {
			drift = append(drift, fmt.Sprintf("%s rep %d (%s)", rec.Scenario.Name, rec.Repetition, strings.Join(changed, ", ")))
		}
	}
	return drift
}

// promptDrift lists the repetitions whose prompt -- system and user, as sent to
// the model -- differs from the first repetition of the same scenario, the one
// prompt.txt shows. The request and the federation are fixed per scenario, so
// it should be empty.
func promptDrift(records []*RepetitionRecord) []string {
	first := map[string]*ollama.Trace{}
	var drift []string
	for _, rec := range records {
		if rec.Decision == nil || rec.Decision.Trace == nil {
			continue
		}
		trace := rec.Decision.Trace
		f, ok := first[rec.Scenario.Name]
		if !ok {
			first[rec.Scenario.Name] = trace
			continue
		}
		if trace.SystemPrompt != f.SystemPrompt || trace.UserPrompt != f.UserPrompt {
			drift = append(drift, fmt.Sprintf("%s rep %d", rec.Scenario.Name, rec.Repetition))
		}
	}
	return drift
}

// sameProviderData compares what a provider advertises, leaving out what is
// derived from it (distance and ranks).
func sameProviderData(a, b CandidateMetrics) bool {
	samePtr := func(x, y *float64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.NodeGroupID == b.NodeGroupID && a.Region == b.Region && a.Latitude == b.Latitude &&
		a.Longitude == b.Longitude && a.AvailableChunks == b.AvailableChunks &&
		a.AvailableCPUMillicores == b.AvailableCPUMillicores && a.AvailableMemoryMiB == b.AvailableMemoryMiB &&
		samePtr(a.CostPerChunk, b.CostPerChunk) && samePtr(a.CarbonIntensity, b.CarbonIntensity)
}

func federationHeader() []string {
	h := append([]string{"scenario", "repetition", "consumer_region", "selection_source"}, comparisonHeader()...)
	return append(h, "reservation_id", "reservation_final_phase", "reservation_peered", "criterion_passed")
}

func writeCSV(path string, header []string, rows [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := csv.NewWriter(f)
	if err := w.Write(header); err != nil {
		return err
	}
	if err := w.WriteAll(rows); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}

// RunSummary is summary.json.
type RunSummary struct {
	RunID       string              `json:"runId"`
	StartedAt   time.Time           `json:"startedAt"`
	FinishedAt  time.Time           `json:"finishedAt"`
	Model       string              `json:"model"`
	Verdict     string              `json:"verdict"`
	Reasons     []string            `json:"verdictReasons,omitempty"`
	Metrics     RunMetrics          `json:"metrics"`
	Repetitions []RepetitionOutcome `json:"repetitions"`
	// AgentPath holds the agent-path checks: the consumer agent's own
	// ConsumerChoice decision, one per scenario.
	AgentPath []*AgentPathResult `json:"agentPath,omitempty"`
	Warnings  []string           `json:"warnings,omitempty"`
}

func writeSummaryMarkdown(path string, s RunSummary, cc *ChoiceConfig, records []*RepetitionRecord) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# ConsumerChoice validation — %s\n\n", s.RunID)
	fmt.Fprintf(&b, "**Verdict: %s**\n\n", s.Verdict)
	for _, r := range s.Reasons {
		fmt.Fprintf(&b, "- %s\n", r)
	}
	if len(s.Reasons) > 0 {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "- Run: %s → %s (%s)\n",
		s.StartedAt.UTC().Format(time.RFC3339), s.FinishedAt.UTC().Format(time.RFC3339),
		s.FinishedAt.Sub(s.StartedAt).Round(time.Second))
	fmt.Fprintf(&b, "- Model: `%s`, queried as the consumer agent queries it (JSON mode, default sampling, "+
		"timeout %s), fallback `%s`\n", s.Model, cc.Ollama.Timeout, cc.Fallback.Mode)
	fmt.Fprintf(&b, "- Scenarios: %d, repetitions each: %d\n", len(cc.Scenarios), cc.Repetitions)
	if len(records) > 0 && records[0].Consumer.HasCoordinates() {
		fmt.Fprintf(&b, "- Providers: `providers.csv`, the federation every decision was made on; distances "+
			"from the consumer in %s (%s)\n", records[0].Consumer.City, records[0].Consumer.Region)
	}
	b.WriteString("\n")

	m := s.Metrics
	b.WriteString("## Metrics\n\n| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Valid-ID rate | %s (%d of %d AI calls) |\n", fmtRate(m.ValidIDRate), m.ValidAIIDs, m.AICalls)
	fmt.Fprintf(&b, "| Unusable answers, the model's own (timeout, broken JSON, no or invented ID) "+
		"| %d of %d AI calls |\n", m.ModelAnswerFailures, m.AICalls)
	fmt.Fprintf(&b, "| Reservation success rate | %s (%d Peered of %d valid AI selections) |\n",
		fmtRate(m.ReservationSuccessRate), m.PeeredFromValidAI, m.ValidAIIDs)
	fmt.Fprintf(&b, "| Prompt-alignment rate | %s (%d of %d AI selections) |\n",
		fmtRate(m.PromptAlignmentRate), m.AlignmentPassed, m.AlignmentEvaluated)
	fmt.Fprintf(&b, "| Fallback rate | %s (%d of %d AI calls) |\n",
		fmtRate(m.FallbackRate), m.FallbackActivations, m.AICalls)
	fmt.Fprintf(&b, "| Decision latency | %s |\n", fmtLatency(m.DecisionLatency))
	fmt.Fprintf(&b, "| Reservation → Peered | %s |\n", fmtLatency(m.PeeringLatency))
	if m.SingleCandidateShortcuts > 0 {
		fmt.Fprintf(&b, "| Single-candidate shortcuts (model not called) | %d |\n", m.SingleCandidateShortcuts)
	}

	b.WriteString("\n## Scenarios\n")
	for _, sc := range cc.Scenarios {
		fmt.Fprintf(&b, "\n### %s\n\n> %s\n\n", sc.Name, sc.UserRequest)
		for _, sm := range m.Scenarios {
			if sm.Scenario != sc.Name {
				continue
			}
			fmt.Fprintf(&b, "- Criterion `%s`: %d of %d AI selections passed\n",
				sc.Criterion.Type, sm.CriterionPassed, sm.CriterionTotal)
			if sm.AICalls > 1 {
				fmt.Fprintf(&b, "- Repeatability: %s of %d AI calls (most frequent `%s`), distribution %v\n",
					fmtRate(sm.Repeatability), sm.AICalls, sm.MostFrequent, sm.Distribution)
			}
		}
		b.WriteString("\n| Rep | Source | Model choice | Final | Carbon | Distance km | Cost/chunk " +
			"| Ranks C/D/$ | Criterion | Peered | Decision |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, rec := range records {
			if rec.Scenario.Name != sc.Name {
				continue
			}
			var fin CandidateMetrics
			for _, c := range rec.Candidates {
				if c.ProviderID == rec.Decision.FinalProviderID {
					fin = c
				}
			}
			peered := "no reservation"
			if rec.Reservation != nil {
				peered = fmt.Sprintf("%v (%s)", rec.Reservation.Peered, rec.Reservation.FinalPhase)
			}
			decision := time.Duration(rec.Outcome.DecisionLatencyMs * float64(time.Millisecond))
			fmt.Fprintf(&b, "| %d | %s | `%s` | `%s` | %s | %s | %s | %s/%s/%s | %s | %s | %s |\n",
				rec.Repetition, rec.Decision.Source,
				rec.Decision.Validation.SelectedProviderID, rec.Decision.FinalProviderID,
				fmtP(fin.CarbonIntensity), fmtP(fin.DistanceKm), fmtP(fin.CostPerChunk),
				fmtRank(fin.CarbonRank), fmtRank(fin.DistanceRank), fmtRank(fin.CostRank),
				criterionString(rec.Criterion.Passed), peered, decision.Round(time.Millisecond))
			if p := rec.Decision.Trace.Parsed; p != nil && p.Reason != "" {
				fmt.Fprintf(&b, "|  |  | reason (not used to judge): _%s_ |  |  |  |  |  |  |  |  |\n",
					tableCell(p.Reason))
			}
		}
	}

	if len(s.AgentPath) > 0 {
		b.WriteString("\n## Agent path\n\n" +
			"A manual reservation per scenario, placed by the consumer agent itself: its local API asked the " +
			"model and masked the list; the harness only asked for capacity.\n\n" +
			"| Scenario | Source | Agent's choice | Reserved | Phase | Active after | Criterion | Passed |\n" +
			"|---|---|---|---|---|---|---|---|\n")
		for _, r := range s.AgentPath {
			if r == nil {
				continue
			}
			active := ""
			if r.ActiveAfterMs > 0 {
				active = time.Duration(r.ActiveAfterMs * float64(time.Millisecond)).Round(time.Second).String()
			}
			passed := "yes"
			if !r.Passed {
				passed = "no: " + strings.Join(r.Failures, ", ")
			}
			fmt.Fprintf(&b, "| %s | %s | `%s` | `%s` | %s | %s | %s | %s |\n", r.Scenario, selectionSource(r.Selection),
				r.ExpectedProvider, r.ReservedProvider, r.FinalPhase, active, criterionString(r.Criterion.Passed), passed)
		}
	}

	if len(m.FailureBreakdown) > 0 {
		b.WriteString("\n## Failures\n\n| Category | Count |\n|---|---|\n")
		categories := make([]string, 0, len(m.FailureBreakdown))
		for cat := range m.FailureBreakdown {
			categories = append(categories, cat)
		}
		sort.Strings(categories) // stable report across runs
		for _, cat := range categories {
			fmt.Fprintf(&b, "| %s | %d |\n", cat, m.FailureBreakdown[cat])
		}
	}
	if len(s.Warnings) > 0 {
		b.WriteString("\n## Warnings\n\n")
		for _, w := range s.Warnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	b.WriteString("\n## Reading these results\n\n" +
		"This is a functional validation of AI-assisted selection, not a benchmark of the model. " +
		"Ranks are computed among the eligible providers the model was shown (1 = lowest value). " +
		"A criterion pass is evidence that the choice is consistent with the request under a transparent rule; " +
		"it is not a claim that the choice was optimal.\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// tableCell makes model-written text safe inside one Markdown table cell: a pipe
// would end the cell and a line break would end the row.
func tableCell(s string) string {
	s = strings.ReplaceAll(s, "|", "/")
	return strings.Join(strings.Fields(s), " ")
}

func fmtF(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func fmtP(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

func fmtRank(r int) string {
	if r == 0 {
		return ""
	}
	return strconv.Itoa(r)
}

func fmtRate(r *float64) string {
	if r == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", *r*100)
}

func fmtLatency(s LatencyStats) string {
	if s.N == 0 {
		return "n/a"
	}
	return fmt.Sprintf("mean %.0f ms, p95 %.0f ms (n=%d)", *s.MeanMs, *s.P95Ms, s.N)
}

func criterionString(p *bool) string {
	if p == nil {
		return "not evaluable"
	}
	if *p {
		return "pass"
	}
	return "fail"
}
