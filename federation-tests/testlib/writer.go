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

package testlib

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Experiment modes and phase-duration modes, as written in the YAML config
// (TestParams.Mode and TestParams.Duration).
const (
	ModeObserve = "observe"
	ModeReserve = "reserve"

	DurationIterations = "iterations"
	DurationTime       = "time"
)

// OutcomeSuccess marks an iteration that ended with the consumer holding the
// capacity it asked for.
const OutcomeSuccess = "success"

// Phase labels used throughout the comparative tests.
const (
	PhaseSetup      = "setup"
	PhaseWarmupA    = "warmup-a"
	PhaseA          = "phase-a"
	PhaseTransition = "transition"
	PhaseWarmupB    = "warmup-b"
	PhaseB          = "phase-b"
	PhaseCleanup    = "cleanup"
)

// OutcomeKeepNoAlternative marks an iteration where the consumer stayed on the
// reservation it already held because the Broker exposed nothing growable to
// move to — the federation was fully booked. It is a success (the consumer
// ends the iteration with working capacity, and the real ResourceRequest
// controller likewise never tears down a working peering just because there
// is no headroom elsewhere right now), but it is kept distinct from a plain
// "success" so analysis can separate "kept because it was the best choice"
// from "kept because it was the only choice". Shared by both comparative
// binaries and by their phase summaries, which count it alongside "success".
const OutcomeKeepNoAlternative = "keep-no-alternative"

// SelectionRecord captures one reservation attempt: which provider was picked,
// under which policy, and any measured RTT.
type SelectionRecord struct {
	Timestamp      time.Time `json:"timestamp"`
	ConsumerID     string    `json:"consumerId"`
	Phase          string    `json:"phase"`
	Policy         string    `json:"policy"`
	Iteration      int       `json:"iteration"`
	SelectedID     string    `json:"selectedProviderId"`
	NodeGroupID    string    `json:"nodeGroupId"`
	ReservationID  string    `json:"reservationId"`
	PlacementValue float64   `json:"placementValue,omitempty"`
	HasMetric      bool      `json:"hasMetric,omitempty"`
	RTTMs          float64   `json:"rttMs,omitempty"`
	DurationMs     float64   `json:"durationMs"`
	Outcome        string    `json:"outcome"`
	ErrorMessage   string    `json:"errorMessage,omitempty"`

	// InitialProviderID is the first candidate this consumer computed, before
	// any capacity-race retries; empty when there was no candidate at all
	// (e.g. a "no-winner" outcome). Differs from SelectedID only when
	// RetryCount > 0 — i.e. this consumer lost a race for its first pick.
	InitialProviderID string `json:"initialProviderId,omitempty"`
	// RetryCount is how many times this consumer lost a capacity race
	// (insufficient-capacity 409) before landing on SelectedID or giving up.
	// Zero means no contention.
	RetryCount int `json:"retryCount"`
}

var selectionCSVHeader = []string{
	"timestamp", "consumer_id", "phase", "policy", "iteration",
	"selected_provider_id", "nodegroup_id", "reservation_id",
	"placement_value", "has_metric", "rtt_ms",
	"duration_ms", "outcome", "error_message",
	"initial_provider_id", "retry_count",
}

func selectionCSVRow(r SelectionRecord) []string {
	return []string{
		r.Timestamp.UTC().Format(time.RFC3339Nano),
		r.ConsumerID,
		r.Phase,
		r.Policy,
		strconv.Itoa(r.Iteration),
		r.SelectedID,
		r.NodeGroupID,
		r.ReservationID,
		strconv.FormatFloat(r.PlacementValue, 'f', 4, 64),
		strconv.FormatBool(r.HasMetric),
		strconv.FormatFloat(r.RTTMs, 'f', 3, 64),
		strconv.FormatFloat(r.DurationMs, 'f', 3, 64),
		r.Outcome,
		r.ErrorMessage,
		r.InitialProviderID,
		strconv.Itoa(r.RetryCount),
	}
}

// writeCSVFile writes header plus rows to dir/name. Closing the file is part
// of the result: a failed Close can leave a truncated CSV behind, which the
// analysis scripts would read as a run that simply produced fewer rows.
func writeCSVFile(dir, name string, header []string, rows [][]string) (err error) {
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()

	w := csv.NewWriter(f)
	if err := w.Write(header); err != nil {
		return err
	}
	for _, row := range rows {
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// WriteSelectionCSV writes selection records to a CSV file in the output dir.
func WriteSelectionCSV(dir, name string, records []SelectionRecord) error {
	rows := make([][]string, 0, len(records))
	for _, r := range records {
		rows = append(rows, selectionCSVRow(r))
	}
	return writeCSVFile(dir, name, selectionCSVHeader, rows)
}

// ProbeRecord captures one RTT measurement session.
type ProbeRecord struct {
	Timestamp  time.Time          `json:"timestamp"`
	ConsumerID string             `json:"consumerId"`
	Phase      string             `json:"phase"`
	Policy     string             `json:"policy"`
	Iteration  int                `json:"iteration"`
	Chosen     string             `json:"chosen"`
	RTTs       map[string]float64 `json:"rtts"`
	DurationMs float64            `json:"durationMs"`
}

// WriteProbeCSV writes probe measurement records to a CSV file.
func WriteProbeCSV(dir, name string, records []ProbeRecord) error {
	header := []string{"timestamp", "consumer_id", "phase", "policy", "iteration", "chosen", "duration_ms", "provider_id", "rtt_ms"}

	rows := make([][]string, 0, len(records))
	for _, r := range records {
		for providerID, rtt := range r.RTTs {
			rows = append(rows, []string{
				r.Timestamp.UTC().Format(time.RFC3339Nano),
				r.ConsumerID,
				r.Phase,
				r.Policy,
				strconv.Itoa(r.Iteration),
				r.Chosen,
				strconv.FormatFloat(r.DurationMs, 'f', 3, 64),
				providerID,
				strconv.FormatFloat(rtt, 'f', 3, 64),
			})
		}
	}
	return writeCSVFile(dir, name, header, rows)
}

// ExperimentSummary is the top-level JSON summary of a comparative run.
type ExperimentSummary struct {
	RunID              string    `json:"runId"`
	TestType           string    `json:"testType"`
	StartTime          time.Time `json:"startTime"`
	EndTime            time.Time `json:"endTime"`
	ConsumerID         string    `json:"consumerId"`
	ConsumerCertFP     string    `json:"consumerCertFingerprint,omitempty"`
	BrokerURL          string    `json:"brokerUrl"`
	ConsoleURL         string    `json:"consoleUrl"`
	ProviderCount      int       `json:"providerCount"`
	IterationsPerPhase int       `json:"iterationsPerPhase"`
	// DurationMode is "iterations" or "time" (see TestParams.Duration).
	// TimerConfigured is the formatted Timer duration when DurationMode is
	// "time", empty otherwise.
	DurationMode    string       `json:"durationMode,omitempty"`
	TimerConfigured string       `json:"timerConfigured,omitempty"`
	PhaseAPolicy    string       `json:"phaseAPolicy"`
	PhaseBPolicy    string       `json:"phaseBPolicy"`
	PhaseASummary   PhaseSummary `json:"phaseASummary"`
	PhaseBSummary   PhaseSummary `json:"phaseBSummary"`
}

// PhaseSummary aggregates one measurement phase's results.
type PhaseSummary struct {
	Iterations       int            `json:"iterations"`
	Successes        int            `json:"successes"`
	Failures         int            `json:"failures"`
	SelectionCounts  map[string]int `json:"selectionCounts"`
	MeanRTTMs        float64        `json:"meanRttMs,omitempty"`
	MedianRTTMs      float64        `json:"medianRttMs,omitempty"`
	MeanPlacementVal float64        `json:"meanPlacementValue,omitempty"`
}

// WriteJSONFile writes an arbitrary value as pretty-printed JSON. Closing the
// file is part of the result: a failed Close can leave truncated JSON behind,
// which the analysis scripts cannot parse at all.
func WriteJSONFile(dir, name string, v any) (err error) {
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// WriteSummaryMarkdown renders a human-readable companion to summary.json.
func WriteSummaryMarkdown(dir string, s ExperimentSummary) error {
	// Rendered into a buffer first: writing to memory cannot fail, so the
	// summary reaches the disk whole or not at all, and the one error that
	// matters — the write itself — is the one returned.
	f := &strings.Builder{}

	fmt.Fprintf(f, "# Comparative Test Summary: %s\n\n", s.TestType)
	fmt.Fprintf(f, "- Run ID: `%s`\n", s.RunID)
	fmt.Fprintf(f, "- Start: %s\n", s.StartTime.UTC().Format(time.RFC3339))
	fmt.Fprintf(f, "- End: %s\n", s.EndTime.UTC().Format(time.RFC3339))
	fmt.Fprintf(f, "- Consumer: `%s`\n", s.ConsumerID)
	fmt.Fprintf(f, "- Broker: %s\n", s.BrokerURL)
	fmt.Fprintf(f, "- Providers: %d\n", s.ProviderCount)
	// Under DurationMode "time" a phase runs to the clock and IterationsPerPhase
	// carries the config's iterations value, which that mode ignores entirely --
	// reporting it as if it described the run made the summary say "15" for a
	// phase that actually ran 830 iterations.
	if s.DurationMode == DurationTime {
		fmt.Fprintf(f, "- Phase length: %s (wall clock; configured iterations ignored)\n\n", s.TimerConfigured)
	} else {
		fmt.Fprintf(f, "- Iterations per phase: %d\n\n", s.IterationsPerPhase)
	}

	for _, ps := range []struct {
		label   string
		policy  string
		summary PhaseSummary
	}{
		{"Phase A (baseline)", s.PhaseAPolicy, s.PhaseASummary},
		{"Phase B (policy-aware)", s.PhaseBPolicy, s.PhaseBSummary},
	} {
		fmt.Fprintf(f, "## %s — %s\n\n", ps.label, ps.policy)
		fmt.Fprintf(f, "- Iterations: %d (success: %d, fail: %d)\n",
			ps.summary.Iterations, ps.summary.Successes, ps.summary.Failures)
		fmt.Fprintf(f, "- Selection distribution:\n")
		for id, count := range ps.summary.SelectionCounts {
			fmt.Fprintf(f, "  - `%s`: %d (%.1f%%)\n", id, count, 100*float64(count)/float64(max(ps.summary.Iterations, 1)))
		}
		if ps.summary.MeanRTTMs > 0 {
			fmt.Fprintf(f, "- Mean RTT: %.2f ms\n", ps.summary.MeanRTTMs)
			fmt.Fprintf(f, "- Median RTT: %.2f ms\n", ps.summary.MedianRTTMs)
		}
		if ps.summary.MeanPlacementVal > 0 {
			fmt.Fprintf(f, "- Mean placement value: %.4f\n", ps.summary.MeanPlacementVal)
		}
		fmt.Fprintf(f, "\n")
	}
	return os.WriteFile(filepath.Join(dir, "summary.md"), []byte(f.String()), 0o644)
}

// ReservationRecord captures one reservation lifecycle event in reserve mode.
type ReservationRecord struct {
	Timestamp         time.Time `json:"timestamp"`
	ConsumerID        string    `json:"consumerId"`
	Phase             string    `json:"phase"`
	Policy            string    `json:"policy"`
	Iteration         int       `json:"iteration"`
	ReservationID     string    `json:"reservationId"`
	ProviderClusterID string    `json:"providerClusterId"`
	NodeGroupID       string    `json:"nodeGroupId"`
	Action            string    `json:"action"`
	PrevProviderID    string    `json:"prevProviderId,omitempty"`
	PeerMs            float64   `json:"peerMs"`
	ReleaseMs         float64   `json:"releaseMs,omitempty"`
	TotalMs           float64   `json:"totalMs"`
	FinalPhase        string    `json:"finalPhase"`
	PlacementMetric   float64   `json:"placementMetric,omitempty"`
	// CarbonIntensity/HasCarbon duplicate the provider's carbon reading
	// already visible per-provider in nodegroups.csv, so comparative-eco
	// analysis doesn't need a join on provider_id to know what a reservation
	// cost. Always zero/false for comparative-latency (no carbon metric).
	CarbonIntensity float64 `json:"carbonIntensity,omitempty"`
	HasCarbon       bool    `json:"hasCarbon"`
	RTTMs           float64 `json:"rttMs,omitempty"`
	Outcome         string  `json:"outcome"`
	ErrorMessage    string  `json:"errorMessage,omitempty"`

	// InitialProviderID is the first candidate this consumer computed for
	// THIS decision, before any capacity-race retries; differs from
	// ProviderClusterID only when RetryCount > 0. Not to be confused with
	// PrevProviderID, which is the provider held before this decision
	// (switch source), not before this decision's own retries.
	InitialProviderID string `json:"initialProviderId,omitempty"`
	// RetryCount is how many times this consumer lost a capacity race
	// (insufficient-capacity 409) before landing on ProviderClusterID or
	// giving up. Zero means no contention.
	RetryCount int `json:"retryCount"`
}

var reservationCSVHeader = []string{
	"timestamp", "consumer_id", "phase", "policy", "iteration",
	"reservation_id", "provider_id", "nodegroup_id",
	"action", "prev_provider_id",
	"peer_duration_ms", "release_duration_ms", "total_duration_ms",
	"final_phase", "placement_metric",
	"carbon_intensity", "has_carbon", "rtt_ms",
	"outcome", "error_message",
	"initial_provider_id", "retry_count",
}

func reservationCSVRow(r ReservationRecord) []string {
	carbonStr := ""
	if r.HasCarbon {
		carbonStr = strconv.FormatFloat(r.CarbonIntensity, 'f', 2, 64)
	}
	return []string{
		r.Timestamp.UTC().Format(time.RFC3339Nano),
		r.ConsumerID,
		r.Phase,
		r.Policy,
		strconv.Itoa(r.Iteration),
		r.ReservationID,
		r.ProviderClusterID,
		r.NodeGroupID,
		r.Action,
		r.PrevProviderID,
		strconv.FormatFloat(r.PeerMs, 'f', 3, 64),
		strconv.FormatFloat(r.ReleaseMs, 'f', 3, 64),
		strconv.FormatFloat(r.TotalMs, 'f', 3, 64),
		r.FinalPhase,
		strconv.FormatFloat(r.PlacementMetric, 'f', 4, 64),
		carbonStr,
		strconv.FormatBool(r.HasCarbon),
		strconv.FormatFloat(r.RTTMs, 'f', 3, 64),
		r.Outcome,
		r.ErrorMessage,
		r.InitialProviderID,
		strconv.Itoa(r.RetryCount),
	}
}

// WriteReservationCSV writes reservation lifecycle records to a CSV file.
func WriteReservationCSV(dir, name string, records []ReservationRecord) error {
	rows := make([][]string, 0, len(records))
	for _, r := range records {
		rows = append(rows, reservationCSVRow(r))
	}
	return writeCSVFile(dir, name, reservationCSVHeader, rows)
}

// NodeGroupSnapshotRecord captures one provider's nodegroup state at a given
// iteration — used to log placement metrics and carbon intensity for all
// providers, not just the selected winner.
type NodeGroupSnapshotRecord struct {
	Timestamp         time.Time `json:"timestamp"`
	ConsumerID        string    `json:"consumerId"`
	Phase             string    `json:"phase"`
	Policy            string    `json:"policy"`
	Iteration         int       `json:"iteration"`
	ProviderClusterID string    `json:"providerClusterId"`
	NodeGroupID       string    `json:"nodeGroupId"`
	PlacementMetric   float64   `json:"placementMetric,omitempty"`
	HasMetric         bool      `json:"hasMetric"`
	CarbonIntensity   float64   `json:"carbonIntensity,omitempty"`
	HasCarbon         bool      `json:"hasCarbon"`
	CurrentReserved   int32     `json:"currentReserved"`
	MaxSize           int32     `json:"maxSize"`
	AppliedPlacement  string    `json:"appliedPlacement"`
	IsSelected        bool      `json:"isSelected"`
}

var nodegroupCSVHeader = []string{
	"timestamp", "consumer_id", "phase", "policy", "iteration",
	"provider_id", "nodegroup_id",
	"placement_metric", "has_metric",
	"carbon_intensity", "has_carbon",
	"current_reserved", "max_size",
	"applied_placement", "is_selected",
}

func nodegroupCSVRow(r NodeGroupSnapshotRecord) []string {
	carbonStr := ""
	if r.HasCarbon {
		carbonStr = strconv.FormatFloat(r.CarbonIntensity, 'f', 2, 64)
	}
	return []string{
		r.Timestamp.UTC().Format(time.RFC3339Nano),
		r.ConsumerID,
		r.Phase,
		r.Policy,
		strconv.Itoa(r.Iteration),
		r.ProviderClusterID,
		r.NodeGroupID,
		strconv.FormatFloat(r.PlacementMetric, 'f', 4, 64),
		strconv.FormatBool(r.HasMetric),
		carbonStr,
		strconv.FormatBool(r.HasCarbon),
		strconv.FormatInt(int64(r.CurrentReserved), 10),
		strconv.FormatInt(int64(r.MaxSize), 10),
		r.AppliedPlacement,
		strconv.FormatBool(r.IsSelected),
	}
}

// WriteNodeGroupCSV writes nodegroup snapshot records to a CSV file.
func WriteNodeGroupCSV(dir, name string, records []NodeGroupSnapshotRecord) error {
	rows := make([][]string, 0, len(records))
	for _, r := range records {
		rows = append(rows, nodegroupCSVRow(r))
	}
	return writeCSVFile(dir, name, nodegroupCSVHeader, rows)
}

// FederationSampleRecord is one periodic snapshot of a single consumer's
// standing state, independent of the iteration/keep/switch event stream:
// which provider it is connected to right now (empty if none) and what that
// connection currently costs. MetricType names what MetricValue means so one
// schema serves both comparative binaries — "carbon_intensity" for
// comparative-eco, "rtt_ms" for comparative-latency — instead of a column
// per test type that is always empty for the other.
type FederationSampleRecord struct {
	Timestamp         time.Time `json:"timestamp"`
	Phase             string    `json:"phase"`
	Policy            string    `json:"policy"`
	ConsumerID        string    `json:"consumerId"`
	ProviderClusterID string    `json:"providerClusterId,omitempty"`
	ReservationID     string    `json:"reservationId,omitempty"`
	MetricType        string    `json:"metricType"`
	MetricValue       float64   `json:"metricValue,omitempty"`
	HasMetric         bool      `json:"hasMetric"`
}

var federationCSVHeader = []string{
	"timestamp", "phase", "policy", "consumer_id",
	"provider_id", "reservation_id",
	"metric_type", "metric_value", "has_metric",
}

func federationCSVRow(r FederationSampleRecord) []string {
	metricStr := ""
	if r.HasMetric {
		metricStr = strconv.FormatFloat(r.MetricValue, 'f', 3, 64)
	}
	return []string{
		r.Timestamp.UTC().Format(time.RFC3339Nano),
		r.Phase,
		r.Policy,
		r.ConsumerID,
		r.ProviderClusterID,
		r.ReservationID,
		r.MetricType,
		metricStr,
		strconv.FormatBool(r.HasMetric),
	}
}

// WriteFederationCSV writes federation-wide periodic sample records to a CSV file.
func WriteFederationCSV(dir, name string, records []FederationSampleRecord) error {
	rows := make([][]string, 0, len(records))
	for _, r := range records {
		rows = append(rows, federationCSVRow(r))
	}
	return writeCSVFile(dir, name, federationCSVHeader, rows)
}

// EnsureOutputDir creates the output directory for a test run.
func EnsureOutputDir(base, testType string) (string, error) {
	dir := filepath.Join(base, testType, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create output dir %q: %w", dir, err)
	}
	return dir, nil
}
