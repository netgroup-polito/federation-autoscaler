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

// Command comparative-latency runs Phase A (Random) vs Phase B (Latency) with
// full automation: creates Kind clusters, deploys all components, applies tc
// netem delays, runs the experiment, collects results, and cleans up.
//
// Usage:
//
//	go run ./federation-tests/comparative-latency/ --config federation-tests/configs/latency.yaml
package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"sort"
	"sync"
	"time"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/federation-tests/testlib"
	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// proberCacheDrain is how long each phase waits, after starting its refresh
// goroutine and before its first sample, for every Consumer's prober cache to
// expire. Twice testlib.ProberCacheTTL: an entry cached just before the wait
// begins expires one TTL in, and the extra TTL is margin for a Consumer whose
// probe round was still in flight at that moment.
const proberCacheDrain = 2 * testlib.ProberCacheTTL

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run holds the actual program body so orch.Teardown (deferred) always
// executes on any error path, including a Setup failure — log.Fatal in main
// calls os.Exit, which would otherwise skip deferred cleanup and leave Kind
// clusters orphaned.
func run() error {
	configPath, keepClusters, skipBuild, runID := parseFlags()

	cfg, err := testlib.LoadAutoConfig(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	orch, err := testlib.NewOrchestrator(cfg, "comparative-latency", keepClusters, skipBuild, runID)
	if err != nil {
		return fmt.Errorf("orchestrator: %w", err)
	}

	defer orch.Teardown(context.Background())

	if err := orch.Setup(ctx); err != nil {
		return fmt.Errorf("setup failed: %w", err)
	}

	if err := runExperiment(ctx, orch); err != nil {
		return fmt.Errorf("experiment failed: %w", err)
	}
	return nil
}

// collectProbeEndpoints reads the UDP echo endpoint each provider advertises,
// which is what the consumers probe to measure RTT. Fewer than two of them
// means there is nothing to compare, so the run stops here rather than
// producing a one-provider "comparison".
func collectProbeEndpoints(ctx context.Context, clients *testlib.ExperimentClients) (map[string]string, error) {
	ngResp, err := clients.Broker.GetNodeGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("get nodegroups: %w", err)
	}
	endpoints := make(map[string]string)
	for _, ng := range ngResp.NodeGroups {
		if ng.ProbeEndpoint != "" {
			endpoints[ng.ProviderClusterID] = ng.ProbeEndpoint
		}
	}
	if len(endpoints) < 2 {
		return nil, fmt.Errorf("need >= 2 providers with ProbeEndpoint, got %d (check udpecho deployment)", len(endpoints))
	}
	log.Printf("probe endpoints: %v", endpoints)
	return endpoints, nil
}

// applyConsumerTCDelays installs the per-(consumer, provider) delay matrix
// with tc inside each consumer container, and restores whatever it managed to
// apply if one of them fails.
//
// Every consumer's matrix names the same providers, so each provider
// container's IP is resolved once and reused: ContainerIP shells out to
// `docker inspect`, and looking it up inside the inner loop cost one inspect
// per (consumer, provider) pair — 2100 of them at 30 consumers x 70
// providers, for 70 distinct answers.
func applyConsumerTCDelays(ctx context.Context, orch *testlib.Orchestrator,
	delays []testlib.ConsumerDelayConfig) ([]*testlib.TCConsumerDelay, error) {

	applied := make([]*testlib.TCConsumerDelay, 0, len(delays))
	provIPs := make(map[int]string, len(delays))
	for _, cd := range delays {
		containerName := orch.ConsumerContainerName(cd.ConsumerIndex)
		entries := make([]testlib.ProviderDelayEntry, 0, len(cd.ProviderDelays))
		for _, pd := range cd.ProviderDelays {
			provIP, ok := provIPs[pd.ProviderIndex]
			if !ok {
				provContainer := orch.ProviderContainerName(pd.ProviderIndex)
				ip, err := testlib.ContainerIP(ctx, provContainer)
				if err != nil {
					return nil, fmt.Errorf("get IP for provider-%d (%s): %w", pd.ProviderIndex, provContainer, err)
				}
				provIPs[pd.ProviderIndex] = ip
				provIP = ip
			}
			entries = append(entries, testlib.ProviderDelayEntry{
				ProviderIP: provIP,
				DelayMs:    pd.DelayMs,
				Label:      fmt.Sprintf("provider-%d", pd.ProviderIndex),
			})
		}
		tc := &testlib.TCConsumerDelay{
			ContainerName:  containerName,
			Interface:      "eth0",
			ProviderDelays: entries,
		}
		if err := tc.Apply(); err != nil {
			for _, done := range applied {
				_ = done.Restore()
			}
			return nil, fmt.Errorf("apply consumer tc on %s: %w", containerName, err)
		}
		applied = append(applied, tc)
	}
	return applied, nil
}

// applyProviderTCDelays installs one uniform delay per provider container
// (legacy mode), restoring what it applied if one of them fails.
func applyProviderTCDelays(orch *testlib.Orchestrator,
	delays []testlib.TCDelayAutoConfig) ([]*testlib.TCDelayKind, error) {

	applied := make([]*testlib.TCDelayKind, 0, len(delays))
	for _, tcd := range delays {
		containerName := orch.ProviderContainerName(tcd.ProviderIndex)
		iface := tcd.Interface
		if iface == "" {
			iface = "eth0"
		}
		tc := &testlib.TCDelayKind{
			ContainerName: containerName,
			Interface:     iface,
			DelayMs:       tcd.DelayMs,
		}
		log.Printf("  tc: %s +%dms", containerName, tcd.DelayMs)
		if err := tc.Apply(); err != nil {
			for _, done := range applied {
				_ = done.Restore()
			}
			return nil, fmt.Errorf("apply tc on %s: %w", containerName, err)
		}
		applied = append(applied, tc)
	}
	return applied, nil
}

func runExperiment(ctx context.Context, orch *testlib.Orchestrator) error {
	startTime := time.Now()
	cfg := orch.Config
	exp := cfg.Experiment
	clients := orch.Clients

	probeEndpoints, err := collectProbeEndpoints(ctx, clients)
	if err != nil {
		return err
	}

	// Clear stale policies.
	if err := clients.SetPolicyAll(ctx, "None", 0); err != nil {
		return fmt.Errorf("clear policies: %w", err)
	}

	// Apply tc delays BEFORE any phase so both phases run under identical
	// network conditions — the only variable between them is the policy.
	log.Println("=== APPLY TC DELAYS ===")
	var consumerTCs []*testlib.TCConsumerDelay
	var providerTCs []*testlib.TCDelayKind
	if len(exp.ConsumerDelays) > 0 {
		var err error
		if consumerTCs, err = applyConsumerTCDelays(ctx, orch, exp.ConsumerDelays); err != nil {
			return err
		}
		defer func() {
			for _, tc := range consumerTCs {
				log.Printf("  restoring consumer tc on %s", tc.ContainerName)
				if err := tc.Restore(); err != nil {
					log.Printf("  tc restore error on %s: %v", tc.ContainerName, err)
				}
			}
		}()
	} else {
		var err error
		if providerTCs, err = applyProviderTCDelays(orch, exp.TCDelaysAuto); err != nil {
			return err
		}
		defer func() {
			for _, tc := range providerTCs {
				log.Printf("  restoring tc on %s", tc.ContainerName)
				if err := tc.Restore(); err != nil {
					log.Printf("  tc restore error on %s: %v", tc.ContainerName, err)
				}
			}
		}()
	}

	// Snapshot the delays exactly as applied above: UpdateDelays/UpdateDelay
	// mutate consumerTCs/providerTCs in place on every refresh tick, so by
	// the end of Phase A they no longer hold their starting values. Phase B
	// resets to this snapshot before it starts so it replays Phase A's
	// exact delay sequence rather than continuing to drift from it.
	initialConsumerDelays := make([][]testlib.ProviderDelayEntry, len(consumerTCs))
	for i, tc := range consumerTCs {
		initialConsumerDelays[i] = append([]testlib.ProviderDelayEntry(nil), tc.ProviderDelays...)
	}
	initialProviderDelayMs := make([]int, len(providerTCs))
	for i, tc := range providerTCs {
		initialProviderDelayMs[i] = tc.DelayMs
	}

	var allSelections []testlib.SelectionRecord
	var allProbes []testlib.ProbeRecord
	var allReservations []testlib.ReservationRecord
	var allFederation []testlib.FederationSampleRecord
	mode := exp.Mode

	// Reused unchanged for both phases so Phase B's refresh goroutine draws
	// the exact same delay sequence Phase A drew (see refreshLatency).
	latencySeed := testlib.SeedFromString(orch.RunID)

	// The federation Phase A is about to run on; the transition below checks
	// Phase B gets the same one back. See comparative-eco for the reasoning --
	// it applies identically here, and more strongly: Random switches provider
	// on nearly every iteration, so Phase A does far more releases than the
	// Latency phase and has far more chances to leave a chunk stranded.
	baseline, err := testlib.ReadFederationCapacity(ctx, clients.Broker)
	if err != nil {
		return fmt.Errorf("baseline federation capacity: %w", err)
	}
	log.Printf("federation baseline: %d node groups, %d chunks reserved",
		len(baseline.NodeGroupIDs), baseline.TotalReserved)

	// --- Phase A: Random ---
	log.Println("=== PHASE A: Random ===")
	if err := clients.SetPolicyAll(ctx, "Random", exp.PolicyPropagationWait); err != nil {
		return fmt.Errorf("set Random: %w", err)
	}

	// Started here, after the policy wait and immediately before the phase's
	// own loop, so the gap between "the delay ticker's first tick" and "the
	// first sample" is the same in both phases. Phase B starts its refresh at
	// exactly this point in its own sequence; starting Phase A's any earlier
	// would put its ticker ahead by PolicyPropagationWait, and every sample
	// taken at the same elapsed time would then land on a different tick of
	// the replayed sequence — which is precisely what the replay is meant to
	// keep aligned.
	latencyRefreshCtxA, cancelLatencyRefreshA := context.WithCancel(ctx)
	var latencyWGA sync.WaitGroup
	latencyWGA.Add(1)
	go func() {
		defer latencyWGA.Done()
		refreshLatency(latencyRefreshCtxA, exp, consumerTCs, providerTCs, rand.New(rand.NewSource(latencySeed)))
	}()

	// Wait out the Consumers' prober cache before sampling. This side does not
	// strictly need it -- the caches are cold here -- but Phase B does, and the
	// gap between "the refresh ticker's first tick" and "the first sample" has
	// to be the same on both sides or samples taken at the same elapsed time
	// land on different ticks of the replayed sequence. Symmetry is the point;
	// see the matching wait at the start of Phase B.
	if err := testlib.SleepCtx(ctx, proberCacheDrain); err != nil {
		cancelLatencyRefreshA()
		latencyWGA.Wait()
		return err
	}

	var phaseASel []testlib.SelectionRecord
	var phaseAProbe []testlib.ProbeRecord
	if mode == testlib.ModeReserve {
		var phaseARes []testlib.ReservationRecord
		var phaseAFed []testlib.FederationSampleRecord
		phaseASel, phaseAProbe, phaseARes, phaseAFed, err = runReservePhase(ctx, orch, clients, probeEndpoints, testlib.PhaseA, "Random")
		allReservations = append(allReservations, phaseARes...)
		allFederation = append(allFederation, phaseAFed...)
	} else {
		phaseASel, phaseAProbe, err = runLatencyPhase(ctx, orch, clients, probeEndpoints, testlib.PhaseA, "Random")
	}
	cancelLatencyRefreshA()
	latencyWGA.Wait()
	if err != nil {
		return fmt.Errorf("phase A: %w", err)
	}
	allSelections = append(allSelections, phaseASel...)
	allProbes = append(allProbes, phaseAProbe...)
	log.Printf("phase A complete: %d samples", len(phaseASel))

	// --- Transition: switch to Latency (tc delays already active) ---
	log.Println("=== TRANSITION ===")

	// Before the policy switch and before Phase B's refresh goroutine: this can
	// block for up to testlib.FederationSettleTimeout, and a variable-length
	// wait after the goroutine would shift Phase B's tick alignment away from
	// Phase A's.
	log.Println("  checking the federation is back to its starting capacity...")
	if _, err := testlib.WaitForFederationCapacity(ctx, clients.Broker, baseline, exp.ReservationPoll, testlib.FederationSettleTimeout); err != nil {
		return fmt.Errorf("phase A leaked capacity, so phase B would not run on the same federation: %w", err)
	}

	if err := clients.SetPolicyAll(ctx, "Latency", exp.PolicyPropagationWait); err != nil {
		return fmt.Errorf("set Latency: %w", err)
	}

	// Reset every tc to its Phase A starting value so Phase B's refresh
	// goroutine (seeded identically below) redraws the exact same sequence
	// of delays Phase A saw, instead of continuing from wherever Phase A's
	// jitter happened to leave off.
	for i, tc := range consumerTCs {
		if err := tc.UpdateDelays(initialConsumerDelays[i]); err != nil {
			return fmt.Errorf("reset consumer tc on %s for phase B replay: %w", tc.ContainerName, err)
		}
	}
	for i, tc := range providerTCs {
		if err := tc.UpdateDelay(initialProviderDelayMs[i]); err != nil {
			return fmt.Errorf("reset tc on %s for phase B replay: %w", tc.ContainerName, err)
		}
	}

	// --- Phase B: Latency ---
	log.Println("=== PHASE B: Latency ===")
	latencyRefreshCtxB, cancelLatencyRefreshB := context.WithCancel(ctx)
	var latencyWGB sync.WaitGroup
	latencyWGB.Add(1)
	go func() {
		defer latencyWGB.Done()
		refreshLatency(latencyRefreshCtxB, exp, consumerTCs, providerTCs, rand.New(rand.NewSource(latencySeed)))
	}()

	// The reason the wait exists. Every Consumer arrives here holding RTTs it
	// measured against Phase A's final delays, and would keep serving them for
	// up to testlib.ProberCacheTTL into Phase B -- against the tc values this
	// phase has just reset. Phase A began with cold caches and no such stale
	// window, so without draining it here the two phases would not be
	// observable as the same environment over their first samples.
	if err := testlib.SleepCtx(ctx, proberCacheDrain); err != nil {
		cancelLatencyRefreshB()
		latencyWGB.Wait()
		return err
	}

	var phaseBSel []testlib.SelectionRecord
	var phaseBProbe []testlib.ProbeRecord
	if mode == testlib.ModeReserve {
		var phaseBRes []testlib.ReservationRecord
		var phaseBFed []testlib.FederationSampleRecord
		phaseBSel, phaseBProbe, phaseBRes, phaseBFed, err = runReservePhase(ctx, orch, clients, probeEndpoints, testlib.PhaseB, "Latency")
		allReservations = append(allReservations, phaseBRes...)
		allFederation = append(allFederation, phaseBFed...)
	} else {
		phaseBSel, phaseBProbe, err = runLatencyPhase(ctx, orch, clients, probeEndpoints, testlib.PhaseB, "Latency")
	}
	cancelLatencyRefreshB()
	latencyWGB.Wait()
	if err != nil {
		return fmt.Errorf("phase B: %w", err)
	}
	allSelections = append(allSelections, phaseBSel...)
	allProbes = append(allProbes, phaseBProbe...)
	log.Printf("phase B complete: %d samples", len(phaseBSel))

	// --- Cleanup ---
	log.Println("=== EXPERIMENT CLEANUP ===")
	if err := clients.SetPolicyAll(ctx, "None", 0); err != nil {
		log.Printf("cleanup: clear policies: %v", err)
	}

	// --- Write results ---
	summary, err := writeResults(ctx, orch, startTime, experimentRecords{
		mode:         mode,
		selections:   allSelections,
		probes:       allProbes,
		reservations: allReservations,
		federation:   allFederation,
		phaseASel:    phaseASel,
		phaseAProbe:  phaseAProbe,
		phaseBSel:    phaseBSel,
		phaseBProbe:  phaseBProbe,
	})
	if err != nil {
		return err
	}

	log.Println("=== DONE ===")
	printSummary(summary)
	return nil
}

// experimentRecords is everything a finished run has to write out.
type experimentRecords struct {
	mode         string
	selections   []testlib.SelectionRecord
	probes       []testlib.ProbeRecord
	reservations []testlib.ReservationRecord
	federation   []testlib.FederationSampleRecord
	phaseASel    []testlib.SelectionRecord
	phaseAProbe  []testlib.ProbeRecord
	phaseBSel    []testlib.SelectionRecord
	phaseBProbe  []testlib.ProbeRecord
}

// writeResults writes every CSV plus summary.json/summary.md, and returns the
// summary it wrote so the caller can print it.
func writeResults(ctx context.Context, orch *testlib.Orchestrator, startTime time.Time,
	rec experimentRecords) (testlib.ExperimentSummary, error) {

	cfg := orch.Config
	exp := cfg.Experiment
	clients := orch.Clients
	outputDir := orch.OutputDir
	log.Printf("writing results to %s", outputDir)

	if err := testlib.WriteSelectionCSV(outputDir, "selections.csv", rec.selections); err != nil {
		return testlib.ExperimentSummary{}, fmt.Errorf("write selections CSV: %w", err)
	}
	if err := testlib.WriteProbeCSV(outputDir, "probes.csv", rec.probes); err != nil {
		return testlib.ExperimentSummary{}, fmt.Errorf("write probes CSV: %w", err)
	}
	if rec.mode == testlib.ModeReserve && len(rec.reservations) > 0 {
		if err := testlib.WriteReservationCSV(outputDir, "reservations.csv", rec.reservations); err != nil {
			return testlib.ExperimentSummary{}, fmt.Errorf("write reservations CSV: %w", err)
		}
	}
	if len(rec.federation) > 0 {
		if err := testlib.WriteFederationCSV(outputDir, "federation.csv", rec.federation); err != nil {
			return testlib.ExperimentSummary{}, fmt.Errorf("write federation CSV: %w", err)
		}
	}

	brokerURL, _ := orch.BrokerURL(ctx)
	consoleURL, _ := orch.ConsoleURL(ctx, 0)

	timerConfigured := ""
	if exp.IsTimeBased() {
		timerConfigured = exp.Timer.String()
	}

	summary := testlib.ExperimentSummary{
		RunID:              orch.RunID,
		TestType:           "comparative-latency",
		StartTime:          startTime,
		EndTime:            time.Now(),
		ConsumerID:         clients.Identity.ClusterID,
		ConsumerCertFP:     clients.CertFP,
		BrokerURL:          brokerURL,
		ConsoleURL:         consoleURL,
		ProviderCount:      cfg.Providers,
		IterationsPerPhase: exp.Iterations,
		DurationMode:       exp.Duration,
		TimerConfigured:    timerConfigured,
		PhaseAPolicy:       "Random",
		PhaseBPolicy:       "Latency",
		PhaseASummary:      summarizeLatencyPhase(rec.phaseASel, rec.phaseAProbe),
		PhaseBSummary:      summarizeLatencyPhase(rec.phaseBSel, rec.phaseBProbe),
	}
	if err := testlib.WriteJSONFile(outputDir, "summary.json", summary); err != nil {
		return testlib.ExperimentSummary{}, fmt.Errorf("write summary: %w", err)
	}
	if err := testlib.WriteSummaryMarkdown(outputDir, summary); err != nil {
		return testlib.ExperimentSummary{}, fmt.Errorf("write markdown: %w", err)
	}
	return summary, nil
}

// runLatencyPhase drives one probe/select cycle per iteration. Consumers are
// dispatched concurrently (one goroutine each) within an iteration, mirroring
// how independent real Consumer Agents behave in production. All consumers
// in an iteration are joined before the next iteration's phasePause, so the
// iteration cadence is unchanged; only the per-consumer work inside each
// iteration is now parallel instead of serial.
func runLatencyPhase(ctx context.Context, orch *testlib.Orchestrator, clients *testlib.ExperimentClients, endpoints map[string]string, phase, policy string) ([]testlib.SelectionRecord, []testlib.ProbeRecord, error) {
	exp := orch.Config.Experiment
	var mu sync.Mutex
	var selections []testlib.SelectionRecord
	var probes []testlib.ProbeRecord

	consumerIDs := make([]string, 0, len(clients.Consoles))
	for id := range clients.Consoles {
		consumerIDs = append(consumerIDs, id)
	}
	sort.Strings(consumerIDs)

	var loopErr error
	if exp.IsTimeBased() {
		deadline := time.Now().Add(exp.Timer)
		var wg sync.WaitGroup
		for _, consID := range consumerIDs {
			wg.Add(1)
			go func(consID string) {
				defer wg.Done()
				for i := 1; ctx.Err() == nil && time.Now().Before(deadline); i++ {
					runLatencyConsumerIteration(ctx, clients, endpoints, phase, policy, i, consID, &mu, &selections, &probes)
					if time.Now().Before(deadline) {
						if testlib.SleepCtx(ctx, exp.PhasePause) != nil {
							return
						}
					}
				}
			}(consID)
		}
		wg.Wait()
		loopErr = ctx.Err()
	} else {
		for i := 1; i <= exp.Iterations; i++ {
			if ctx.Err() != nil {
				loopErr = ctx.Err()
				break
			}

			var wg sync.WaitGroup
			for _, consID := range consumerIDs {
				wg.Add(1)
				go func(consID string) {
					defer wg.Done()
					runLatencyConsumerIteration(ctx, clients, endpoints, phase, policy, i, consID, &mu, &selections, &probes)
				}(consID)
			}
			wg.Wait()

			if i < exp.Iterations {
				if err := testlib.SleepCtx(ctx, exp.PhasePause); err != nil {
					loopErr = err
					break
				}
			}
		}
	}

	return selections, probes, loopErr
}

// runLatencyConsumerIteration is the per-consumer, per-iteration body of
// runLatencyPhase, safe to run concurrently with other consumers' calls: it
// only touches shared state (selections, probes) behind mu.
func runLatencyConsumerIteration(ctx context.Context, clients *testlib.ExperimentClients, endpoints map[string]string, phase, policy string, i int, consID string, mu *sync.Mutex, selections *[]testlib.SelectionRecord, probes *[]testlib.ProbeRecord) {
	console := clients.Consoles[consID]
	broker := clients.BrokerFor(consID)
	start := time.Now()
	rec := testlib.SelectionRecord{
		Timestamp:  start,
		ConsumerID: consID,
		Phase:      phase,
		Policy:     policy,
		Iteration:  i,
	}
	appendSel := func(r testlib.SelectionRecord) {
		mu.Lock()
		*selections = append(*selections, r)
		mu.Unlock()
	}
	appendProbe := func(p testlib.ProbeRecord) {
		mu.Lock()
		*probes = append(*probes, p)
		mu.Unlock()
	}

	ngResp, err := broker.GetNodeGroups(ctx)
	if err != nil {
		rec.Outcome = "error"
		rec.ErrorMessage = fmt.Sprintf("get nodegroups: %v", err)
		rec.DurationMs = msSince(start)
		appendSel(rec)
		log.Printf("[%s] iter %d %s: nodegroups error: %v", phase, i, consID, err)
		return
	}

	if ngResp.LatencyShortlist {
		growable := testlib.GrowableNodeGroups(ngResp.NodeGroups)
		var candidates []testlib.ProbeCandidate
		for _, ng := range growable {
			if ep, ok := endpoints[ng.ProviderClusterID]; ok {
				candidates = append(candidates, testlib.ProbeCandidate{
					ProviderClusterID: ng.ProviderClusterID,
					Endpoint:          ep,
				})
			}
		}
		if len(candidates) == 0 {
			rec.Outcome = "no-candidates"
			rec.ErrorMessage = "no growable with ProbeEndpoint"
			rec.DurationMs = msSince(start)
			appendSel(rec)
			return
		}

		probeResp, err := console.Probe(ctx, candidates)
		if err != nil {
			rec.Outcome = "probe-error"
			rec.ErrorMessage = err.Error()
			rec.DurationMs = msSince(start)
			appendSel(rec)
			return
		}

		rec.SelectedID = probeResp.Chosen
		if ng := testlib.FindNodeGroupByProvider(ngResp.NodeGroups, probeResp.Chosen); ng != nil {
			rec.NodeGroupID = ng.ID
		}
		if rtt, ok := probeResp.RTTs[probeResp.Chosen]; ok {
			rec.RTTMs = rtt
		}
		rec.Outcome = testlib.OutcomeSuccess
		rec.DurationMs = msSince(start)
		appendSel(rec)

		appendProbe(testlib.ProbeRecord{
			Timestamp:  start,
			ConsumerID: consID,
			Phase:      phase,
			Policy:     policy,
			Iteration:  i,
			Chosen:     probeResp.Chosen,
			RTTs:       probeResp.RTTs,
			DurationMs: probeResp.Duration,
		})
	} else {
		winner := testlib.FindWinner(ngResp.NodeGroups)
		if winner == nil {
			rec.Outcome = "no-winner"
			rec.DurationMs = msSince(start)
			appendSel(rec)
			return
		}

		rec.SelectedID = winner.ProviderClusterID
		rec.NodeGroupID = winner.ID

		if ep, ok := endpoints[winner.ProviderClusterID]; ok {
			probeResp, err := console.Probe(ctx, []testlib.ProbeCandidate{{
				ProviderClusterID: winner.ProviderClusterID,
				Endpoint:          ep,
			}})
			if err == nil {
				if rtt, ok := probeResp.RTTs[winner.ProviderClusterID]; ok {
					rec.RTTMs = rtt
				}
				appendProbe(testlib.ProbeRecord{
					Timestamp:  start,
					ConsumerID: consID,
					Phase:      phase,
					Policy:     policy,
					Iteration:  i,
					Chosen:     winner.ProviderClusterID,
					RTTs:       probeResp.RTTs,
					DurationMs: probeResp.Duration,
				})
			}
		}

		rec.Outcome = testlib.OutcomeSuccess
		rec.DurationMs = msSince(start)
		appendSel(rec)
	}

	log.Printf("[%s] iter %02d %s: winner=%-20s rtt=%8.2fms shortlist=%v",
		phase, i, consID, rec.SelectedID, rec.RTTMs, ngResp.LatencyShortlist)
}

// latencyReservePhaseState is the shared, mutex-protected state one
// runReservePhase call accumulates across all consumers and iterations. Every
// consumer's own entries in active/seqNo are only ever touched by that
// consumer's own goroutine, but Go maps are not safe for concurrent access
// from multiple goroutines even across disjoint keys, so every access —
// reads included — goes through mu.
type latencyReservePhaseState struct {
	mu                sync.Mutex
	selections        []testlib.SelectionRecord
	probes            []testlib.ProbeRecord
	reservations      []testlib.ReservationRecord
	federationSamples []testlib.FederationSampleRecord
	active            map[string]*testlib.ConsumerReservation
	seqNo             map[string]int
}

func newLatencyReservePhaseState() *latencyReservePhaseState {
	return &latencyReservePhaseState{
		active: make(map[string]*testlib.ConsumerReservation),
		seqNo:  make(map[string]int),
	}
}

func (s *latencyReservePhaseState) addSelection(sel testlib.SelectionRecord) {
	s.mu.Lock()
	s.selections = append(s.selections, sel)
	s.mu.Unlock()
}

func (s *latencyReservePhaseState) addProbe(p testlib.ProbeRecord) {
	s.mu.Lock()
	s.probes = append(s.probes, p)
	s.mu.Unlock()
}

func (s *latencyReservePhaseState) addReservation(res testlib.ReservationRecord) {
	s.mu.Lock()
	s.reservations = append(s.reservations, res)
	s.mu.Unlock()
}

func (s *latencyReservePhaseState) addFederationSamples(samples []testlib.FederationSampleRecord) {
	s.mu.Lock()
	s.federationSamples = append(s.federationSamples, samples...)
	s.mu.Unlock()
}

func (s *latencyReservePhaseState) getActive(consID string) *testlib.ConsumerReservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[consID]
}

func (s *latencyReservePhaseState) setActive(consID string, res *testlib.ConsumerReservation) {
	s.mu.Lock()
	s.active[consID] = res
	s.mu.Unlock()
}

func (s *latencyReservePhaseState) deleteActive(consID string) {
	s.mu.Lock()
	delete(s.active, consID)
	s.mu.Unlock()
}

func (s *latencyReservePhaseState) nextSeq(consID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := s.seqNo[consID]
	s.seqNo[consID] = seq + 1
	return seq
}

// runReservePhase drives one reserve/keep/switch cycle per iteration.
// Consumers are dispatched concurrently (one goroutine each) within an
// iteration, mirroring how independent real Consumer Agents behave in
// production — each probes/reserves/releases on its own timeline, none
// blocks behind another's Liqo peering. All consumers in an iteration are
// joined before the next iteration's phasePause, so the iteration cadence is
// unchanged; only the per-consumer work inside each iteration is now
// parallel instead of serial.
func runReservePhase(ctx context.Context, orch *testlib.Orchestrator, clients *testlib.ExperimentClients, endpoints map[string]string, phase, policy string) ([]testlib.SelectionRecord, []testlib.ProbeRecord, []testlib.ReservationRecord, []testlib.FederationSampleRecord, error) {
	exp := orch.Config.Experiment
	pollInterval := exp.ReservationPoll

	consumerIDs := make([]string, 0, len(clients.Consoles))
	for id := range clients.Consoles {
		consumerIDs = append(consumerIDs, id)
	}
	sort.Strings(consumerIDs)

	state := newLatencyReservePhaseState()

	defer func() {
		for consID, res := range state.active {
			if res == nil {
				continue
			}
			log.Printf("[%s] cleanup: releasing %s (%s)", phase, res.ReservationID, consID)
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if err := clients.BrokerFor(consID).ReleaseAndWait(cleanupCtx, res.ReservationID, res.Request, pollInterval); err != nil {
				log.Printf("[%s] cleanup: release error %s: %v", phase, res.ReservationID, err)
			}
			cancel()
		}
	}()

	// Federation-wide sampler: independent of the iteration cadence, ticks on
	// its own schedule for the whole phase and records where each consumer
	// stands right now, not just what changed.
	sampleCtx, cancelSample := context.WithCancel(ctx)
	var sampleWG sync.WaitGroup
	sampleWG.Add(1)
	go func() {
		defer sampleWG.Done()
		runFederationSampler(sampleCtx, exp.FederationSampleInterval, func() {
			state.addFederationSamples(sampleFederationLatency(ctx, clients, endpoints, consumerIDs, phase, policy, state))
		})
	}()

	var loopErr error
	if exp.IsTimeBased() {
		loopErr = runReservePhaseTimed(ctx, orch, clients, endpoints, phase, policy, pollInterval, exp, state, consumerIDs)
	} else {
		loopErr = runReservePhaseCounted(ctx, orch, clients, endpoints, phase, policy, pollInterval, exp, state, consumerIDs)
	}

	cancelSample()
	sampleWG.Wait()

	return state.selections, state.probes, state.reservations, state.federationSamples, loopErr
}

// runReservePhaseCounted is the original, unchanged loop: exp.Iterations
// synchronized rounds, every consumer dispatched concurrently within each
// round and joined before the next round's phasePause.
func runReservePhaseCounted(ctx context.Context, orch *testlib.Orchestrator, clients *testlib.ExperimentClients, endpoints map[string]string, phase, policy string, pollInterval time.Duration, exp testlib.TestParams, state *latencyReservePhaseState, consumerIDs []string) error {
	for i := 1; i <= exp.Iterations; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var wg sync.WaitGroup
		for _, consID := range consumerIDs {
			wg.Add(1)
			go func(consID string) {
				defer wg.Done()
				runLatencyReserveConsumerIteration(ctx, orch, clients, endpoints, phase, policy, i, consID, pollInterval, exp, state)
			}(consID)
		}
		wg.Wait()

		if i < exp.Iterations {
			if err := testlib.SleepCtx(ctx, exp.PhasePause); err != nil {
				return err
			}
		}
	}
	return nil
}

// runReservePhaseTimed runs the phase for exp.Timer wall-clock duration
// instead of a fixed iteration count. Each consumer loops independently on
// its own local iteration counter and its own phasePause, rather than
// waiting for the others each round — one consumer may complete more
// iterations than another in the same window.
func runReservePhaseTimed(ctx context.Context, orch *testlib.Orchestrator, clients *testlib.ExperimentClients, endpoints map[string]string, phase, policy string, pollInterval time.Duration, exp testlib.TestParams, state *latencyReservePhaseState, consumerIDs []string) error {
	deadline := time.Now().Add(exp.Timer)
	var wg sync.WaitGroup
	for _, consID := range consumerIDs {
		wg.Add(1)
		go func(consID string) {
			defer wg.Done()
			for i := 1; ctx.Err() == nil && time.Now().Before(deadline); i++ {
				runLatencyReserveConsumerIteration(ctx, orch, clients, endpoints, phase, policy, i, consID, pollInterval, exp, state)
				if time.Now().Before(deadline) {
					if testlib.SleepCtx(ctx, exp.PhasePause) != nil {
						return
					}
				}
			}
		}(consID)
	}
	wg.Wait()
	return ctx.Err()
}

// runFederationSampler ticks sample on interval until ctx is done, firing
// once immediately first so a very short phase still gets one snapshot.
func runFederationSampler(ctx context.Context, interval time.Duration, sample func()) {
	sample()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sample()
		}
	}
}

// sampleFederationLatency snapshots every consumer's currently-held provider
// and the live RTT to it. Unlike carbon intensity, RTT is a per-(consumer,
// provider) pair value that depends on the calling consumer's own network
// path, so it takes one UDP probe per consumer rather than a single shared
// lookup — run concurrently so the tick's duration is one probe, not their sum.
func sampleFederationLatency(ctx context.Context, clients *testlib.ExperimentClients, endpoints map[string]string, consumerIDs []string, phase, policy string, state *latencyReservePhaseState) []testlib.FederationSampleRecord {
	ts := time.Now()
	var mu sync.Mutex
	samples := make([]testlib.FederationSampleRecord, 0, len(consumerIDs))
	var wg sync.WaitGroup
	for _, consID := range consumerIDs {
		wg.Add(1)
		go func(consID string) {
			defer wg.Done()
			rec := testlib.FederationSampleRecord{
				Timestamp: ts, Phase: phase, Policy: policy, ConsumerID: consID,
				MetricType: "rtt_ms",
			}
			if res := state.getActive(consID); res != nil {
				rec.ProviderClusterID = res.ProviderClusterID
				rec.ReservationID = res.ReservationID
				if ep, ok := endpoints[res.ProviderClusterID]; ok {
					if resp, err := clients.Consoles[consID].Probe(ctx, []testlib.ProbeCandidate{{
						ProviderClusterID: res.ProviderClusterID,
						Endpoint:          ep,
					}}); err == nil {
						if rtt, ok := resp.RTTs[res.ProviderClusterID]; ok {
							rec.MetricValue, rec.HasMetric = rtt, true
						}
					}
				}
			}
			mu.Lock()
			samples = append(samples, rec)
			mu.Unlock()
		}(consID)
	}
	wg.Wait()
	return samples
}

// maxReserveRaceRetries bounds how many times a consumer re-picks (re-probing
// if needed) after losing a race for the last chunk on its chosen provider:
// two consumers can both read the same "provider X has room" snapshot, only
// one PostReservation lands, and the loser's view is now stale. A fresh
// GetNodeGroups naturally exposes the next-best candidate — the Broker only
// ever leaves providers with headroom unmasked — so retrying mirrors what
// the real, continuously-reconciling ResourceRequest controller / Cluster
// Autoscaler would do on its next tick, just resolved within this iteration.
//
// 5 was tight at high concurrency: with many consumers converging on a few
// exposed candidates (Eco's single best-with-headroom, Latency's top-3
// shortlist), a consumer unlucky in the spread cascade could exhaust its
// budget before ever reaching a provider with room — observed as ~10% of
// iterations failing on pure capacity exhaustion in a 30-consumer/70-provider
// run. Raised to give more room to find a free slot; the cost only lands on
// iterations with real contention.
const maxReserveRaceRetries = 10

// raceRetryBackoff is how long to wait before retrying after a 429
// (per-cluster rate limit — 10 burst / 5rps, see internal/broker/api's
// RateLimitMiddleware). Unlike a lost capacity race, hammering again
// immediately just refires the same limiter; the token bucket refills at
// 5/s, so this leaves ample margin. 409 (capacity) and 5xx (e.g. a
// provider's advertisement gone stale) get no backoff — the retry loop
// already re-reads GetNodeGroups from scratch, which is the correct
// response to both: a different provider, or the same one once fresh.
const raceRetryBackoff = 750 * time.Millisecond

// runLatencyReserveConsumerIteration is the per-consumer, per-iteration body
// of runReservePhase, safe to run concurrently with other consumers' calls:
// every access to shared state goes through state's own locking.
// latencyChoiceInput is what picking a provider for one iteration needs: the
// clients to talk to, where to record the outcome, and the iteration's own
// identity for the records it writes.
type latencyChoiceInput struct {
	ctx               context.Context
	console           *testlib.ConsoleClient
	state             *latencyReservePhaseState
	endpoints         map[string]string
	phase             string
	policy            string
	consID            string
	iteration         int
	attempt           int
	start             time.Time
	initialProvider   string
	cur               *testlib.ConsumerReservation
	keepNoAlternative func(reason string)
}

// latencyChoice is the provider one iteration decided to aim for, with the
// RTT measurement that led there.
type latencyChoice struct {
	providerID  string
	nodeGroupID string
	rtt         float64
	probe       *testlib.ProbeRecord
	// hadAlternatives is false when the Broker offered nowhere to move to.
	// The Random branch always yields a winner distinct from the incumbent's
	// masking, so only the shortlist branch can find itself with the
	// incumbent as the sole candidate.
	hadAlternatives bool
}

// chooseLatencyProvider picks the provider this iteration should end up on:
// by probing the Broker's shortlist under the Latency policy, or by taking
// the single winner the Broker masked to under Random. The second return
// value is true when the iteration's outcome has already been recorded (no
// candidate, no winner, probe failure) and the caller must stop.
func chooseLatencyProvider(it latencyChoiceInput, ngResp *brokerapi.NodeGroupListResponse) (latencyChoice, bool) {
	if ngResp.LatencyShortlist {
		return chooseByProbe(it, ngResp)
	}
	return chooseBrokerWinner(it, ngResp)
}

// chooseByProbe measures every shortlisted provider from the consumer's own
// network namespace and takes the fastest.
func chooseByProbe(it latencyChoiceInput, ngResp *brokerapi.NodeGroupListResponse) (latencyChoice, bool) {
	growable := testlib.GrowableNodeGroups(ngResp.NodeGroups)
	candidates := make([]testlib.ProbeCandidate, 0, len(growable)+1)
	for _, ng := range growable {
		if ep, ok := it.endpoints[ng.ProviderClusterID]; ok {
			candidates = append(candidates, testlib.ProbeCandidate{
				ProviderClusterID: ng.ProviderClusterID,
				Endpoint:          ep,
			})
		}
	}
	// Whether the Broker offered anywhere to move to, decided before the
	// incumbent is added below so a lone incumbent still reads as "nothing
	// else was free".
	choice := latencyChoice{hadAlternatives: len(candidates) > 0}

	// Probe the provider we are already on, even though the Broker masked it.
	// Its shortlist only carries providers with head-room, and a consumer
	// holding the last chunk of its own provider fills it — so the incumbent
	// never appeared, was never measured, and the switch test (which requires
	// the incumbent's RTT) could only ever answer "keep". Measuring it makes
	// the choice a real comparison between where we are and where we could
	// go, and gives the keep rows the RTT of the provider actually kept
	// instead of some other candidate's.
	if it.cur != nil {
		if ep, ok := it.endpoints[it.cur.ProviderClusterID]; ok && !hasCandidate(candidates, it.cur.ProviderClusterID) {
			candidates = append(candidates, testlib.ProbeCandidate{
				ProviderClusterID: it.cur.ProviderClusterID,
				Endpoint:          ep,
			})
		}
	}

	if len(candidates) == 0 {
		if it.cur != nil {
			it.keepNoAlternative(fmt.Sprintf("growable=%d, none with ProbeEndpoint", len(growable)))
			return choice, true
		}
		it.state.addSelection(it.record("no-candidates", "no growable with ProbeEndpoint"))
		log.Printf("[%s] iter %d %s: no probe candidates (growable=%d)",
			it.phase, it.iteration, it.consID, len(growable))
		return choice, true
	}

	probeResp, err := it.console.Probe(it.ctx, candidates)
	if err != nil {
		it.state.addSelection(it.record("probe-error", err.Error()))
		return choice, true
	}

	choice.providerID = probeResp.Chosen
	if ng := testlib.FindNodeGroupByProvider(ngResp.NodeGroups, probeResp.Chosen); ng != nil {
		choice.nodeGroupID = ng.ID
	}
	if rtt, ok := probeResp.RTTs[probeResp.Chosen]; ok {
		choice.rtt = rtt
	}
	choice.probe = it.probeRecord(probeResp.Chosen, probeResp.RTTs, probeResp.Duration)
	return choice, false
}

// chooseBrokerWinner takes the single provider the Broker masked to (Random
// phase) and measures it, so the baseline rows carry an RTT too.
func chooseBrokerWinner(it latencyChoiceInput, ngResp *brokerapi.NodeGroupListResponse) (latencyChoice, bool) {
	choice := latencyChoice{hadAlternatives: true}

	winner := testlib.FindWinner(ngResp.NodeGroups)
	if winner == nil {
		growable := testlib.GrowableNodeGroups(ngResp.NodeGroups)
		if it.cur != nil {
			it.keepNoAlternative(fmt.Sprintf("growable=%d", len(growable)))
			return choice, true
		}
		it.state.addSelection(it.record("no-winner",
			fmt.Sprintf("growable=%d applied=%s", len(growable), ngResp.AppliedPlacement)))
		log.Printf("[%s] iter %d %s: no single winner (growable=%d)", it.phase, it.iteration, it.consID, len(growable))
		return choice, true
	}
	choice.providerID = winner.ProviderClusterID
	choice.nodeGroupID = winner.ID

	if ep, ok := it.endpoints[winner.ProviderClusterID]; ok {
		probeResp, err := it.console.Probe(it.ctx, []testlib.ProbeCandidate{{
			ProviderClusterID: winner.ProviderClusterID,
			Endpoint:          ep,
		}})
		if err == nil {
			if rtt, ok := probeResp.RTTs[winner.ProviderClusterID]; ok {
				choice.rtt = rtt
			}
			choice.probe = it.probeRecord(winner.ProviderClusterID, probeResp.RTTs, probeResp.Duration)
		}
	}
	return choice, false
}

func hasCandidate(candidates []testlib.ProbeCandidate, providerID string) bool {
	for _, c := range candidates {
		if c.ProviderClusterID == providerID {
			return true
		}
	}
	return false
}

// record builds the selection row for an iteration that ended without a
// reservation attempt.
func (it latencyChoiceInput) record(outcome, message string) testlib.SelectionRecord {
	return testlib.SelectionRecord{
		Timestamp:         it.start,
		ConsumerID:        it.consID,
		Phase:             it.phase,
		Policy:            it.policy,
		Iteration:         it.iteration,
		Outcome:           outcome,
		ErrorMessage:      message,
		DurationMs:        msSince(it.start),
		InitialProviderID: it.initialProvider,
		RetryCount:        it.attempt,
	}
}

func (it latencyChoiceInput) probeRecord(chosen string, rtts map[string]float64, durationMs float64) *testlib.ProbeRecord {
	return &testlib.ProbeRecord{
		Timestamp:  it.start,
		ConsumerID: it.consID,
		Phase:      it.phase,
		Policy:     it.policy,
		Iteration:  it.iteration,
		Chosen:     chosen,
		RTTs:       rtts,
		DurationMs: durationMs,
	}
}

func runLatencyReserveConsumerIteration(ctx context.Context, orch *testlib.Orchestrator, clients *testlib.ExperimentClients, endpoints map[string]string, phase, policy string, i int, consID string, pollInterval time.Duration, exp testlib.TestParams, state *latencyReservePhaseState) {
	console := clients.Consoles[consID]
	broker := clients.BrokerFor(consID)
	start := time.Now()

	var prevProvider string
	var releaseMs float64
	var initialProvider string

	for attempt := 0; ; attempt++ {
		ngResp, err := broker.GetNodeGroups(ctx)
		if err != nil {
			state.addSelection(testlib.SelectionRecord{
				Timestamp:         start,
				ConsumerID:        consID,
				Phase:             phase,
				Policy:            policy,
				Iteration:         i,
				Outcome:           "error",
				ErrorMessage:      fmt.Sprintf("get nodegroups: %v", err),
				DurationMs:        msSince(start),
				InitialProviderID: initialProvider,
				RetryCount:        attempt,
			})
			log.Printf("[%s] iter %d %s: nodegroups error: %v", phase, i, consID, err)
			return
		}

		// Determine winner via probing (latency shortlist) or single winner.
		var chosenProviderID, chosenNodeGroupID string
		var chosenRTT float64
		var probeRec *testlib.ProbeRecord

		// Read the consumer's current reservation up front, BEFORE the
		// no-candidates / no-winner branches below. A fully booked federation
		// (every node group at MaxSize == CurrentReserved, so nothing is
		// growable) says nothing about the reservation this consumer already
		// holds — that one is still Peered and serving. Deciding "no winner ⇒
		// failure" without looking at it recorded a healthy consumer as a
		// failure on every iteration once capacity ran out.
		cur := state.getActive(consID)

		// keepNoAlternative records the iteration as a keep on the current
		// reservation because the Broker offered nothing to move to. Mirrors
		// the record pair built by the regular !shouldSwitch path below.
		keepNoAlternative := func(reason string) {
			state.addSelection(testlib.SelectionRecord{
				Timestamp:         start,
				ConsumerID:        consID,
				Phase:             phase,
				Policy:            policy,
				Iteration:         i,
				SelectedID:        cur.ProviderClusterID,
				NodeGroupID:       cur.NodeGroupID,
				ReservationID:     cur.ReservationID,
				Outcome:           testlib.OutcomeKeepNoAlternative,
				ErrorMessage:      reason,
				DurationMs:        msSince(start),
				InitialProviderID: initialProvider,
				RetryCount:        attempt,
			})
			state.addReservation(testlib.ReservationRecord{
				Timestamp:         start,
				ConsumerID:        consID,
				Phase:             phase,
				Policy:            policy,
				Iteration:         i,
				ReservationID:     cur.ReservationID,
				ProviderClusterID: cur.ProviderClusterID,
				NodeGroupID:       cur.NodeGroupID,
				Action:            "keep",
				FinalPhase:        "Peered",
				Outcome:           testlib.OutcomeKeepNoAlternative,
				ErrorMessage:      reason,
				TotalMs:           msSince(start),
				InitialProviderID: initialProvider,
				RetryCount:        attempt,
			})
			log.Printf("[%s] iter %02d %s: keep  %-20s (no alternative: %s)",
				phase, i, consID, cur.ProviderClusterID, reason)
		}

		it := latencyChoiceInput{
			ctx:               ctx,
			console:           console,
			state:             state,
			endpoints:         endpoints,
			phase:             phase,
			policy:            policy,
			consID:            consID,
			iteration:         i,
			attempt:           attempt,
			start:             start,
			initialProvider:   initialProvider,
			cur:               cur,
			keepNoAlternative: keepNoAlternative,
		}
		choice, recorded := chooseLatencyProvider(it, ngResp)
		if recorded {
			return
		}
		chosenProviderID = choice.providerID
		chosenNodeGroupID = choice.nodeGroupID
		chosenRTT = choice.rtt
		probeRec = choice.probe
		hadAlternatives := choice.hadAlternatives

		if initialProvider == "" && chosenProviderID != "" {
			initialProvider = chosenProviderID
		}

		// No candidate answered at all: chosenProviderID is empty, and sending
		// that to the Broker earns a 400 "providerClusterId is required".
		// Stay put if we can; otherwise end the iteration with an outcome that
		// says why rather than with a malformed request.
		if chosenProviderID == "" {
			if cur != nil {
				keepNoAlternative("no probe answered")
				return
			}
			state.addSelection(testlib.SelectionRecord{
				Timestamp:         start,
				ConsumerID:        consID,
				Phase:             phase,
				Policy:            policy,
				Iteration:         i,
				Outcome:           "no-winner",
				ErrorMessage:      "no probe answered",
				DurationMs:        msSince(start),
				InitialProviderID: initialProvider,
				RetryCount:        attempt,
			})
			log.Printf("[%s] iter %d %s: no probe answered", phase, i, consID)
			return
		}

		shouldSwitch := cur == nil
		if cur != nil && chosenProviderID != cur.ProviderClusterID {
			if ngResp.LatencyShortlist {
				// Switch only when the incumbent was actually measured and
				// lost: its endpoint was added to the probe candidates above
				// when reachable (see keepNoAlternative context), so an
				// unmeasured incumbent gives nothing to compare against.
				shouldSwitch = probeRec != nil
				if shouldSwitch {
					_, curProbed := probeRec.RTTs[cur.ProviderClusterID]
					shouldSwitch = curProbed
				}
			} else {
				// Single-winner branch (Random / Eco-without-metric / Price /
				// Standard): FindWinner already picked the one node group the
				// Broker left growable, so a different winner here means the
				// Broker genuinely wants a move. probeRec in this branch is
				// only the winner's own RTT for the CSV's rtt_ms column, not
				// a measurement of the incumbent — gating on it here meant
				// probeRec.RTTs[cur.ProviderClusterID] was never present
				// (only the winner was ever probed), so curProbed was always
				// false and Random could never switch even though the
				// Broker rerolled a genuinely different winner on every
				// call (visible as initial_provider_id != provider_id with
				// retry_count == 0 in reservations.csv). No "measured"
				// requirement applies here, matching testlib.ShouldSwitch's
				// no-metric case used by comparative-eco.
				shouldSwitch = true
			}
		}

		if !shouldSwitch {
			// Distinguish "stayed because it was the best" from "stayed
			// because the Broker offered nowhere to go".
			outcome := testlib.OutcomeSuccess
			reason := ""
			if !hadAlternatives {
				outcome = testlib.OutcomeKeepNoAlternative
				reason = "no growable alternative"
			}
			if probeRec != nil {
				state.addProbe(*probeRec)
			}
			state.addSelection(testlib.SelectionRecord{
				Timestamp:         start,
				ConsumerID:        consID,
				Phase:             phase,
				Policy:            policy,
				Iteration:         i,
				SelectedID:        cur.ProviderClusterID,
				NodeGroupID:       cur.NodeGroupID,
				ReservationID:     cur.ReservationID,
				RTTMs:             chosenRTT,
				Outcome:           outcome,
				ErrorMessage:      reason,
				DurationMs:        msSince(start),
				InitialProviderID: initialProvider,
				RetryCount:        attempt,
			})
			state.addReservation(testlib.ReservationRecord{
				Timestamp:         start,
				ConsumerID:        consID,
				Phase:             phase,
				Policy:            policy,
				Iteration:         i,
				ReservationID:     cur.ReservationID,
				ProviderClusterID: cur.ProviderClusterID,
				NodeGroupID:       cur.NodeGroupID,
				Action:            "keep",
				FinalPhase:        "Peered",
				RTTMs:             chosenRTT,
				Outcome:           outcome,
				ErrorMessage:      reason,
				TotalMs:           msSince(start),
				InitialProviderID: initialProvider,
				RetryCount:        attempt,
			})
			log.Printf("[%s] iter %02d %s: keep  %-20s (rtt=%.2fms)", phase, i, consID, cur.ProviderClusterID, chosenRTT)
			return
		}

		if probeRec != nil {
			state.addProbe(*probeRec)
		}

		if cur != nil && prevProvider == "" {
			prevProvider = cur.ProviderClusterID
			releaseStart := time.Now()
			releaseCtx, releaseCancel := context.WithTimeout(ctx, exp.ReservationTimeout)
			if err := broker.ReleaseAndWait(releaseCtx, cur.ReservationID, cur.Request, pollInterval); err != nil {
				log.Printf("[%s] iter %d %s: release error for %s: %v", phase, i, consID, cur.ReservationID, err)
			}
			releaseCancel()
			releaseMs = msSince(releaseStart)
			state.deleteActive(consID)
		}

		seq := state.nextSeq(consID)
		resID := testlib.MakeReservationID(orch.RunID, phase, consID, seq)

		if chosenNodeGroupID == "" {
			if ng := testlib.FindNodeGroupByProvider(ngResp.NodeGroups, chosenProviderID); ng != nil {
				chosenNodeGroupID = ng.ID
			}
		}

		req := &brokerapi.ReservationRequest{
			ProviderClusterID: chosenProviderID,
			NodeGroupID:       chosenNodeGroupID,
			ChunkCount:        1,
			ChunkType:         brokerv1alpha1.ChunkTypeStandard,
		}

		peerStart := time.Now()
		peerCtx, peerCancel := context.WithTimeout(ctx, exp.ReservationTimeout)
		resp, peerErr := broker.ReserveAndWait(peerCtx, resID, req, pollInterval)
		peerCancel()
		peerMs := msSince(peerStart)

		action := "create"
		if prevProvider != "" {
			action = "switch"
		}

		if peerErr != nil {
			switch {
			case agentclient.IsTooManyRequests(peerErr) && attempt < maxReserveRaceRetries:
				log.Printf("[%s] iter %d %s: rate limited on %s — backing off %s before retrying",
					phase, i, consID, chosenProviderID, raceRetryBackoff)
				if sleepErr := testlib.SleepCtx(ctx, raceRetryBackoff); sleepErr != nil {
					return
				}
				continue
			case (agentclient.IsConflict(peerErr) || agentclient.IsTransient(peerErr)) && attempt < maxReserveRaceRetries:
				log.Printf("[%s] iter %d %s: %s — retrying with next-best (%v)",
					phase, i, consID, chosenProviderID, peerErr)
				continue
			}

			finalPhase := ""
			if resp != nil {
				finalPhase = string(resp.Status)
			}
			state.addSelection(testlib.SelectionRecord{
				Timestamp:         start,
				ConsumerID:        consID,
				Phase:             phase,
				Policy:            policy,
				Iteration:         i,
				SelectedID:        chosenProviderID,
				NodeGroupID:       chosenNodeGroupID,
				ReservationID:     resID,
				RTTMs:             chosenRTT,
				Outcome:           "reserve-error",
				ErrorMessage:      peerErr.Error(),
				DurationMs:        msSince(start),
				InitialProviderID: initialProvider,
				RetryCount:        attempt,
			})
			state.addReservation(testlib.ReservationRecord{
				Timestamp:         start,
				ConsumerID:        consID,
				Phase:             phase,
				Policy:            policy,
				Iteration:         i,
				ReservationID:     resID,
				ProviderClusterID: chosenProviderID,
				NodeGroupID:       chosenNodeGroupID,
				Action:            action,
				PrevProviderID:    prevProvider,
				PeerMs:            peerMs,
				ReleaseMs:         releaseMs,
				TotalMs:           msSince(start),
				FinalPhase:        finalPhase,
				RTTMs:             chosenRTT,
				Outcome:           "error",
				ErrorMessage:      peerErr.Error(),
				InitialProviderID: initialProvider,
				RetryCount:        attempt,
			})
			log.Printf("[%s] iter %d %s: %s error → %s: %v", phase, i, consID, action, chosenProviderID, peerErr)
			return
		}

		state.setActive(consID, &testlib.ConsumerReservation{
			ReservationID:     resID,
			ProviderClusterID: chosenProviderID,
			NodeGroupID:       chosenNodeGroupID,
			Request:           req,
		})

		state.addSelection(testlib.SelectionRecord{
			Timestamp:         start,
			ConsumerID:        consID,
			Phase:             phase,
			Policy:            policy,
			Iteration:         i,
			SelectedID:        chosenProviderID,
			NodeGroupID:       chosenNodeGroupID,
			ReservationID:     resID,
			RTTMs:             chosenRTT,
			Outcome:           testlib.OutcomeSuccess,
			DurationMs:        msSince(start),
			InitialProviderID: initialProvider,
			RetryCount:        attempt,
		})
		state.addReservation(testlib.ReservationRecord{
			Timestamp:         start,
			ConsumerID:        consID,
			Phase:             phase,
			Policy:            policy,
			Iteration:         i,
			ReservationID:     resID,
			ProviderClusterID: chosenProviderID,
			NodeGroupID:       chosenNodeGroupID,
			Action:            action,
			PrevProviderID:    prevProvider,
			PeerMs:            peerMs,
			ReleaseMs:         releaseMs,
			TotalMs:           msSince(start),
			FinalPhase:        string(resp.Status),
			RTTMs:             chosenRTT,
			Outcome:           testlib.OutcomeSuccess,
			InitialProviderID: initialProvider,
			RetryCount:        attempt,
		})
		log.Printf("[%s] iter %02d %s: %s %-20s (res=%s peer=%.0fms rel=%.0fms rtt=%.2fms)",
			phase, i, consID, action, chosenProviderID, resID, peerMs, releaseMs, chosenRTT)
		return
	}
}

func summarizeLatencyPhase(selections []testlib.SelectionRecord, _ []testlib.ProbeRecord) testlib.PhaseSummary {
	s := testlib.PhaseSummary{
		Iterations:      len(selections),
		SelectionCounts: make(map[string]int),
	}
	var rttValues []float64
	for _, r := range selections {
		// A keep-no-alternative iteration ended with the consumer holding
		// working capacity, so it counts as a success here; selections.csv
		// keeps the two apart for anyone who needs the distinction.
		if r.Outcome == testlib.OutcomeSuccess || r.Outcome == testlib.OutcomeKeepNoAlternative {
			s.Successes++
			s.SelectionCounts[r.SelectedID]++
			if r.RTTMs > 0 && !math.IsInf(r.RTTMs, 1) {
				rttValues = append(rttValues, r.RTTMs)
			}
		} else {
			s.Failures++
		}
	}
	if len(rttValues) > 0 {
		sort.Float64s(rttValues)
		var sum float64
		for _, v := range rttValues {
			sum += v
		}
		s.MeanRTTMs = sum / float64(len(rttValues))
		s.MedianRTTMs = rttValues[len(rttValues)/2]
	}
	return s
}

func printSummary(s testlib.ExperimentSummary) {
	log.Println("─── Results ───")
	for _, ps := range []struct {
		label   string
		summary testlib.PhaseSummary
	}{
		{fmt.Sprintf("Phase A (%s)", s.PhaseAPolicy), s.PhaseASummary},
		{fmt.Sprintf("Phase B (%s)", s.PhaseBPolicy), s.PhaseBSummary},
	} {
		log.Printf("%s: %d success, %d fail", ps.label, ps.summary.Successes, ps.summary.Failures)
		for id, count := range ps.summary.SelectionCounts {
			pct := 100 * float64(count) / float64(max(ps.summary.Successes, 1))
			log.Printf("  %-20s %d selections (%.0f%%)", id, count, pct)
		}
		if ps.summary.MeanRTTMs > 0 {
			log.Printf("  mean RTT: %.2fms  median RTT: %.2fms", ps.summary.MeanRTTMs, ps.summary.MedianRTTMs)
		}
	}
}

// refreshLatency periodically redraws every simulated delay. rng is an
// explicit, caller-owned source (never the global math/rand) so that Phase A
// and Phase B can each be handed a freshly seeded rng with the same seed
// value (see runExperiment) and draw byte-for-byte identical sequences,
// unaffected by any unrelated goroutine in the process also consuming the
// shared global source between the two phases.
func refreshLatency(ctx context.Context, exp testlib.TestParams, consumerTCs []*testlib.TCConsumerDelay, providerTCs []*testlib.TCDelayKind, rng *rand.Rand) {
	interval := exp.LatencyRefreshInterval
	minMs, maxMs := exp.LatencyMinMs, exp.LatencyMaxMs
	log.Printf("[latency-refresh] started (interval=%s, delays redrawn in %d-%dms)", interval, minMs, maxMs)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[latency-refresh] stopped")
			return
		case <-ticker.C:
			// Redraw the whole matrix from scratch rather than nudging the
			// previous values, mirroring refreshCarbon in comparative-eco.
			// A random walk kept each consumer's ranking of providers almost
			// unchanged from one refresh to the next, so the nearest provider
			// never really moved and the Latency policy had no reason to
			// switch; an independent draw reshuffles the ranking every cycle.
			for _, tc := range consumerTCs {
				newDelays := make([]testlib.ProviderDelayEntry, len(tc.ProviderDelays))
				for i, pd := range tc.ProviderDelays {
					newDelays[i] = testlib.ProviderDelayEntry{
						ProviderIP: pd.ProviderIP,
						DelayMs:    testlib.RandomDelayMs(rng, minMs, maxMs),
						Label:      pd.Label,
					}
				}
				if err := tc.UpdateDelays(newDelays); err != nil {
					if ctx.Err() != nil {
						return
					}
					log.Printf("[latency-refresh] error updating %s: %v", tc.ContainerName, err)
					continue
				}
				testlib.LogProviderDelays("[latency-refresh]   "+tc.ContainerName, newDelays)
			}
			for _, tc := range providerTCs {
				newDelay := testlib.RandomDelayMs(rng, minMs, maxMs)
				if err := tc.UpdateDelay(newDelay); err != nil {
					if ctx.Err() != nil {
						return
					}
					log.Printf("[latency-refresh] error updating %s: %v", tc.ContainerName, err)
					continue
				}
				log.Printf("[latency-refresh]   %s: %dms", tc.ContainerName, newDelay)
			}
		}
	}
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}
