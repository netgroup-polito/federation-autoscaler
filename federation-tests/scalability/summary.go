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
	"math"
	"sort"
	"time"
)

// OperationStats aggregates every measurement-phase Record for one
// Operation. Latency percentiles are computed over SUCCESSFUL requests
// only — a timed-out or failed call's "latency" is dominated by
// --request-timeout or an immediate connection-refused, neither of which
// describes how fast the Broker actually processed a request, so mixing
// them into the success-latency distribution would misrepresent Broker
// performance. Every individual record (successful or not) is still in the
// raw CSV, so nothing is hidden — this is purely a summary-statistics
// choice, documented here and in README.md.
//
// A request the harness itself cancelled (Ctrl+C) is counted apart and left
// out of Attempts and ErrorRate: it did not get an answer from the Broker,
// right or wrong, so it measures nothing about it.
type OperationStats struct {
	Operation Operation `json:"operation"`

	Attempts  int `json:"attempts"`
	Successes int `json:"successes"`
	Failures  int `json:"failures"`
	Timeouts  int `json:"timeouts"`
	Cancelled int `json:"cancelledByHarness"`

	MeanMS float64 `json:"meanLatencyMs"`
	P50MS  float64 `json:"p50LatencyMs"`
	P90MS  float64 `json:"p90LatencyMs"`
	P95MS  float64 `json:"p95LatencyMs"`
	P99MS  float64 `json:"p99LatencyMs"`
	MinMS  float64 `json:"minLatencyMs"`
	MaxMS  float64 `json:"maxLatencyMs"`

	SuccessPerSecond float64 `json:"successfulPerSecond"`
	SuccessPerMinute float64 `json:"successfulPerMinute"`
	ErrorRate        float64 `json:"errorAndTimeoutRate"` // (failures+timeouts)/attempts, 0..1
}

// computeStats aggregates one operation's records over the measurement phase.
// Warm-up records are deliberately left out: they are the federation coming
// up, not the steady-state load the run is there to measure.
func computeStats(records []Record, op Operation, duration time.Duration) OperationStats {
	stats := OperationStats{Operation: op}
	var successLatencies []float64

	for _, r := range records {
		if r.Operation != op || r.Phase != PhaseMeasurement {
			continue
		}
		if r.Outcome == OutcomeCancelled {
			stats.Cancelled++
			continue
		}
		stats.Attempts++
		switch r.Outcome {
		case OutcomeSuccess:
			stats.Successes++
			successLatencies = append(successLatencies, r.LatencyMS)
		case OutcomeTimeout:
			stats.Timeouts++
		default:
			stats.Failures++
		}
	}

	if stats.Attempts > 0 {
		stats.ErrorRate = float64(stats.Failures+stats.Timeouts) / float64(stats.Attempts)
	}
	if secs := duration.Seconds(); secs > 0 {
		stats.SuccessPerSecond = float64(stats.Successes) / secs
		stats.SuccessPerMinute = float64(stats.Successes) / (secs / 60)
	}

	sort.Float64s(successLatencies)
	stats.MeanMS = mean(successLatencies)
	stats.P50MS = percentile(successLatencies, 50)
	stats.P90MS = percentile(successLatencies, 90)
	stats.P95MS = percentile(successLatencies, 95)
	stats.P99MS = percentile(successLatencies, 99)
	if n := len(successLatencies); n > 0 {
		stats.MinMS = successLatencies[0]
		stats.MaxMS = successLatencies[n-1]
	}
	return stats
}

func mean(sorted []float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	return sum / float64(len(sorted))
}

// percentile uses the nearest-rank method on an already-ascending-sorted
// slice: rank = ceil(p/100 * n), 1-indexed, clamped into range.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// WarmupStats reports how the warm-up gate resolved for one role, so the
// summary shows whether every agent actually became ready before
// measurement traffic started (README "Warm-up Phase").
type WarmupStats struct {
	Total     int      `json:"total"`
	Ready     int      `json:"ready"`
	TimedOut  []string `json:"timedOutClusterIds,omitempty"`
	ElapsedMS float64  `json:"elapsedMs"`
}

// ResourceUsageSummary condenses every ResourceSample taken during the
// measurement phase. Mode/CPUUnit make the numbers self-describing since
// --monitor-mode changes what "CPU" and "RAM" actually mean (see monitor.go).
type ResourceUsageSummary struct {
	Mode        string   `json:"mode"`
	CPUUnit     string   `json:"cpuUnit,omitempty"`
	SampleCount int      `json:"sampleCount"`
	AvgCPU      float64  `json:"avgCpu"`
	PeakCPU     float64  `json:"peakCpu"`
	AvgMemMiB   float64  `json:"avgMemMib"`
	PeakMemMiB  float64  `json:"peakMemMib"`
	Errors      []string `json:"sampleErrors,omitempty"`
}

func computeResourceUsage(mode string, samples []ResourceSample, errs []string) ResourceUsageSummary {
	out := ResourceUsageSummary{Mode: mode, SampleCount: len(samples), Errors: errs}
	if len(samples) == 0 {
		return out
	}
	out.CPUUnit = samples[0].CPUUnit
	var sumCPU, sumMem float64
	for _, s := range samples {
		sumCPU += s.CPUValue
		sumMem += s.MemMiB
		if s.CPUValue > out.PeakCPU {
			out.PeakCPU = s.CPUValue
		}
		if s.MemMiB > out.PeakMemMiB {
			out.PeakMemMiB = s.MemMiB
		}
	}
	out.AvgCPU = sumCPU / float64(len(samples))
	out.AvgMemMiB = sumMem / float64(len(samples))
	return out
}

// Summary is the top-level machine-readable result written to summary.json
// (and rendered as summary.md).
type Summary struct {
	RunID                            string    `json:"runId"`
	StartTime                        time.Time `json:"startTime"`
	EndTime                          time.Time `json:"endTime"`
	ActualMeasurementDurationSeconds float64   `json:"actualMeasurementDurationSeconds"`

	Consumers int `json:"configuredConsumers"`
	Providers int `json:"configuredProviders"`

	ProviderWarmup WarmupStats `json:"providerWarmup"`
	ConsumerWarmup WarmupStats `json:"consumerWarmup"`

	Evaluation      OperationStats  `json:"evaluation"`
	Advertisement   OperationStats  `json:"advertisement"`
	Heartbeat       OperationStats  `json:"heartbeat"`
	InstructionPoll *OperationStats `json:"instructionPoll,omitempty"`

	BrokerResourceUsage ResourceUsageSummary `json:"brokerResourceUsage"`
}

func buildSummary(cfg *Config, records []Record, measurementDuration time.Duration, start, end time.Time,
	providerWarmup, consumerWarmup WarmupStats, resUsage ResourceUsageSummary) Summary {

	s := Summary{
		RunID:                            cfg.RunID,
		StartTime:                        start,
		EndTime:                          end,
		ActualMeasurementDurationSeconds: measurementDuration.Seconds(),
		Consumers:                        cfg.Consumers,
		Providers:                        cfg.Providers,
		ProviderWarmup:                   providerWarmup,
		ConsumerWarmup:                   consumerWarmup,
		Evaluation:                       computeStats(records, OpEvaluation, measurementDuration),
		Advertisement:                    computeStats(records, OpAdvertisement, measurementDuration),
		Heartbeat:                        computeStats(records, OpHeartbeat, measurementDuration),
		BrokerResourceUsage:              resUsage,
	}
	if cfg.InstructionPoll {
		st := computeStats(records, OpInstructions, measurementDuration)
		s.InstructionPoll = &st
	}
	return s
}
