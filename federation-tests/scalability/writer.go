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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// writeJSONFile writes v as pretty-printed JSON. Closing the file is part of
// the result: a failed Close can leave truncated JSON behind, which nothing
// downstream can parse.
func writeJSONFile(dir, name string, v any) (err error) {
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

// writeCSVFile writes header plus rows to dir/name, reporting a failed close
// as an error: a truncated CSV would otherwise read as a run with fewer
// requests in it.
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

var recordCSVHeader = []string{
	"timestamp", "agent_role", "agent_id", "operation", "phase",
	"latency_ms", "status_code", "outcome", "error_category", "error_message",
}

func recordCSVRow(r Record) []string {
	return []string{
		r.Timestamp.UTC().Format(time.RFC3339Nano),
		r.AgentRole,
		r.AgentID,
		string(r.Operation),
		string(r.Phase),
		strconv.FormatFloat(r.LatencyMS, 'f', 3, 64),
		strconv.Itoa(r.StatusCode),
		string(r.Outcome),
		r.ErrorCategory,
		r.ErrorMessage,
	}
}

// writeRecordCSV writes every record matching filter (nil = all) to name.
func writeRecordCSV(dir, name string, records []Record, filter func(Record) bool) error {
	rows := make([][]string, 0, len(records))
	for _, r := range records {
		if filter != nil && !filter(r) {
			continue
		}
		rows = append(rows, recordCSVRow(r))
	}
	return writeCSVFile(dir, name, recordCSVHeader, rows)
}

func writeResourceUsageCSV(dir string, samples []ResourceSample) error {
	rows := make([][]string, 0, len(samples))
	for _, s := range samples {
		rows = append(rows, []string{
			s.Timestamp.UTC().Format(time.RFC3339Nano),
			strconv.FormatFloat(s.CPUValue, 'f', 4, 64),
			s.CPUUnit,
			strconv.FormatFloat(s.MemMiB, 'f', 3, 64),
			s.Source,
		})
	}
	header := []string{"timestamp", "cpu_value", "cpu_unit", "mem_mib", "source"}
	return writeCSVFile(dir, "broker_resource_usage.csv", header, rows)
}

// writeSummaryMarkdown renders a human-readable companion to summary.json.
func writeSummaryMarkdown(dir string, s Summary, cfg *Config) error {
	// Rendered into a buffer first: writing to memory cannot fail, so the
	// summary reaches the disk whole or not at all, and the one error that
	// matters — the write itself — is the one returned.
	f := &strings.Builder{}

	opRow := func(label string, st OperationStats) string {
		return fmt.Sprintf("| %s | %d | %d | %d | %d | %d | %.1f | %.1f | %.1f | %.1f | %.1f | %.2f%% |\n",
			label, st.Attempts, st.Successes, st.Failures, st.Timeouts, st.Cancelled,
			st.MeanMS, st.P50MS, st.P95MS, st.P99MS, st.SuccessPerMinute, st.ErrorRate*100)
	}

	fmt.Fprintf(f, "# Broker Scalability Test Summary\n\n")
	fmt.Fprintf(f, "- Run ID: `%s`\n", s.RunID)
	fmt.Fprintf(f, "- Start: %s\n", s.StartTime.UTC().Format(time.RFC3339))
	fmt.Fprintf(f, "- End: %s\n", s.EndTime.UTC().Format(time.RFC3339))
	fmt.Fprintf(f, "- Measurement-phase duration: %.1fs (configured: %s)\n", s.ActualMeasurementDurationSeconds, cfg.Duration)
	fmt.Fprintf(f, "- Logical consumers: %d, logical providers: %d\n", s.Consumers, s.Providers)
	fmt.Fprintf(f, "- Broker URL: %s\n\n", cfg.BrokerURL)

	fmt.Fprintf(f, "## Warm-up\n\n")
	fmt.Fprintf(f, "- Providers ready: %d/%d (%.1fs)", s.ProviderWarmup.Ready, s.ProviderWarmup.Total, s.ProviderWarmup.ElapsedMS/1000)
	if len(s.ProviderWarmup.TimedOut) > 0 {
		fmt.Fprintf(f, " — timed out: %v", s.ProviderWarmup.TimedOut)
	}
	fmt.Fprintf(f, "\n- Consumers ready: %d/%d (%.1fs)", s.ConsumerWarmup.Ready, s.ConsumerWarmup.Total, s.ConsumerWarmup.ElapsedMS/1000)
	if len(s.ConsumerWarmup.TimedOut) > 0 {
		fmt.Fprintf(f, " — timed out: %v", s.ConsumerWarmup.TimedOut)
	}
	fmt.Fprintf(f, "\n\n")

	fmt.Fprintf(f, "## Measurement-phase traffic\n\n")
	fmt.Fprintf(f, "| Operation | Attempts | Success | Fail | Timeout | Cancelled | Mean ms | p50 ms | p95 ms | p99 ms "+
		"| Success/min | Error rate |\n")
	fmt.Fprintf(f, "|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	fmt.Fprint(f, opRow("Evaluation (GET /api/v1/nodegroups)", s.Evaluation))
	fmt.Fprint(f, opRow("Advertisement (POST /api/v1/advertisements)", s.Advertisement))
	fmt.Fprint(f, opRow("Heartbeat (POST /api/v1/heartbeat)", s.Heartbeat))
	if s.InstructionPoll != nil {
		fmt.Fprint(f, opRow("Instruction poll (GET /api/v1/instructions)", *s.InstructionPoll))
	}
	fmt.Fprintf(f, "\n_Latency percentiles are computed over successful requests only; error rate = "+
		"(failures+timeouts)/attempts over all measurement-phase requests. Cancelled = stopped by the "+
		"harness itself (Ctrl+C): not an attempt, not an error. See README.md for why._\n\n")

	fmt.Fprintf(f, "## Broker resource usage (--monitor-mode=%s)\n\n", s.BrokerResourceUsage.Mode)
	if s.BrokerResourceUsage.SampleCount == 0 {
		fmt.Fprintf(f, "No samples collected.\n")
	} else {
		fmt.Fprintf(f, "- Samples: %d\n", s.BrokerResourceUsage.SampleCount)
		fmt.Fprintf(f, "- CPU: avg %.3f, peak %.3f (%s)\n", s.BrokerResourceUsage.AvgCPU, s.BrokerResourceUsage.PeakCPU, s.BrokerResourceUsage.CPUUnit)
		fmt.Fprintf(f, "- Memory: avg %.1f MiB, peak %.1f MiB\n", s.BrokerResourceUsage.AvgMemMiB, s.BrokerResourceUsage.PeakMemMiB)
	}
	if len(s.BrokerResourceUsage.Errors) > 0 {
		fmt.Fprintf(f, "- %d sampling error(s) occurred (see summary.json for detail).\n", len(s.BrokerResourceUsage.Errors))
	}

	return os.WriteFile(filepath.Join(dir, "summary.md"), []byte(f.String()), 0o644)
}
