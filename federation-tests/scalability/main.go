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
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
)

// brokerCAFile is resolved once in run() (from resolveIdentities) and read
// by every provider/consumer goroutine spawned afterwards; see certs.go.
var brokerCAFile string

func main() {
	if len(os.Args) > 1 && os.Args[1] == "cleanup" {
		if err := runCleanup(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup error:", err)
			os.Exit(1)
		}
		return
	}

	fs := flag.NewFlagSet("broker-scalability-test", flag.ExitOnError)
	cfg := bindFlags(fs)
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(cfg *Config) error {
	providers, consumers, caFile, err := resolveIdentities(cfg)
	if err != nil {
		return fmt.Errorf("resolve agent identities: %w", err)
	}
	brokerCAFile = caFile
	log.Printf("resolved %d provider identit(y/ies) and %d consumer identit(y/ies) from %s",
		len(providers), len(consumers), identitySource(cfg))

	probeID, err := pickProbeIdentity(providers, consumers)
	if err != nil {
		return err
	}
	log.Printf("checking Broker reachability at %s ...", cfg.BrokerURL)
	if err := checkHealthz(context.Background(), cfg, probeID, caFile); err != nil {
		return fmt.Errorf("broker not reachable: %w (mTLS is mandatory — /healthz sits behind the same "+
			"ClusterIDMiddleware as every other route; see internal/broker/api/runnable.go)", err)
	}
	log.Printf("broker reachable")

	if err := writeJSONFile(cfg.OutputDir, "configuration.json", cfg); err != nil {
		return fmt.Errorf("write configuration.json: %w", err)
	}

	// rootCtx ends only on Ctrl+C/SIGTERM and is what every request runs
	// under. genCtx ends when the measurement does and only stops the load
	// generators from starting new requests: one already in flight completes,
	// so the end of the run is not recorded as a burst of Broker errors.
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	genCtx, stopGenerators := context.WithCancel(rootCtx)
	defer stopGenerators()

	collector := NewCollector()
	var monitorSamples []ResourceSample
	var monitorErrs []string

	monitor, err := newMonitor(cfg)
	if err != nil {
		return err
	}
	var monitorWG sync.WaitGroup
	if monitor != nil {
		monitorWG.Add(1)
		go func() {
			defer monitorWG.Done()
			monitor.Run(rootCtx)
		}()
	}

	start := time.Now()

	// --- warm-up: providers -------------------------------------------------
	var agentsWG sync.WaitGroup
	providerWarmupCh := make(chan warmupOutcome, len(providers))
	for i, id := range providers {
		agentsWG.Add(1)
		go func(id agentIdentity, index int) {
			defer agentsWG.Done()
			runProvider(genCtx, rootCtx, cfg, id, index, collector, providerWarmupCh)
		}(id, i+1)
	}
	providerWarmup := collectWarmup(len(providers), providerWarmupCh)
	log.Printf("provider warm-up: %d/%d ready in %.1fs", providerWarmup.Ready, providerWarmup.Total, providerWarmup.ElapsedMS/1000)
	if len(providerWarmup.TimedOut) > 0 {
		log.Printf("warning: providers that never advertised successfully: %v (proceeding anyway; see README Limitations)", providerWarmup.TimedOut)
	}

	// --- warm-up: consumers ---------------------------------------------------
	evalGate := make(chan struct{})
	consumerWarmupCh := make(chan warmupOutcome, len(consumers))
	for i, id := range consumers {
		agentsWG.Add(1)
		go func(id agentIdentity, index int) {
			defer agentsWG.Done()
			runConsumer(genCtx, rootCtx, cfg, id, index, collector, consumerWarmupCh, evalGate)
		}(id, i+1)
	}
	consumerWarmup := collectWarmup(len(consumers), consumerWarmupCh)
	log.Printf("consumer warm-up: %d/%d ready in %.1fs", consumerWarmup.Ready, consumerWarmup.Total, consumerWarmup.ElapsedMS/1000)
	if len(consumerWarmup.TimedOut) > 0 {
		log.Printf("warning: consumers that never heartbeated successfully: %v (proceeding anyway; see README Limitations)", consumerWarmup.TimedOut)
	}

	// Every consumer either heartbeated successfully or gave up — either way,
	// release the evaluation gate now so GET /api/v1/nodegroups traffic
	// starts from a stable baseline rather than hanging forever behind a
	// dead consumer's warm-up.
	measurementStart := time.Now()
	close(evalGate)
	log.Printf("measurement phase started: duration=%s", cfg.Duration)

	select {
	case <-time.After(cfg.Duration):
		log.Printf("measurement duration elapsed")
	case <-rootCtx.Done():
		log.Printf("interrupted — stopping early")
	}
	measurementEnd := time.Now()

	// Graceful shutdown, in two steps. First stop the generators from
	// starting new requests and let the ones in flight finish (bounded by
	// the per-request timeout); only then cancel everything else. Waiting
	// before the snapshot also means no request writes a record after it.
	stopGenerators()
	waitWithTimeout(&agentsWG, drainTimeout(cfg), "load generators")
	stop()
	waitWithTimeout(&monitorWG, 5*time.Second, "resource monitor")
	if monitor != nil {
		monitorSamples, monitorErrs = monitor.samples, monitor.errs
	}
	end := time.Now()

	records := collector.Snapshot()
	resUsage := computeResourceUsage(cfg.MonitorMode, monitorSamples, monitorErrs)
	summary := buildSummary(cfg, records, measurementEnd.Sub(measurementStart), start, end, providerWarmup, consumerWarmup, resUsage)

	if err := writeOutputs(cfg, records, monitorSamples, summary); err != nil {
		return fmt.Errorf("write outputs: %w", err)
	}

	log.Printf("done — results in %s", cfg.OutputDir)
	log.Printf("evaluation: %d attempts, %d success, %.1f%% error rate, p95=%.1fms",
		summary.Evaluation.Attempts, summary.Evaluation.Successes, summary.Evaluation.ErrorRate*100, summary.Evaluation.P95MS)
	return nil
}

func writeOutputs(cfg *Config, records []Record, samples []ResourceSample, summary Summary) error {
	if err := writeRecordCSV(cfg.OutputDir, "raw_evaluations.csv", records, func(r Record) bool {
		return r.Operation == OpEvaluation
	}); err != nil {
		return err
	}
	if err := writeRecordCSV(cfg.OutputDir, "raw_provider_requests.csv", records, func(r Record) bool {
		return r.AgentRole == "provider" && (r.Operation == OpAdvertisement || r.Operation == OpInstructions)
	}); err != nil {
		return err
	}
	// Extra file beyond the required minimum: heartbeat is Consumer-side, not
	// Provider-side (see design report §1), so it does not belong in
	// raw_provider_requests.csv. Kept separate rather than folded into
	// raw_evaluations.csv, which is GET /api/v1/nodegroups only.
	if err := writeRecordCSV(cfg.OutputDir, "raw_consumer_requests.csv", records, func(r Record) bool {
		return r.AgentRole == "consumer" && (r.Operation == OpHeartbeat || r.Operation == OpInstructions)
	}); err != nil {
		return err
	}
	if err := writeResourceUsageCSV(cfg.OutputDir, samples); err != nil {
		return err
	}
	if err := writeJSONFile(cfg.OutputDir, "summary.json", summary); err != nil {
		return err
	}
	return writeSummaryMarkdown(cfg.OutputDir, summary, cfg)
}

func identitySource(cfg *Config) string {
	if cfg.CertsDir != "" {
		return cfg.CertsDir
	}
	return cfg.TLSCert
}

func pickProbeIdentity(providers, consumers []agentIdentity) (agentIdentity, error) {
	if len(providers) > 0 {
		return providers[0], nil
	}
	if len(consumers) > 0 {
		return consumers[0], nil
	}
	return agentIdentity{}, fmt.Errorf("no provider or consumer identities resolved")
}

// checkHealthz performs a single mTLS GET /healthz. It intentionally does
// not use internal/agent/client.Client: GET /healthz is not one of the
// docs/design.md §7.3 endpoints that package wraps (server.go: "Liveness
// probe — not part of §7.3"), so this issues a raw request over the same
// mTLS transport instead of inventing a client method that doesn't exist
// upstream.
func checkHealthz(ctx context.Context, cfg *Config, id agentIdentity, caFile string) error {
	tlsCfg, err := (agentclient.TLSConfig{
		CertFile:     id.CertFile,
		KeyFile:      id.KeyFile,
		BrokerCAFile: caFile,
		ServerName:   cfg.BrokerServerName,
	}).BuildClientTLSConfig()
	if err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsCfg
	httpClient := &http.Client{Transport: transport, Timeout: cfg.RequestTimeout}

	reqCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, cfg.BrokerURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /healthz: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// collectWarmup drains exactly n warmupOutcome values (the generator
// goroutines each send exactly one, win or lose — see provider.go/consumer.go)
// and reports how many actually became ready.
func collectWarmup(n int, ch <-chan warmupOutcome) WarmupStats {
	start := time.Now()
	stats := WarmupStats{Total: n}
	for i := 0; i < n; i++ {
		o := <-ch
		if o.OK {
			stats.Ready++
		} else {
			stats.TimedOut = append(stats.TimedOut, o.ClusterID)
		}
	}
	stats.ElapsedMS = float64(time.Since(start)) / float64(time.Millisecond)
	return stats
}

// drainTimeout bounds how long the end of the run waits for the requests
// still in flight. One request can take every attempt the agent client makes
// -- the first plus its retries, each up to the per-attempt timeout, with a
// backoff of at most DefaultMaxBackoff between them -- and cutting it short
// would record it as cancelled rather than as the error it may turn into.
// The client treats a retry count of 0 or less as its default, and so does
// this bound.
func drainTimeout(cfg *Config) time.Duration {
	retries := cfg.ClientMaxRetries
	if retries <= 0 {
		retries = agentclient.DefaultMaxRetries
	}
	return time.Duration(retries+1)*cfg.RequestTimeout + time.Duration(retries)*agentclient.DefaultMaxBackoff +
		5*time.Second
}

func waitWithTimeout(wg *sync.WaitGroup, timeout time.Duration, what string) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("warning: %s did not stop within %s; continuing shutdown anyway", what, timeout)
	}
}
