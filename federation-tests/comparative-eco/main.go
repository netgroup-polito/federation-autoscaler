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

// Command comparative-eco runs Phase A (Random) vs Phase B (Eco) with full
// automation: creates Kind clusters, deploys all components, runs the
// experiment, collects results, and cleans up.
//
// Usage:
//
//	go run ./federation-tests/comparative-eco/ --config federation-tests/configs/eco.yaml
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

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run holds the actual program body so orch.Teardown (deferred) always
// executes on any error path, including a Setup failure — log.Fatal in main
// calls os.Exit, which would otherwise skip deferred cleanup and leave Kind
// clusters orphaned (observed: a transient setup error left 11 running Kind
// clusters + a busy inotify budget behind).
func run() error {
	configPath, keepClusters, skipBuild, runID := parseFlags()

	cfg, err := testlib.LoadAutoConfig(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	orch, err := testlib.NewOrchestrator(cfg, "comparative-eco", keepClusters, skipBuild, runID)
	if err != nil {
		return fmt.Errorf("orchestrator: %w", err)
	}

	// Ensure cleanup runs on exit — now reachable on every return path below,
	// since none of them calls os.Exit directly.
	defer orch.Teardown(context.Background())

	if err := orch.Setup(ctx); err != nil {
		return fmt.Errorf("setup failed: %w", err)
	}

	if err := runExperiment(ctx, orch); err != nil {
		return fmt.Errorf("experiment failed: %w", err)
	}
	return nil
}

func runExperiment(ctx context.Context, orch *testlib.Orchestrator) error {
	startTime := time.Now()
	cfg := orch.Config
	exp := cfg.Experiment
	clients := orch.Clients

	// Build mock-eco client.
	mockEcoURL, err := orch.MockEcoURL(ctx)
	if err != nil {
		return fmt.Errorf("get mock-eco URL: %w", err)
	}
	mockEco := testlib.NewMockEcoClient(mockEcoURL)
	log.Printf("[setup] mock-eco at %s", mockEcoURL)

	// Clear stale policies.
	if err := clients.SetPolicyAll(ctx, "None", 0); err != nil {
		return fmt.Errorf("clear policies: %w", err)
	}

	var allRecords []testlib.SelectionRecord
	var allReservations []testlib.ReservationRecord
	var allSnapshots []testlib.NodeGroupSnapshotRecord
	var allFederation []testlib.FederationSampleRecord
	mode := exp.Mode

	// carbonSeed is reused, unchanged, to start BOTH phases' carbon refresh --
	// a fresh *rand.Rand seeded identically at the start of each phase draws
	// the exact same sequence of region assignments, so Phase B ("Eco")
	// replays Phase A's ("Random") carbon-intensity conditions tick for tick
	// instead of seeing its own independent random draw. This is what makes
	// the two phases a genuine paired comparison (same environment, only the
	// placement policy differs) rather than two independent samples from the
	// same distribution.
	carbonSeed := testlib.SeedFromString(orch.RunID)

	// One settle budget, computed once and spent identically by both phases.
	// It used to be PolicyPropagationWait for Phase A and
	// max(AdvertisementLag, PolicyPropagationWait) for Phase B, which happened
	// to agree only because the config sets both knobs to the same value --
	// raise advertisementLag alone and Phase B would silently get a longer
	// runway than Phase A, breaking the paired comparison in a way nothing
	// would report.
	policySettle := exp.AdvertisementLag
	if exp.PolicyPropagationWait > policySettle {
		policySettle = exp.PolicyPropagationWait
	}

	// The federation Phase A is about to run on. Phase B has to start on this
	// same one to be a paired comparison, and the transition below checks that
	// it does. Measured rather than assumed to be empty: a deploy that came up
	// with something already reserved is a different starting point, not a
	// broken one, and either way the two phases only have to agree with
	// each other.
	baseline, err := testlib.ReadFederationCapacity(ctx, clients.Broker)
	if err != nil {
		return fmt.Errorf("baseline federation capacity: %w", err)
	}
	log.Printf("federation baseline: %d node groups, %d chunks reserved",
		len(baseline.NodeGroupIDs), baseline.TotalReserved)

	// --- Phase A: Random ---
	log.Println("=== PHASE A: Random ===")
	if err := clients.SetPolicyAll(ctx, "Random", policySettle); err != nil {
		return fmt.Errorf("set Random: %w", err)
	}

	carbonRefreshCtxA, cancelCarbonRefreshA := context.WithCancel(ctx)
	var carbonWGA sync.WaitGroup
	carbonWGA.Add(1)
	go func() {
		defer carbonWGA.Done()
		refreshCarbon(carbonRefreshCtxA, mockEco, cfg, exp, rand.New(rand.NewSource(carbonSeed)))
	}()

	// Both phases start their refresh goroutine at the same point in their own
	// sequence and then wait the same CarbonRefreshInterval before sampling, so
	// a sample taken at the same elapsed time lands on the same tick of the
	// replayed sequence in both. Expressing the gap as the same value on both
	// sides is the point: it used to be PolicyPropagationWait here and
	// CarbonRefreshInterval there, which only lined up because the config
	// happens to set both to 35s.
	if err := testlib.SleepCtx(ctx, exp.CarbonRefreshInterval); err != nil {
		cancelCarbonRefreshA()
		carbonWGA.Wait()
		return err
	}

	var phaseARecords []testlib.SelectionRecord
	if mode == testlib.ModeReserve {
		var phaseARes []testlib.ReservationRecord
		var phaseASnaps []testlib.NodeGroupSnapshotRecord
		var phaseAFed []testlib.FederationSampleRecord
		phaseARecords, phaseARes, phaseASnaps, phaseAFed, err = runReservePhase(ctx, orch, clients, testlib.PhaseA, "Random")
		allReservations = append(allReservations, phaseARes...)
		allSnapshots = append(allSnapshots, phaseASnaps...)
		allFederation = append(allFederation, phaseAFed...)
	} else {
		var phaseASnaps []testlib.NodeGroupSnapshotRecord
		phaseARecords, phaseASnaps, err = runObservePhase(ctx, orch, clients, testlib.PhaseA, "Random")
		allSnapshots = append(allSnapshots, phaseASnaps...)
	}
	cancelCarbonRefreshA()
	carbonWGA.Wait()
	if err != nil {
		return fmt.Errorf("phase A: %w", err)
	}
	allRecords = append(allRecords, phaseARecords...)
	log.Printf("phase A complete: %d samples", len(phaseARecords))

	// --- Transition: switch to Eco policy ---
	log.Println("=== TRANSITION ===")

	// Confirm Phase A gave the federation back before Phase B takes it. This
	// has to happen HERE -- before the policy switch and before Phase B's
	// refresh goroutine starts -- because it can block for up to
	// testlib.FederationSettleTimeout, and any variable-length wait placed after the
	// goroutine would shift Phase B's tick alignment relative to Phase A's and
	// undo the replay the rest of this function exists to preserve.
	log.Println("  checking the federation is back to its starting capacity...")
	if _, err := testlib.WaitForFederationCapacity(ctx, clients.Broker, baseline, exp.ReservationPoll, testlib.FederationSettleTimeout); err != nil {
		return fmt.Errorf("phase A leaked capacity, so phase B would not run on the same federation: %w", err)
	}

	// Switch to Eco (carbon values are already being refreshed by the goroutine).
	// Same policySettle Phase A spent, for the same reason.
	log.Printf("  waiting %s for policy + advertisement propagation...", policySettle)
	if err := clients.SetPolicyAll(ctx, "Eco", policySettle); err != nil {
		return fmt.Errorf("set Eco: %w", err)
	}

	// --- Phase B: Eco ---
	log.Println("=== PHASE B: Eco ===")
	// Freshly seeded with the SAME carbonSeed as Phase A above: replays Phase
	// A's exact carbon-intensity sequence instead of drawing an independent
	// one, so the two phases are directly comparable tick for tick.
	carbonRefreshCtxB, cancelCarbonRefreshB := context.WithCancel(ctx)
	var carbonWGB sync.WaitGroup
	carbonWGB.Add(1)
	go func() {
		defer carbonWGB.Done()
		refreshCarbon(carbonRefreshCtxB, mockEco, cfg, exp, rand.New(rand.NewSource(carbonSeed)))
	}()

	// Let provider-side eco-client caches (TTL = CarbonRefreshInterval) catch
	// up before sampling starts: a provider whose cache was still fresh from
	// Phase A's last tick would otherwise keep advertising that stale value
	// for the first sampled iteration or two of Phase B, even though the
	// goroutine above has already posted Phase B's replayed values.
	if err := testlib.SleepCtx(ctx, exp.CarbonRefreshInterval); err != nil {
		cancelCarbonRefreshB()
		carbonWGB.Wait()
		return err
	}

	var phaseBRecords []testlib.SelectionRecord
	if mode == testlib.ModeReserve {
		var phaseBRes []testlib.ReservationRecord
		var phaseBSnaps []testlib.NodeGroupSnapshotRecord
		var phaseBFed []testlib.FederationSampleRecord
		phaseBRecords, phaseBRes, phaseBSnaps, phaseBFed, err = runReservePhase(ctx, orch, clients, testlib.PhaseB, "Eco")
		allReservations = append(allReservations, phaseBRes...)
		allSnapshots = append(allSnapshots, phaseBSnaps...)
		allFederation = append(allFederation, phaseBFed...)
	} else {
		var phaseBSnaps []testlib.NodeGroupSnapshotRecord
		phaseBRecords, phaseBSnaps, err = runObservePhase(ctx, orch, clients, testlib.PhaseB, "Eco")
		allSnapshots = append(allSnapshots, phaseBSnaps...)
	}
	cancelCarbonRefreshB()
	carbonWGB.Wait()
	if err != nil {
		return fmt.Errorf("phase B: %w", err)
	}
	allRecords = append(allRecords, phaseBRecords...)
	log.Printf("phase B complete: %d samples", len(phaseBRecords))

	// --- Cleanup policies ---
	log.Println("=== EXPERIMENT CLEANUP ===")
	if err := clients.SetPolicyAll(ctx, "None", 0); err != nil {
		log.Printf("cleanup: clear policies: %v", err)
	}
	if err := mockEco.ResetCarbon(ctx); err != nil {
		log.Printf("cleanup: reset mock-eco: %v", err)
	}

	// --- Write results ---
	outputDir := orch.OutputDir
	log.Printf("writing results to %s", outputDir)
	if err := testlib.WriteSelectionCSV(outputDir, "selections.csv", allRecords); err != nil {
		return fmt.Errorf("write CSV: %w", err)
	}
	if mode == testlib.ModeReserve && len(allReservations) > 0 {
		if err := testlib.WriteReservationCSV(outputDir, "reservations.csv", allReservations); err != nil {
			return fmt.Errorf("write reservations CSV: %w", err)
		}
	}
	if len(allSnapshots) > 0 {
		if err := testlib.WriteNodeGroupCSV(outputDir, "nodegroups.csv", allSnapshots); err != nil {
			return fmt.Errorf("write nodegroups CSV: %w", err)
		}
	}
	if len(allFederation) > 0 {
		if err := testlib.WriteFederationCSV(outputDir, "federation.csv", allFederation); err != nil {
			return fmt.Errorf("write federation CSV: %w", err)
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
		TestType:           "comparative-eco",
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
		PhaseBPolicy:       "Eco",
		PhaseASummary:      summarizePhase(phaseARecords),
		PhaseBSummary:      summarizePhase(phaseBRecords),
	}
	if err := testlib.WriteJSONFile(outputDir, "summary.json", summary); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	if err := testlib.WriteSummaryMarkdown(outputDir, summary); err != nil {
		return fmt.Errorf("write markdown: %w", err)
	}

	log.Println("=== DONE ===")
	printSummary(summary)
	return nil
}

// runObservePhase drives one consumer-poll cycle per iteration. Consumers are
// dispatched concurrently (one goroutine each) within an iteration, mirroring
// how independent real Consumer Agents behave in production — each polls the
// Broker on its own, none waits in line behind another. All consumers in an
// iteration are joined (awaited) before the next iteration's phasePause, so
// the iteration cadence itself is unchanged; only the per-consumer work
// inside each iteration is now parallel instead of serial.
func runObservePhase(ctx context.Context, orch *testlib.Orchestrator, clients *testlib.ExperimentClients, phase, policy string) ([]testlib.SelectionRecord, []testlib.NodeGroupSnapshotRecord, error) {
	exp := orch.Config.Experiment
	var mu sync.Mutex
	var records []testlib.SelectionRecord
	var snapshots []testlib.NodeGroupSnapshotRecord

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
					runObserveConsumerIteration(ctx, clients, phase, policy, i, consID, &mu, &records, &snapshots)
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
					runObserveConsumerIteration(ctx, clients, phase, policy, i, consID, &mu, &records, &snapshots)
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

	return records, snapshots, loopErr
}

// runObserveConsumerIteration is the per-consumer, per-iteration body of
// runObservePhase, safe to run concurrently with other consumers' calls: it
// only touches shared state (records, snapshots) behind mu, and everything
// else is function-local.
func runObserveConsumerIteration(ctx context.Context, clients *testlib.ExperimentClients, phase, policy string, i int, consID string, mu *sync.Mutex, records *[]testlib.SelectionRecord, snapshots *[]testlib.NodeGroupSnapshotRecord) {
	start := time.Now()
	broker := clients.BrokerFor(consID)

	ngResp, err := broker.GetNodeGroups(ctx)
	if err != nil {
		mu.Lock()
		*records = append(*records, testlib.SelectionRecord{
			Timestamp:    start,
			ConsumerID:   consID,
			Phase:        phase,
			Policy:       policy,
			Iteration:    i,
			Outcome:      "error",
			ErrorMessage: fmt.Sprintf("get nodegroups: %v", err),
			DurationMs:   msSince(start),
		})
		mu.Unlock()
		log.Printf("[%s] iter %d %s: nodegroups error: %v", phase, i, consID, err)
		return
	}

	winner := testlib.FindWinner(ngResp.NodeGroups)

	snaps := make([]testlib.NodeGroupSnapshotRecord, 0, len(ngResp.NodeGroups))
	for _, ng := range ngResp.NodeGroups {
		var carbon float64
		var hasCarbon bool
		if ng.CarbonIntensity != nil {
			carbon = *ng.CarbonIntensity
			hasCarbon = true
		}
		snaps = append(snaps, testlib.NodeGroupSnapshotRecord{
			Timestamp:         start,
			ConsumerID:        consID,
			Phase:             phase,
			Policy:            policy,
			Iteration:         i,
			ProviderClusterID: ng.ProviderClusterID,
			NodeGroupID:       ng.ID,
			PlacementMetric:   ng.PlacementMetric,
			HasMetric:         ng.HasMetric,
			CarbonIntensity:   carbon,
			HasCarbon:         hasCarbon,
			CurrentReserved:   ng.CurrentReserved,
			MaxSize:           ng.MaxSize,
			AppliedPlacement:  string(ngResp.AppliedPlacement),
			IsSelected:        winner != nil && ng.ID == winner.ID,
		})
	}
	mu.Lock()
	*snapshots = append(*snapshots, snaps...)
	mu.Unlock()

	if winner == nil {
		growable := testlib.GrowableNodeGroups(ngResp.NodeGroups)
		mu.Lock()
		*records = append(*records, testlib.SelectionRecord{
			Timestamp:    start,
			ConsumerID:   consID,
			Phase:        phase,
			Policy:       policy,
			Iteration:    i,
			Outcome:      "no-winner",
			ErrorMessage: fmt.Sprintf("growable=%d applied=%s", len(growable), ngResp.AppliedPlacement),
			DurationMs:   msSince(start),
		})
		mu.Unlock()
		log.Printf("[%s] iter %d %s: no single winner (growable=%d applied=%s)",
			phase, i, consID, len(growable), ngResp.AppliedPlacement)
		return
	}

	rec := testlib.SelectionRecord{
		Timestamp:      start,
		ConsumerID:     consID,
		Phase:          phase,
		Policy:         policy,
		Iteration:      i,
		SelectedID:     winner.ProviderClusterID,
		NodeGroupID:    winner.ID,
		PlacementValue: winner.PlacementMetric,
		HasMetric:      winner.HasMetric,
		Outcome:        testlib.OutcomeSuccess,
		DurationMs:     msSince(start),
	}
	mu.Lock()
	*records = append(*records, rec)
	mu.Unlock()

	log.Printf("[%s] iter %02d %s: winner=%-20s metric=%8.2f applied=%s",
		phase, i, consID, winner.ProviderClusterID, winner.PlacementMetric, ngResp.AppliedPlacement)
}

// reservePhaseState is the shared, mutex-protected state one runReservePhase
// call accumulates across all consumers and iterations. Every consumer's own
// entries in active/seqNo are only ever touched by that consumer's own
// goroutine, but Go maps are not safe for concurrent access from multiple
// goroutines even across disjoint keys, so every access — reads included —
// goes through mu.
type reservePhaseState struct {
	mu                sync.Mutex
	selections        []testlib.SelectionRecord
	reservations      []testlib.ReservationRecord
	snapshots         []testlib.NodeGroupSnapshotRecord
	federationSamples []testlib.FederationSampleRecord
	active            map[string]*testlib.ConsumerReservation
	seqNo             map[string]int
}

func newReservePhaseState() *reservePhaseState {
	return &reservePhaseState{
		active: make(map[string]*testlib.ConsumerReservation),
		seqNo:  make(map[string]int),
	}
}

func (s *reservePhaseState) addSnapshots(snaps []testlib.NodeGroupSnapshotRecord) {
	s.mu.Lock()
	s.snapshots = append(s.snapshots, snaps...)
	s.mu.Unlock()
}

func (s *reservePhaseState) addFederationSamples(samples []testlib.FederationSampleRecord) {
	s.mu.Lock()
	s.federationSamples = append(s.federationSamples, samples...)
	s.mu.Unlock()
}

func (s *reservePhaseState) addSelection(sel testlib.SelectionRecord) {
	s.mu.Lock()
	s.selections = append(s.selections, sel)
	s.mu.Unlock()
}

func (s *reservePhaseState) addReservation(res testlib.ReservationRecord) {
	s.mu.Lock()
	s.reservations = append(s.reservations, res)
	s.mu.Unlock()
}

func (s *reservePhaseState) getActive(consID string) *testlib.ConsumerReservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[consID]
}

func (s *reservePhaseState) setActive(consID string, res *testlib.ConsumerReservation) {
	s.mu.Lock()
	s.active[consID] = res
	s.mu.Unlock()
}

func (s *reservePhaseState) deleteActive(consID string) {
	s.mu.Lock()
	delete(s.active, consID)
	s.mu.Unlock()
}

func (s *reservePhaseState) nextSeq(consID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := s.seqNo[consID]
	s.seqNo[consID] = seq + 1
	return seq
}

// runReservePhase drives one reserve/keep/switch cycle per iteration, plus a
// background federation-wide sampler for the whole phase. exp.Duration picks
// how long the phase runs: runReservePhaseCounted (exp.Iterations
// synchronized rounds, consumers dispatched concurrently within each round
// and joined before the next round's phasePause — unchanged default
// behavior) or runReservePhaseTimed (exp.Timer wall-clock duration, each
// consumer looping independently on its own counter).
func runReservePhase(ctx context.Context, orch *testlib.Orchestrator, clients *testlib.ExperimentClients, phase, policy string) ([]testlib.SelectionRecord, []testlib.ReservationRecord, []testlib.NodeGroupSnapshotRecord, []testlib.FederationSampleRecord, error) {
	exp := orch.Config.Experiment
	pollInterval := exp.ReservationPoll

	consumerIDs := make([]string, 0, len(clients.Consoles))
	for id := range clients.Consoles {
		consumerIDs = append(consumerIDs, id)
	}
	sort.Strings(consumerIDs)

	state := newReservePhaseState()

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
	// stands right now, not just what changed. Scoped to this phase's state
	// (and so to its own active-reservation tracking) the same way the
	// per-iteration work already is.
	sampleCtx, cancelSample := context.WithCancel(ctx)
	var sampleWG sync.WaitGroup
	sampleWG.Add(1)
	go func() {
		defer sampleWG.Done()
		runFederationSampler(sampleCtx, exp.FederationSampleInterval, func() {
			state.addFederationSamples(sampleFederationEco(ctx, clients, consumerIDs, phase, policy, state))
		})
	}()

	var loopErr error
	if exp.IsTimeBased() {
		loopErr = runReservePhaseTimed(ctx, orch, clients, phase, policy, pollInterval, exp, state, consumerIDs)
	} else {
		loopErr = runReservePhaseCounted(ctx, orch, clients, phase, policy, pollInterval, exp, state, consumerIDs)
	}

	cancelSample()
	sampleWG.Wait()

	return state.selections, state.reservations, state.snapshots, state.federationSamples, loopErr
}

// runReservePhaseCounted is the original, unchanged loop: exp.Iterations
// synchronized rounds, every consumer dispatched concurrently within each
// round and joined before the next round's phasePause.
func runReservePhaseCounted(ctx context.Context, orch *testlib.Orchestrator, clients *testlib.ExperimentClients, phase, policy string, pollInterval time.Duration, exp testlib.TestParams, state *reservePhaseState, consumerIDs []string) error {
	for i := 1; i <= exp.Iterations; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var wg sync.WaitGroup
		for _, consID := range consumerIDs {
			wg.Add(1)
			go func(consID string) {
				defer wg.Done()
				runReserveConsumerIteration(ctx, orch, clients, phase, policy, i, consID, pollInterval, exp, state)
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
func runReservePhaseTimed(ctx context.Context, orch *testlib.Orchestrator, clients *testlib.ExperimentClients, phase, policy string, pollInterval time.Duration, exp testlib.TestParams, state *reservePhaseState, consumerIDs []string) error {
	deadline := time.Now().Add(exp.Timer)
	var wg sync.WaitGroup
	for _, consID := range consumerIDs {
		wg.Add(1)
		go func(consID string) {
			defer wg.Done()
			for i := 1; ctx.Err() == nil && time.Now().Before(deadline); i++ {
				runReserveConsumerIteration(ctx, orch, clients, phase, policy, i, consID, pollInterval, exp, state)
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

// sampleFederationEco snapshots every consumer's currently-held provider and
// its live carbon intensity. Carbon intensity is a per-provider value the
// Broker's policy masking never hides (visible on every row of
// nodegroups.csv regardless of is_selected), so one GetNodeGroups call
// (any consumer's client works) builds the provider→carbon map the whole
// tick reuses, instead of one call per consumer.
func sampleFederationEco(ctx context.Context, clients *testlib.ExperimentClients, consumerIDs []string, phase, policy string, state *reservePhaseState) []testlib.FederationSampleRecord {
	if len(consumerIDs) == 0 {
		return nil
	}
	ts := time.Now()
	carbon := map[string]float64{}
	if ngResp, err := clients.BrokerFor(consumerIDs[0]).GetNodeGroups(ctx); err == nil {
		for _, ng := range ngResp.NodeGroups {
			if ng.CarbonIntensity != nil {
				carbon[ng.ProviderClusterID] = *ng.CarbonIntensity
			}
		}
	}
	samples := make([]testlib.FederationSampleRecord, 0, len(consumerIDs))
	for _, consID := range consumerIDs {
		rec := testlib.FederationSampleRecord{
			Timestamp: ts, Phase: phase, Policy: policy, ConsumerID: consID,
			MetricType: "carbon_intensity",
		}
		if res := state.getActive(consID); res != nil {
			rec.ProviderClusterID = res.ProviderClusterID
			rec.ReservationID = res.ReservationID
			if v, ok := carbon[res.ProviderClusterID]; ok {
				rec.MetricValue, rec.HasMetric = v, true
			}
		}
		samples = append(samples, rec)
	}
	return samples
}

// maxReserveRaceRetries bounds how many times a consumer re-picks after
// losing a race for the last chunk on its computed winner: two consumers
// both read the same "provider X has room" snapshot, only one
// PostReservation actually lands, and the loser's view is now stale. A
// fresh GetNodeGroups + FindWinner naturally exposes the next-best
// candidate — the Broker only ever leaves the cheapest/greenest provider
// WITH HEADROOM unmasked — so retrying mirrors what the real,
// continuously-reconciling ResourceRequest controller / Cluster Autoscaler
// would do on its next tick, just resolved within this iteration instead of
// waiting out a real requeue delay.
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

// nodeGroupSnapshots builds one record per node group in resp, flagging
// winner (nil if none) as the selected one.
func nodeGroupSnapshots(resp *brokerapi.NodeGroupListResponse, ts time.Time, consID, phase, policy string, i int, winner *brokerapi.NodeGroupView) []testlib.NodeGroupSnapshotRecord {
	snaps := make([]testlib.NodeGroupSnapshotRecord, 0, len(resp.NodeGroups))
	for _, ng := range resp.NodeGroups {
		var carbon float64
		var hasCarbon bool
		if ng.CarbonIntensity != nil {
			carbon = *ng.CarbonIntensity
			hasCarbon = true
		}
		snaps = append(snaps, testlib.NodeGroupSnapshotRecord{
			Timestamp:         ts,
			ConsumerID:        consID,
			Phase:             phase,
			Policy:            policy,
			Iteration:         i,
			ProviderClusterID: ng.ProviderClusterID,
			NodeGroupID:       ng.ID,
			PlacementMetric:   ng.PlacementMetric,
			HasMetric:         ng.HasMetric,
			CarbonIntensity:   carbon,
			HasCarbon:         hasCarbon,
			CurrentReserved:   ng.CurrentReserved,
			MaxSize:           ng.MaxSize,
			AppliedPlacement:  string(resp.AppliedPlacement),
			IsSelected:        winner != nil && ng.ID == winner.ID,
		})
	}
	return snaps
}

// runReserveConsumerIteration is the per-consumer, per-iteration body of
// runReservePhase, safe to run concurrently with other consumers' calls:
// every access to shared state goes through state's own locking. Records
// exactly one selection/reservation row per (consumer, iteration) — even
// across capacity-race retries — on whichever attempt actually terminates.
func runReserveConsumerIteration(ctx context.Context, orch *testlib.Orchestrator, clients *testlib.ExperimentClients, phase, policy string, i int, consID string, pollInterval time.Duration, exp testlib.TestParams, state *reservePhaseState) {
	start := time.Now()
	broker := clients.BrokerFor(consID)

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

		winner := testlib.FindWinner(ngResp.NodeGroups)
		if winner != nil && initialProvider == "" {
			initialProvider = winner.ProviderClusterID
		}
		var winnerCarbon float64
		var winnerHasCarbon bool
		if winner != nil && winner.CarbonIntensity != nil {
			winnerCarbon = *winner.CarbonIntensity
			winnerHasCarbon = true
		}

		// Read the current reservation BEFORE the no-winner branch: a fully
		// booked federation (nothing growable) says nothing about the
		// reservation this consumer already holds, which is still Peered and
		// serving. Treating "no winner" as a failure without looking at it
		// recorded a healthy consumer as failing on every iteration once
		// capacity ran out.
		cur := state.getActive(consID)

		if winner == nil {
			growable := testlib.GrowableNodeGroups(ngResp.NodeGroups)
			state.addSnapshots(nodeGroupSnapshots(ngResp, start, consID, phase, policy, i, nil))
			if cur != nil {
				// Stayed put because there was nothing to move to — a success,
				// but tagged so analysis can tell it apart from a keep that
				// won on merit.
				reason := fmt.Sprintf("growable=%d", len(growable))
				var curCarbon float64
				var curHasCarbon bool
				if curNG := testlib.FindNodeGroupByProvider(ngResp.NodeGroups, cur.ProviderClusterID); curNG != nil && curNG.CarbonIntensity != nil {
					curCarbon = *curNG.CarbonIntensity
					curHasCarbon = true
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
					CarbonIntensity:   curCarbon,
					HasCarbon:         curHasCarbon,
					Outcome:           testlib.OutcomeKeepNoAlternative,
					ErrorMessage:      reason,
					TotalMs:           msSince(start),
					InitialProviderID: initialProvider,
					RetryCount:        attempt,
				})
				log.Printf("[%s] iter %02d %s: keep  %-20s (no alternative: %s)",
					phase, i, consID, cur.ProviderClusterID, reason)
				return
			}
			state.addSelection(testlib.SelectionRecord{
				Timestamp:         start,
				ConsumerID:        consID,
				Phase:             phase,
				Policy:            policy,
				Iteration:         i,
				Outcome:           "no-winner",
				ErrorMessage:      fmt.Sprintf("growable=%d applied=%s", len(growable), ngResp.AppliedPlacement),
				DurationMs:        msSince(start),
				InitialProviderID: initialProvider,
				RetryCount:        attempt,
			})
			log.Printf("[%s] iter %d %s: no single winner (growable=%d)", phase, i, consID, len(growable))
			return
		}

		if !testlib.ShouldSwitch(cur, winner.ProviderClusterID, ngResp.NodeGroups) {
			state.addSnapshots(nodeGroupSnapshots(ngResp, start, consID, phase, policy, i, winner))
			curNG := testlib.FindNodeGroupByProvider(ngResp.NodeGroups, cur.ProviderClusterID)
			var curMetric float64
			var curHasMetric bool
			var curCarbon float64
			var curHasCarbon bool
			if curNG != nil {
				curMetric = curNG.PlacementMetric
				curHasMetric = curNG.HasMetric
				if curNG.CarbonIntensity != nil {
					curCarbon = *curNG.CarbonIntensity
					curHasCarbon = true
				}
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
				PlacementValue:    curMetric,
				HasMetric:         curHasMetric,
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
				ReservationID:     cur.ReservationID,
				ProviderClusterID: cur.ProviderClusterID,
				NodeGroupID:       cur.NodeGroupID,
				Action:            "keep",
				FinalPhase:        "Peered",
				PlacementMetric:   curMetric,
				CarbonIntensity:   curCarbon,
				HasCarbon:         curHasCarbon,
				Outcome:           testlib.OutcomeSuccess,
				TotalMs:           msSince(start),
				InitialProviderID: initialProvider,
				RetryCount:        attempt,
			})
			log.Printf("[%s] iter %02d %s: keep  %-20s (metric=%.2f)", phase, i, consID, cur.ProviderClusterID, curMetric)
			return
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

		req := &brokerapi.ReservationRequest{
			ProviderClusterID: winner.ProviderClusterID,
			NodeGroupID:       winner.ID,
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
					phase, i, consID, winner.ProviderClusterID, raceRetryBackoff)
				if sleepErr := testlib.SleepCtx(ctx, raceRetryBackoff); sleepErr != nil {
					return
				}
				continue
			case (agentclient.IsConflict(peerErr) || agentclient.IsTransient(peerErr)) && attempt < maxReserveRaceRetries:
				log.Printf("[%s] iter %d %s: %s — retrying with next-best (%v)",
					phase, i, consID, winner.ProviderClusterID, peerErr)
				continue
			}

			state.addSnapshots(nodeGroupSnapshots(ngResp, start, consID, phase, policy, i, winner))
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
				SelectedID:        winner.ProviderClusterID,
				NodeGroupID:       winner.ID,
				ReservationID:     resID,
				PlacementValue:    winner.PlacementMetric,
				HasMetric:         winner.HasMetric,
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
				ProviderClusterID: winner.ProviderClusterID,
				NodeGroupID:       winner.ID,
				Action:            action,
				PrevProviderID:    prevProvider,
				PeerMs:            peerMs,
				ReleaseMs:         releaseMs,
				TotalMs:           msSince(start),
				FinalPhase:        finalPhase,
				PlacementMetric:   winner.PlacementMetric,
				CarbonIntensity:   winnerCarbon,
				HasCarbon:         winnerHasCarbon,
				Outcome:           "error",
				ErrorMessage:      peerErr.Error(),
				InitialProviderID: initialProvider,
				RetryCount:        attempt,
			})
			log.Printf("[%s] iter %d %s: %s error → %s: %v", phase, i, consID, action, winner.ProviderClusterID, peerErr)
			return
		}

		state.addSnapshots(nodeGroupSnapshots(ngResp, start, consID, phase, policy, i, winner))
		state.setActive(consID, &testlib.ConsumerReservation{
			ReservationID:     resID,
			ProviderClusterID: winner.ProviderClusterID,
			NodeGroupID:       winner.ID,
			Request:           req,
		})

		state.addSelection(testlib.SelectionRecord{
			Timestamp:         start,
			ConsumerID:        consID,
			Phase:             phase,
			Policy:            policy,
			Iteration:         i,
			SelectedID:        winner.ProviderClusterID,
			NodeGroupID:       winner.ID,
			ReservationID:     resID,
			PlacementValue:    winner.PlacementMetric,
			HasMetric:         winner.HasMetric,
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
			ProviderClusterID: winner.ProviderClusterID,
			NodeGroupID:       winner.ID,
			Action:            action,
			PrevProviderID:    prevProvider,
			PeerMs:            peerMs,
			ReleaseMs:         releaseMs,
			TotalMs:           msSince(start),
			FinalPhase:        string(resp.Status),
			PlacementMetric:   winner.PlacementMetric,
			CarbonIntensity:   winnerCarbon,
			HasCarbon:         winnerHasCarbon,
			Outcome:           testlib.OutcomeSuccess,
			InitialProviderID: initialProvider,
			RetryCount:        attempt,
		})
		log.Printf("[%s] iter %02d %s: %s %-20s (res=%s peer=%.0fms rel=%.0fms metric=%.2f)",
			phase, i, consID, action, winner.ProviderClusterID, resID, peerMs, releaseMs, winner.PlacementMetric)
		return
	}
}

func summarizePhase(records []testlib.SelectionRecord) testlib.PhaseSummary {
	s := testlib.PhaseSummary{
		Iterations:      len(records),
		SelectionCounts: make(map[string]int),
	}
	var metricSum float64
	var metricCount int
	for _, r := range records {
		// A keep-no-alternative iteration ended with the consumer holding
		// working capacity, so it counts as a success here; selections.csv
		// keeps the two apart for anyone who needs the distinction.
		if r.Outcome == testlib.OutcomeSuccess || r.Outcome == testlib.OutcomeKeepNoAlternative {
			s.Successes++
			s.SelectionCounts[r.SelectedID]++
			if r.HasMetric {
				metricSum += r.PlacementValue
				metricCount++
			}
		} else {
			s.Failures++
		}
	}
	if metricCount > 0 {
		s.MeanPlacementVal = metricSum / float64(metricCount)
	}
	return s
}

func printSummary(s testlib.ExperimentSummary) {
	log.Println("─── Results ───")
	log.Printf("Phase A (%s): %d success, %d fail", s.PhaseAPolicy, s.PhaseASummary.Successes, s.PhaseASummary.Failures)
	for id, count := range s.PhaseASummary.SelectionCounts {
		pct := 100 * float64(count) / float64(max(s.PhaseASummary.Successes, 1))
		log.Printf("  %-20s %d selections (%.0f%%)", id, count, pct)
	}
	log.Printf("Phase B (%s): %d success, %d fail", s.PhaseBPolicy, s.PhaseBSummary.Successes, s.PhaseBSummary.Failures)
	for id, count := range s.PhaseBSummary.SelectionCounts {
		pct := 100 * float64(count) / float64(max(s.PhaseBSummary.Successes, 1))
		log.Printf("  %-20s %d selections (%.0f%%)", id, count, pct)
	}
	if s.PhaseBSummary.MeanPlacementVal > 0 {
		log.Printf("  mean eco metric: %.4f", s.PhaseBSummary.MeanPlacementVal)
	}
}

// refreshCarbon periodically reassigns carbon intensity per region. rng is an
// explicit, caller-owned source (never the global math/rand) so that Phase A
// and Phase B can each be handed a freshly seeded rng with the same seed
// value (see runExperiment) and draw byte-for-byte identical sequences,
// unaffected by any unrelated goroutine in the process also consuming the
// shared global source between the two phases.
func refreshCarbon(ctx context.Context, mockEco *testlib.MockEcoClient, cfg *testlib.AutoConfig, exp testlib.TestParams, rng *rand.Rand) {
	interval := exp.CarbonRefreshInterval
	log.Printf("[carbon-refresh] started (interval=%s, green fraction=%.0f%%–%.0f%%, low=%d high=%d)",
		interval, exp.CarbonGreenFractionMin*100, exp.CarbonGreenFractionMax*100,
		exp.CarbonLow, exp.CarbonHigh)

	assignCarbon := func() {
		unique := uniqueRegions(cfg.ProviderRegions)
		n := len(unique)
		if n == 0 {
			return
		}

		frac := exp.CarbonGreenFractionMin +
			rng.Float64()*(exp.CarbonGreenFractionMax-exp.CarbonGreenFractionMin)
		greenCount := int(math.Round(frac * float64(n)))
		if greenCount < 1 {
			greenCount = 1
		}
		if greenCount > n-1 {
			greenCount = n - 1
		}

		perm := rng.Perm(n)
		greenSet := make(map[int]bool, greenCount)
		for i := 0; i < greenCount; i++ {
			greenSet[perm[i]] = true
		}

		for i, region := range unique {
			var carbon int
			if greenSet[i] {
				jitter := 0.7 + rng.Float64()*0.6
				carbon = max(1, int(float64(exp.CarbonLow)*jitter))
			} else {
				jitter := 0.7 + rng.Float64()*0.6
				carbon = max(1, int(float64(exp.CarbonHigh)*jitter))
			}
			if err := mockEco.SetCarbon(ctx, region, carbon, nil); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[carbon-refresh] error setting %s: %v", region, err)
			}
		}
		log.Printf("[carbon-refresh] assigned %d/%d green regions", greenCount, n)
	}

	assignCarbon()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[carbon-refresh] stopped")
			return
		case <-ticker.C:
			assignCarbon()
		}
	}
}

func uniqueRegions(regions []string) []string {
	seen := make(map[string]bool, len(regions))
	var result []string
	for _, r := range regions {
		if r != "" && !seen[r] {
			seen[r] = true
			result = append(result, r)
		}
	}
	return result
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}
