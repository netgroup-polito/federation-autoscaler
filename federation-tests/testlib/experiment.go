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
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// regionPool lists the 50 world regions available for auto-generation.
// Each must have a corresponding entry in regionLocation (deploy.go).
var regionPool = []string{
	"QC", "CA", "NSW", "IDF", "ENG", "13", "HE", "SP",
	"MH", "AB", "LOM", "SG", "VA", "TX", "OR", "IE",
	"NL", "KR", "ZA", "AE",
	"IL", "OH", "GA", "BC", "BY", "RM", "CT", "AN",
	"VIC", "NCL", "HK", "TW", "PH", "CL", "CO", "MX",
	"FI", "NO", "DK", "PL", "CZ", "AT", "CH", "BE",
	"PT", "GR", "RO", "IL2", "EG", "NG",
}

func autoGenerateRegions(n int) []string {
	pool := make([]string, len(regionPool))
	copy(pool, regionPool)
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	regions := make([]string, n)
	for i := range regions {
		regions[i] = pool[i%len(pool)]
	}
	return regions
}

// RandomDelayMs draws one simulated one-way delay uniformly from [minMs, maxMs]
// using rng. Used both to seed the matrix and to redraw it on every refresh,
// so the nearest provider genuinely moves between iterations instead of
// drifting. Takes an explicit *rand.Rand (never the global math/rand source)
// so a caller replaying the same sequence across two phases (see
// SeedFromString) gets a deterministic result unaffected by any unrelated
// goroutine in the process also drawing from the shared global source.
func RandomDelayMs(rng *rand.Rand, minMs, maxMs int) int {
	if maxMs <= minMs {
		return minMs
	}
	return minMs + rng.Intn(maxMs-minMs+1)
}

// SeedFromString deterministically derives an int64 rand seed from s (e.g. a
// run ID). Two rand.Rand instances seeded with the same string produce the
// exact same sequence of draws -- used to make one phase replay another
// phase's exact sequence of randomized values (carbon intensity, simulated
// latency) within the same run, while still varying from one full run to the
// next since each run has its own RunID.
func SeedFromString(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int64(h.Sum64())
}

func autoGenerateConsumerDelays(consumers, providers, minMs, maxMs int) []ConsumerDelayConfig {
	// This initial matrix is generated once at config-load time, before either
	// phase runs, so it has no phase-to-phase replay to stay consistent with --
	// an unpredictable seed here preserves today's "different every run"
	// behavior. Phase B replaying Phase A's sequence (see refreshLatency in
	// comparative-latency/main.go) works by resetting to and then redrawing
	// from THIS realized starting matrix, not by regenerating it.
	rng := rand.New(rand.NewSource(rand.Int63()))
	configs := make([]ConsumerDelayConfig, consumers)
	for c := range configs {
		pds := make([]ProviderDelay, providers)
		for p := range pds {
			pds[p] = ProviderDelay{
				ProviderIndex: p + 1,
				DelayMs:       RandomDelayMs(rng, minMs, maxMs),
			}
		}
		configs[c] = ConsumerDelayConfig{
			ConsumerIndex:  c + 1,
			ProviderDelays: pds,
		}
	}
	return configs
}

// AutoConfig is the fully automated YAML schema. The user specifies counts,
// regions, and experiment parameters; everything else is computed at runtime.
type AutoConfig struct {
	Consumers       int          `yaml:"consumers"`
	Providers       int          `yaml:"providers"`
	ProviderRegions []string     `yaml:"providerRegions"`
	Experiment      TestParams   `yaml:"experiment"`
	Infra           InfraConfig  `yaml:"infra"`
	Cleanup         *bool        `yaml:"cleanup,omitempty"`
	Output          OutputConfig `yaml:"output"`
}

// InfraConfig controls how the orchestrator builds infrastructure.
type InfraConfig struct {
	Registry         string        `yaml:"registry"`
	ReadinessTimeout time.Duration `yaml:"readinessTimeout"`
	LiqoProvider     string        `yaml:"liqoProvider"`
}

// ExperimentConfig is the single YAML file that describes the full
// topology and test parameters. Both comparative-eco and comparative-latency
// read it.
type ExperimentConfig struct {
	Broker     BrokerConfig     `yaml:"broker"`
	Consumers  []ConsumerConfig `yaml:"consumers"`
	Providers  []ProviderConfig `yaml:"providers"`
	Certs      CertsConfig      `yaml:"certs"`
	MockEco    *MockEcoConfig   `yaml:"mockEco,omitempty"`
	Experiment TestParams       `yaml:"experiment"`
	Output     OutputConfig     `yaml:"output"`
}

// BrokerConfig identifies the Broker endpoint.
type BrokerConfig struct {
	URL        string `yaml:"url"`
	ServerName string `yaml:"serverName"`
}

// ConsumerConfig identifies one consumer cluster.
type ConsumerConfig struct {
	ID         string `yaml:"id"`
	ConsoleURL string `yaml:"consoleURL"`
}

// ProviderConfig identifies one provider cluster and its region.
type ProviderConfig struct {
	ID     string `yaml:"id"`
	Region string `yaml:"region"`
}

// CertsConfig points to the mTLS certificates.
type CertsConfig struct {
	Dir      string `yaml:"dir"`
	Prefix   string `yaml:"prefix"`
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
	CAFile   string `yaml:"caFile"`
}

// MockEcoConfig identifies the controllable mock-eco service.
type MockEcoConfig struct {
	URL string `yaml:"url"`
}

// TestParams are the knobs for the experiment.
type TestParams struct {
	Mode       string `yaml:"mode"`
	Iterations int    `yaml:"iterations"`
	// Duration selects how a phase's length is decided: "iterations"
	// (default) runs exactly Iterations synchronized rounds, as it always
	// has; "time" instead runs each phase for Timer wall-clock duration,
	// with every consumer looping independently on its own iteration
	// counter rather than waiting for the others each round (see
	// runReservePhaseTimed) — one consumer may finish more iterations than
	// another in the same window.
	Duration              string        `yaml:"duration"`
	Timer                 time.Duration `yaml:"timer"`
	PhasePause            time.Duration `yaml:"phasePause"`
	PolicyPropagationWait time.Duration `yaml:"policyPropagationWait"`
	AdvertisementLag      time.Duration `yaml:"advertisementLag"`
	WarmupTimeout         time.Duration `yaml:"warmupTimeout"`
	ReservationPoll       time.Duration `yaml:"reservationPoll"`
	ReservationTimeout    time.Duration `yaml:"reservationTimeout"`

	// Eco-specific.
	CarbonLow              int           `yaml:"carbonLow"`
	CarbonHigh             int           `yaml:"carbonHigh"`
	CarbonGreenFractionMin float64       `yaml:"carbonGreenFractionMin"`
	CarbonGreenFractionMax float64       `yaml:"carbonGreenFractionMax"`
	CarbonRefreshInterval  time.Duration `yaml:"carbonRefreshInterval"`
	// EcoCacheTTL is how long a provider agent caches a region's carbon
	// intensity before re-reading it from mock-eco. It must stay well BELOW
	// CarbonRefreshInterval, never equal to it: the value a Consumer finally
	// observes lags the harness's write by up to EcoCacheTTL (cache) plus the
	// provider's 30s advertisement cycle, and if that combined lag is
	// comparable to the refresh period, different providers land on different
	// ticks of the sequence and the two phases stop being observable as the
	// same environment even when they replay the identical sequence.
	EcoCacheTTL time.Duration `yaml:"ecoCacheTTL"`

	// Latency-specific (automated Kind mode).
	TCDelaysAuto           []TCDelayAutoConfig   `yaml:"tcDelays,omitempty"`
	ConsumerDelays         []ConsumerDelayConfig `yaml:"consumerDelays,omitempty"`
	SSHKey                 string                `yaml:"sshKey,omitempty"`
	LatencyRefreshInterval time.Duration         `yaml:"latencyRefreshInterval"`
	// LatencyMinMs/LatencyMaxMs bound the simulated one-way delay drawn for
	// each (consumer, provider) pair, both at setup and again from scratch on
	// every refresh. The ceiling stays under the prober's 300ms per-probe
	// deadline (DefaultProbeTimeout): a delay above it never answers in time,
	// so that provider would be scored unreachable and its RTT would never
	// reach the CSVs.
	LatencyMinMs int `yaml:"latencyMinMs"`
	LatencyMaxMs int `yaml:"latencyMaxMs"`

	// FederationSampleInterval is how often the federation-wide sampler
	// snapshots every consumer's current provider and cost metric
	// (carbon_intensity for comparative-eco, rtt_ms for comparative-latency)
	// into federation.csv, independent of the iteration/keep/switch cadence.
	FederationSampleInterval time.Duration `yaml:"federationSampleInterval"`
}

// IsTimeBased reports whether phases run for a wall-clock Timer duration
// (with consumers iterating independently) instead of a fixed Iterations
// count.
func (t TestParams) IsTimeBased() bool {
	return t.Duration == DurationTime
}

// TCDelayAutoConfig is the automated tc delay config (uses providerIndex).
type TCDelayAutoConfig struct {
	ProviderIndex int    `yaml:"providerIndex"` // 1-based index
	DelayMs       int    `yaml:"delayMs"`
	Interface     string `yaml:"interface,omitempty"` // default "eth0"
}

// ConsumerDelayConfig specifies per-provider delays for one consumer.
// When present, delays are applied on consumer containers (lato consumer)
// instead of on provider containers, enabling per-consumer latency simulation.
type ConsumerDelayConfig struct {
	ConsumerIndex  int             `yaml:"consumerIndex"`
	ProviderDelays []ProviderDelay `yaml:"providerDelays"`
}

// ProviderDelay is one entry in the consumer delay matrix.
type ProviderDelay struct {
	ProviderIndex int `yaml:"providerIndex"`
	DelayMs       int `yaml:"delayMs"`
}

// TCDelayConfig is one per-provider netem delay entry (legacy SSH mode).
type TCDelayConfig struct {
	Host      string `yaml:"host"`
	Interface string `yaml:"interface"`
	DelayMs   int    `yaml:"delayMs"`
}

// OutputConfig controls where results go.
type OutputConfig struct {
	Dir string `yaml:"dir"`
}

// LoadAutoConfig reads the automated YAML config file.
func LoadAutoConfig(path string) (*AutoConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg AutoConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	cfg.applyDefaults()
	return &cfg, cfg.Validate()
}

// advertisementCycle mirrors advertise.DefaultInterval: the provider agent
// republishes its advertisement (carbon value included) on this cadence and it
// is not configurable from the harness, so it is the floor on how quickly any
// environment change can become visible to a Consumer.
const advertisementCycle = 30 * time.Second

// ProberCacheTTL mirrors latency.DefaultCacheTTL: how long a Consumer reuses a
// measured RTT before re-probing. Not configurable from the harness, so it is
// the floor on how quickly a redrawn tc delay can become visible -- and the
// span for which a Consumer entering a new phase keeps serving measurements it
// took during the previous one. comparative-latency waits it out at the start
// of BOTH phases so neither begins with a warmer cache than the other.
const ProberCacheTTL = 15 * time.Second

// applyDefaults fills in every unset field with the value the suites run
// with. It is split by area so each group stays readable on its own.
func (c *AutoConfig) applyDefaults() {
	c.applyTopologyDefaults()
	c.applyExperimentDefaults()
	c.applyEcoDefaults()
	c.applyLatencyDefaults()
	c.applyInfraDefaults()
	c.applyGeneratedDefaults()
}

// applyTopologyDefaults sizes the federation.
func (c *AutoConfig) applyTopologyDefaults() {
	if c.Consumers <= 0 {
		c.Consumers = 1
	}
	if c.Providers <= 0 {
		c.Providers = 2
	}
}

// applyExperimentDefaults fills the phase pacing and reservation timeouts.
func (c *AutoConfig) applyExperimentDefaults() {
	if c.Experiment.Mode == "" {
		c.Experiment.Mode = ModeObserve
	}
	if c.Experiment.Iterations <= 0 {
		c.Experiment.Iterations = 10
	}
	if c.Experiment.Duration == "" {
		c.Experiment.Duration = DurationIterations
	}
	if c.Experiment.PhasePause <= 0 {
		c.Experiment.PhasePause = 30 * time.Second
	}
	if c.Experiment.PolicyPropagationWait <= 0 {
		c.Experiment.PolicyPropagationWait = 20 * time.Second
	}
	if c.Experiment.AdvertisementLag <= 0 {
		c.Experiment.AdvertisementLag = 35 * time.Second
	}
	if c.Experiment.WarmupTimeout <= 0 {
		c.Experiment.WarmupTimeout = 5 * time.Minute
	}
	if c.Experiment.ReservationPoll <= 0 {
		c.Experiment.ReservationPoll = 5 * time.Second
	}
	if c.Experiment.ReservationTimeout <= 0 {
		c.Experiment.ReservationTimeout = 10 * time.Minute
	}
}

// applyEcoDefaults fills the carbon generator's range and cadence.
func (c *AutoConfig) applyEcoDefaults() {
	if c.Experiment.CarbonLow <= 0 {
		c.Experiment.CarbonLow = 50
	}
	if c.Experiment.CarbonHigh <= 0 {
		c.Experiment.CarbonHigh = 800
	}
	if c.Experiment.CarbonGreenFractionMin <= 0 {
		c.Experiment.CarbonGreenFractionMin = 0.3
	}
	if c.Experiment.CarbonGreenFractionMax <= 0 {
		c.Experiment.CarbonGreenFractionMax = 0.7
	}
	if c.Experiment.CarbonRefreshInterval <= 0 {
		c.Experiment.CarbonRefreshInterval = 3 * time.Minute
	}
	if c.Experiment.EcoCacheTTL <= 0 {
		c.Experiment.EcoCacheTTL = 5 * time.Second
	}
	// A Consumer sees a carbon value at most EcoCacheTTL + the provider's 30s
	// advertisement cycle after the harness wrote it. Once that lag reaches a
	// sizeable share of one refresh tick, providers are observed on different
	// ticks and the phases can no longer be overlaid point-by-point, even
	// though Phase B replays Phase A's exact sequence.
	if lag := c.Experiment.EcoCacheTTL + advertisementCycle; lag*2 > c.Experiment.CarbonRefreshInterval {
		log.Printf("[config] WARNING: carbon observation lag (ecoCacheTTL %s + %s advertisement cycle = %s) "+
			"is more than half of carbonRefreshInterval (%s); Phase A and Phase B will not be "+
			"point-by-point comparable. Lower ecoCacheTTL or raise carbonRefreshInterval.",
			c.Experiment.EcoCacheTTL, advertisementCycle, lag, c.Experiment.CarbonRefreshInterval)
	}
}

// applyLatencyDefaults fills the simulated-delay range and cadence.
func (c *AutoConfig) applyLatencyDefaults() {
	if c.Experiment.LatencyRefreshInterval <= 0 {
		c.Experiment.LatencyRefreshInterval = 3 * time.Minute
	}
	// Same check on the latency side. The lag here is just the Consumer's
	// prober cache: it probes the provider's echo endpoint directly, so no
	// advertisement cycle sits in this path.
	if ProberCacheTTL*2 > c.Experiment.LatencyRefreshInterval {
		log.Printf("[config] WARNING: latency observation lag (%s prober cache) is more than half of "+
			"latencyRefreshInterval (%s); Phase A and Phase B will not be point-by-point comparable. "+
			"Raise latencyRefreshInterval.", ProberCacheTTL, c.Experiment.LatencyRefreshInterval)
	}
	if c.Experiment.LatencyMinMs <= 0 {
		c.Experiment.LatencyMinMs = 30
	}
	if c.Experiment.LatencyMaxMs <= c.Experiment.LatencyMinMs {
		c.Experiment.LatencyMaxMs = 250
	}
}

// applyInfraDefaults fills sampling, output and cluster-side settings.
func (c *AutoConfig) applyInfraDefaults() {
	if c.Experiment.FederationSampleInterval <= 0 {
		c.Experiment.FederationSampleInterval = time.Minute
	}
	if c.Output.Dir == "" {
		c.Output.Dir = "results"
	}
	if c.Infra.ReadinessTimeout <= 0 {
		c.Infra.ReadinessTimeout = 10 * time.Minute
	}
	if c.Infra.LiqoProvider == "" {
		c.Infra.LiqoProvider = liqoProviderKind
	}
	if c.Cleanup == nil {
		t := true
		c.Cleanup = &t
	}
}

// applyGeneratedDefaults draws the values that are not fixed numbers:
// provider regions and the consumer-to-provider delay matrix.
func (c *AutoConfig) applyGeneratedDefaults() {
	if len(c.ProviderRegions) == 0 {
		c.ProviderRegions = autoGenerateRegions(c.Providers)
		log.Printf("[config] auto-generated regions: %v", c.ProviderRegions)
	}
	if len(c.Experiment.ConsumerDelays) == 0 && len(c.Experiment.TCDelaysAuto) == 0 {
		c.Experiment.ConsumerDelays = autoGenerateConsumerDelays(
			c.Consumers, c.Providers, c.Experiment.LatencyMinMs, c.Experiment.LatencyMaxMs)
		// Print the full matrix only while it is small enough to read: it has
		// consumers x providers entries, so at 30 x 70 the per-entry form
		// dumped 2100 lines before the run even started.
		if c.Providers <= logProviderDelaysThreshold {
			for _, cd := range c.Experiment.ConsumerDelays {
				for _, pd := range cd.ProviderDelays {
					log.Printf("[config] auto-generated consumer delay: consumer-%d → provider-%d: %dms",
						cd.ConsumerIndex, pd.ProviderIndex, pd.DelayMs)
				}
			}
		} else {
			log.Printf("[config] auto-generated consumer delays: %d consumers x %d providers",
				c.Consumers, c.Providers)
		}
	}
	for i := range c.Experiment.TCDelaysAuto {
		if c.Experiment.TCDelaysAuto[i].Interface == "" {
			c.Experiment.TCDelaysAuto[i].Interface = "eth0"
		}
	}
}

// Validate checks the automated config for completeness.
func (c *AutoConfig) Validate() error {
	if c.Consumers < 1 {
		return fmt.Errorf("consumers must be >= 1")
	}
	if c.Providers < 2 {
		return fmt.Errorf("providers must be >= 2")
	}
	if len(c.ProviderRegions) > 0 && len(c.ProviderRegions) != c.Providers {
		return fmt.Errorf("providerRegions length (%d) must match providers (%d)", len(c.ProviderRegions), c.Providers)
	}
	if c.Experiment.Mode != ModeObserve && c.Experiment.Mode != ModeReserve {
		return fmt.Errorf("experiment.mode must be observe or reserve (got %q)", c.Experiment.Mode)
	}
	if c.Experiment.Duration != DurationIterations && c.Experiment.Duration != DurationTime {
		return fmt.Errorf("experiment.duration must be iterations or time (got %q)", c.Experiment.Duration)
	}
	if c.Experiment.IsTimeBased() && c.Experiment.Timer <= 0 {
		return fmt.Errorf("experiment.timer must be > 0 when duration is \"time\"")
	}
	for _, td := range c.Experiment.TCDelaysAuto {
		if td.ProviderIndex < 1 || td.ProviderIndex > c.Providers {
			return fmt.Errorf("tcDelays.providerIndex %d out of range [1, %d]", td.ProviderIndex, c.Providers)
		}
	}
	for _, cd := range c.Experiment.ConsumerDelays {
		if cd.ConsumerIndex < 1 || cd.ConsumerIndex > c.Consumers {
			return fmt.Errorf("consumerDelays.consumerIndex %d out of range [1, %d]", cd.ConsumerIndex, c.Consumers)
		}
		for _, pd := range cd.ProviderDelays {
			if pd.ProviderIndex < 1 || pd.ProviderIndex > c.Providers {
				return fmt.Errorf("consumerDelays[consumer-%d].providerIndex %d out of range [1, %d]",
					cd.ConsumerIndex, pd.ProviderIndex, c.Providers)
			}
		}
	}
	return nil
}

// ShouldCleanup returns whether cleanup should run.
func (c *AutoConfig) ShouldCleanup() bool {
	return c.Cleanup == nil || *c.Cleanup
}

// Orchestrator manages the full lifecycle of a comparative test run.
type Orchestrator struct {
	Config        *AutoConfig
	TestType      string // "comparative-eco" or "comparative-latency"
	RunID         string
	RepoRoot      string
	Specs         []ClusterSpec
	KubeconfigDir string
	CADir         string
	Clients       *ExperimentClients
	OutputDir     string
	KeepClusters  bool
	SkipBuild     bool
}

// NewOrchestrator creates an orchestrator for the given config.
func NewOrchestrator(cfg *AutoConfig, testType string, keepClusters, skipBuild bool, runID string) (*Orchestrator, error) {
	repoRoot, err := RepoRoot()
	if err != nil {
		return nil, fmt.Errorf("find repo root: %w", err)
	}
	if runID == "" {
		runID = RunID()
	}
	return &Orchestrator{
		Config:       cfg,
		TestType:     testType,
		RunID:        runID,
		RepoRoot:     repoRoot,
		KeepClusters: keepClusters,
		SkipBuild:    skipBuild,
	}, nil
}

// Setup creates clusters, builds images, deploys components, and waits for readiness.
func (o *Orchestrator) Setup(ctx context.Context) error {
	log.Println("=== PREREQUISITES ===")
	if err := CheckPrerequisites(); err != nil {
		return err
	}

	// Create temp dirs for PKI and kubeconfigs.
	var err error
	o.KubeconfigDir, err = os.MkdirTemp("", o.RunID+"-kubeconfigs-")
	if err != nil {
		return fmt.Errorf("create kubeconfig dir: %w", err)
	}
	o.CADir, err = os.MkdirTemp("", o.RunID+"-pki-")
	if err != nil {
		return fmt.Errorf("create CA dir: %w", err)
	}

	imgPrefix := "federation-autoscaler"
	imgTag := "latest"
	registry := DefaultStandaloneRegistry

	// Build Docker images.
	if !o.SkipBuild {
		log.Println("=== BUILD IMAGES ===")
		if err := DockerBuild(ctx, o.RepoRoot); err != nil {
			return fmt.Errorf("docker build: %w", err)
		}
	}

	// Retag images to match standalone scripts' naming convention.
	log.Println("=== RETAG IMAGES ===")
	if err := RetagForStandalone(ctx, imgPrefix, imgTag, registry); err != nil {
		return fmt.Errorf("retag: %w", err)
	}

	// Pre-pull Liqo's images once on the host, so kind-loading them into
	// each provider/consumer cluster (below) never re-downloads from
	// ghcr.io per cluster. Sequential, not backgrounded: a goroutine left
	// running past an early return (e.g. a later cluster-creation failure)
	// would leak with its error never checked.
	log.Println("=== PRELOAD LIQO + UDPECHO IMAGES ===")
	if err := DockerPullImages(ctx, append(append([]string{}, LiqoImages...), UDPEchoImage)); err != nil {
		return fmt.Errorf("pull liqo/udpecho images: %w", err)
	}

	// Generate cluster specs.
	log.Println("=== CREATE CLUSTERS ===")
	o.Specs = GenerateClusterSpecs(o.RunID, o.Config.Consumers, o.Config.Providers)
	for i := range o.Specs {
		if err := KindCreateCluster(ctx, &o.Specs[i], o.KubeconfigDir); err != nil {
			return fmt.Errorf("create cluster %s: %w", o.Specs[i].Name, err)
		}
	}

	// Load images into clusters (using standalone naming).
	log.Println("=== LOAD IMAGES ===")
	centralImgs := CentralImages(registry, imgTag)
	agentImgs := AgentImages(registry, imgTag)
	consumerExtra := ConsumerExtraImages(registry, imgTag)

	if err := KindLoadImages(ctx, o.Specs[0].Name, centralImgs); err != nil {
		return fmt.Errorf("load images into central: %w", err)
	}
	for i := 0; i < o.Config.Consumers; i++ {
		imgs := append([]string{}, agentImgs...)
		imgs = append(imgs, consumerExtra...)
		imgs = append(imgs, LiqoImages...)
		if err := KindLoadImages(ctx, o.Specs[1+i].Name, imgs); err != nil {
			return fmt.Errorf("load images into consumer-%d: %w", i+1, err)
		}
	}
	for i := 0; i < o.Config.Providers; i++ {
		imgs := append([]string{}, agentImgs...)
		imgs = append(imgs, LiqoImages...)
		imgs = append(imgs, UDPEchoImage)
		if err := KindLoadImages(ctx, o.Specs[1+o.Config.Consumers+i].Name, imgs); err != nil {
			return fmt.Errorf("load images into provider-%d: %w", i+1, err)
		}
	}

	// Deploy all components (don't pass --registry/--tag — use standalone defaults).
	log.Println("=== DEPLOY COMPONENTS ===")
	deployOpts := DeployOpts{
		RepoRoot:        o.RepoRoot,
		RunID:           o.RunID,
		Specs:           o.Specs,
		KubeconfigDir:   o.KubeconfigDir,
		CADir:           o.CADir,
		NumConsumers:    o.Config.Consumers,
		NumProviders:    o.Config.Providers,
		ProviderRegions: o.Config.ProviderRegions,
		ImgPrefix:       registry,
		ImgTag:          imgTag,
		LiqoProvider:    o.Config.Infra.LiqoProvider,
		EcoCacheTTL:     o.Config.Experiment.EcoCacheTTL,
	}
	if err := DeployAll(ctx, deployOpts); err != nil {
		return fmt.Errorf("deploy: %w", err)
	}

	// Cap each provider's advertised capacity to 2 standard chunks (4 CPU /
	// 8 GiB). Left at the default, a provider advertises its Kind node's
	// real allocatable — the shared host's full CPU/RAM — so every provider
	// has far more room than this many consumers could ever fill, and the
	// single cheapest/greenest provider never actually saturates. Capping it
	// this low means only 2 consumers fit on any one provider before the
	// policy has to move on to the next-best, which is the point: without
	// it, every consumer piling onto the same provider is a test-environment
	// artifact, not a property of the placement policy being compared.
	log.Println("=== CAP PROVIDER CAPACITY ===")
	for i := 0; i < o.Config.Providers; i++ {
		spec := o.Specs[1+o.Config.Consumers+i]
		if err := SetProviderCapacity(ctx, spec.Kubeconfig, "4000m", "8Gi"); err != nil {
			return fmt.Errorf("cap capacity on provider-%d: %w", i+1, err)
		}
	}

	// Resolve identities from all consumer join bundles.
	log.Println("=== WAIT FOR READINESS ===")
	identities, caFile, err := o.resolveIdentities()
	if err != nil {
		return fmt.Errorf("resolve identities: %w", err)
	}

	centralIP, err := ContainerIP(ctx, o.Specs[0].Name+"-control-plane")
	if err != nil {
		return fmt.Errorf("get central IP: %w", err)
	}
	brokerURL := fmt.Sprintf("https://%s:30443", centralIP)
	serverName := "broker.federation-autoscaler-system.svc"

	var consoleURLs []string
	for i := 0; i < o.Config.Consumers; i++ {
		consIP, err := ContainerIP(ctx, o.Specs[1+i].Name+"-control-plane")
		if err != nil {
			return fmt.Errorf("get consumer-%d IP: %w", i+1, err)
		}
		consoleURLs = append(consoleURLs, fmt.Sprintf("http://%s:30445", consIP))
	}

	var expectedProviders []string
	for i := 1; i <= o.Config.Providers; i++ {
		expectedProviders = append(expectedProviders, fmt.Sprintf("provider-%d", i))
	}

	o.Clients, err = WaitForReadiness(ctx, brokerURL, serverName, consoleURLs, expectedProviders, identities, caFile, o.Config.Infra.ReadinessTimeout)
	if err != nil {
		return fmt.Errorf("readiness: %w", err)
	}

	// Create output dir.
	o.OutputDir, err = EnsureOutputDir(o.Config.Output.Dir, o.TestType)
	if err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	log.Printf("[setup] run=%s output=%s", o.RunID, o.OutputDir)
	return nil
}

func (o *Orchestrator) resolveIdentities() (map[string]Identity, string, error) {
	bundleDir := filepath.Join(o.CADir, "bundles")
	identities := make(map[string]Identity, o.Config.Consumers)
	var caFile string

	for i := 1; i <= o.Config.Consumers; i++ {
		cid := fmt.Sprintf("consumer-%d", i)
		bundlePath := filepath.Join(bundleDir, cid+"-bundle.tgz")

		extractDir, err := os.MkdirTemp("", fmt.Sprintf("%s-%s-extract-", o.RunID, cid))
		if err != nil {
			return nil, "", err
		}

		cmd := exec.Command("tar", "-xzf", bundlePath, "-C", extractDir)
		if err := cmd.Run(); err != nil {
			// Best-effort: the extraction error is the one worth reporting.
			_ = os.RemoveAll(extractDir)
			return nil, "", fmt.Errorf("extract bundle for %s: %w", cid, err)
		}

		certFile := filepath.Join(extractDir, "client.crt")
		keyFile := filepath.Join(extractDir, "client.key")

		id, err := ResolveIdentityFromFiles(certFile, keyFile, filepath.Join(extractDir, "ca.crt"))
		if err != nil {
			// Best-effort: the identity error is the one worth reporting.
			_ = os.RemoveAll(extractDir)
			return nil, "", fmt.Errorf("resolve identity for %s: %w", cid, err)
		}
		identities[cid] = id

		if caFile == "" {
			caFile = filepath.Join(extractDir, "ca.crt")
		}
	}

	return identities, caFile, nil
}

// Teardown deletes all Kind clusters created by this run.
func (o *Orchestrator) Teardown(ctx context.Context) {
	if o.KeepClusters {
		log.Printf("[cleanup] --keep-clusters: skipping cleanup. Delete manually:")
		for _, s := range o.Specs {
			log.Printf("  kind delete cluster --name %s", s.Name)
		}
		return
	}

	log.Println("=== CLEANUP ===")
	for _, s := range o.Specs {
		if err := KindDeleteCluster(ctx, s.Name); err != nil {
			log.Printf("[cleanup] warning: failed to delete %s: %v", s.Name, err)
		}
	}

	// Best-effort: cleanup runs after the results are already written, and a
	// leftover temp directory is not worth failing the run over.
	if o.KubeconfigDir != "" {
		_ = os.RemoveAll(o.KubeconfigDir)
	}
	if o.CADir != "" {
		_ = os.RemoveAll(o.CADir)
	}
	log.Println("[cleanup] done")
}

// BrokerURL returns the broker URL computed from the central cluster.
func (o *Orchestrator) BrokerURL(ctx context.Context) (string, error) {
	centralIP, err := ContainerIP(ctx, o.Specs[0].Name+"-control-plane")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://%s:30443", centralIP), nil
}

// ConsoleURL returns the console URL for a consumer by 0-based index.
func (o *Orchestrator) ConsoleURL(ctx context.Context, consumerIdx int) (string, error) {
	consIP, err := ContainerIP(ctx, o.Specs[1+consumerIdx].Name+"-control-plane")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://%s:30445", consIP), nil
}

// MockEcoURL returns the mock-eco URL on the central cluster.
func (o *Orchestrator) MockEcoURL(ctx context.Context) (string, error) {
	centralIP, err := ContainerIP(ctx, o.Specs[0].Name+"-control-plane")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://%s:30081", centralIP), nil
}

// ConsumerContainerName returns the Docker container name for a consumer by 1-based index.
// Mirrors ProviderContainerName: with a worker node the consumer agent — and so
// the UDP prober whose egress the tc delays are meant to shape — is scheduled on
// the worker, because a multi-node Kind cluster keeps the control-plane tainted
// NoSchedule. Naming the control-plane here installed the qdisc on a container
// the probe traffic never traverses, so injected delays of 1-200ms produced
// measured RTTs of 0.2-0.4ms and the latency policy had nothing to act on.
func (o *Orchestrator) ConsumerContainerName(consumerIdx int) string {
	spec := o.Specs[1+consumerIdx-1]
	if spec.HasWorker {
		return spec.Name + "-worker"
	}
	return spec.Name + "-control-plane"
}

// ProviderContainerName returns the Docker container name for a provider by 1-based index.
// For clusters with a worker node the agent (and its probe endpoint) lives on
// the worker, so tc delays must target that container.
func (o *Orchestrator) ProviderContainerName(providerIdx int) string {
	spec := o.Specs[1+o.Config.Consumers+providerIdx-1]
	if spec.HasWorker {
		return spec.Name + "-worker"
	}
	return spec.Name + "-control-plane"
}

// BuildExperimentConfig converts the AutoConfig into the runtime ExperimentConfig
// used by the experiment logic (for backward compatibility with existing code).
func (o *Orchestrator) BuildExperimentConfig(ctx context.Context) (*ExperimentConfig, error) {
	brokerURL, err := o.BrokerURL(ctx)
	if err != nil {
		return nil, err
	}

	var consumers []ConsumerConfig
	for i := 0; i < o.Config.Consumers; i++ {
		cURL, err := o.ConsoleURL(ctx, i)
		if err != nil {
			return nil, err
		}
		consumers = append(consumers, ConsumerConfig{
			ID:         fmt.Sprintf("consumer-%d", i+1),
			ConsoleURL: cURL,
		})
	}

	var providers []ProviderConfig
	for i := 0; i < o.Config.Providers; i++ {
		region := ""
		if i < len(o.Config.ProviderRegions) {
			region = o.Config.ProviderRegions[i]
		}
		providers = append(providers, ProviderConfig{
			ID:     fmt.Sprintf("provider-%d", i+1),
			Region: region,
		})
	}

	var mockEco *MockEcoConfig
	meURL, err := o.MockEcoURL(ctx)
	if err == nil {
		mockEco = &MockEcoConfig{URL: meURL}
	}

	return &ExperimentConfig{
		Broker: BrokerConfig{
			URL:        brokerURL,
			ServerName: "broker.federation-autoscaler-system.svc",
		},
		Consumers:  consumers,
		Providers:  providers,
		MockEco:    mockEco,
		Experiment: o.Config.Experiment,
		Output:     o.Config.Output,
	}, nil
}

// --- Legacy functions kept for backward compatibility ---

// LoadExperimentConfig reads and validates a YAML config file (legacy format).
func LoadExperimentConfig(path string) (*ExperimentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg ExperimentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	cfg.applyDefaults()
	return &cfg, cfg.ValidateLegacy()
}

func (c *ExperimentConfig) applyDefaults() {
	if c.Certs.Prefix == "" {
		c.Certs.Prefix = "scaltest"
	}
	if c.Experiment.Mode == "" {
		c.Experiment.Mode = ModeObserve
	}
	if c.Experiment.Iterations <= 0 {
		c.Experiment.Iterations = 10
	}
	if c.Experiment.Duration == "" {
		c.Experiment.Duration = DurationIterations
	}
	if c.Experiment.PhasePause <= 0 {
		c.Experiment.PhasePause = 30 * time.Second
	}
	if c.Experiment.PolicyPropagationWait <= 0 {
		c.Experiment.PolicyPropagationWait = 20 * time.Second
	}
	if c.Experiment.AdvertisementLag <= 0 {
		c.Experiment.AdvertisementLag = 35 * time.Second
	}
	if c.Experiment.WarmupTimeout <= 0 {
		c.Experiment.WarmupTimeout = 5 * time.Minute
	}
	if c.Experiment.ReservationPoll <= 0 {
		c.Experiment.ReservationPoll = 5 * time.Second
	}
	if c.Experiment.ReservationTimeout <= 0 {
		c.Experiment.ReservationTimeout = 10 * time.Minute
	}
	if c.Experiment.CarbonLow <= 0 {
		c.Experiment.CarbonLow = 50
	}
	if c.Experiment.CarbonHigh <= 0 {
		c.Experiment.CarbonHigh = 800
	}
	if c.Experiment.CarbonGreenFractionMin <= 0 {
		c.Experiment.CarbonGreenFractionMin = 0.3
	}
	if c.Experiment.CarbonGreenFractionMax <= 0 {
		c.Experiment.CarbonGreenFractionMax = 0.7
	}
	if c.Experiment.CarbonRefreshInterval <= 0 {
		c.Experiment.CarbonRefreshInterval = 3 * time.Minute
	}
	if c.Experiment.EcoCacheTTL <= 0 {
		c.Experiment.EcoCacheTTL = 5 * time.Second
	}
	if c.Experiment.LatencyRefreshInterval <= 0 {
		c.Experiment.LatencyRefreshInterval = 3 * time.Minute
	}
	if c.Experiment.LatencyMinMs <= 0 {
		c.Experiment.LatencyMinMs = 30
	}
	if c.Experiment.LatencyMaxMs <= c.Experiment.LatencyMinMs {
		c.Experiment.LatencyMaxMs = 250
	}
	if c.Experiment.FederationSampleInterval <= 0 {
		c.Experiment.FederationSampleInterval = time.Minute
	}
	if c.Output.Dir == "" {
		c.Output.Dir = "results"
	}
}

// ValidateLegacy checks the legacy config for completeness.
func (c *ExperimentConfig) ValidateLegacy() error {
	if c.Broker.URL == "" {
		return fmt.Errorf("broker.url is required")
	}
	if len(c.Consumers) == 0 {
		return fmt.Errorf("at least one consumer is required")
	}
	for i, cons := range c.Consumers {
		if cons.ID == "" {
			return fmt.Errorf("consumers[%d].id is required", i)
		}
		if cons.ConsoleURL == "" {
			return fmt.Errorf("consumers[%d].consoleURL is required", i)
		}
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("at least one provider is required")
	}
	for i, prov := range c.Providers {
		if prov.ID == "" {
			return fmt.Errorf("providers[%d].id is required", i)
		}
	}
	if c.Certs.Dir == "" && (c.Certs.CertFile == "" || c.Certs.KeyFile == "" || c.Certs.CAFile == "") {
		return fmt.Errorf("certs: either dir or certFile+keyFile+caFile is required")
	}
	if c.Experiment.Mode != ModeObserve && c.Experiment.Mode != ModeReserve {
		return fmt.Errorf("experiment.mode must be observe or reserve (got %q)", c.Experiment.Mode)
	}
	return nil
}

// BuildClients builds all the clients needed for the experiment and validates
// that every component is reachable.
func (c *ExperimentConfig) BuildClients(ctx context.Context) (*ExperimentClients, error) {
	var id Identity
	var caFile string
	var err error
	if c.Certs.Dir != "" {
		id, caFile, err = ResolveConsumerIdentity(c.Certs.Dir, c.Certs.Prefix)
	} else {
		id, err = ResolveIdentityFromFiles(c.Certs.CertFile, c.Certs.KeyFile, c.Certs.CAFile)
		caFile = c.Certs.CAFile
	}
	if err != nil {
		return nil, fmt.Errorf("resolve identity: %w", err)
	}

	certFP, _ := id.CertFingerprint()

	broker, err := NewBrokerClientFromIdentity(c.Broker.URL, c.Broker.ServerName, id, caFile)
	if err != nil {
		return nil, fmt.Errorf("build broker client: %w", err)
	}

	log.Printf("[setup] checking broker at %s...", c.Broker.URL)
	if err := broker.CheckReachable(ctx); err != nil {
		return nil, fmt.Errorf("broker not reachable at %s: %w", c.Broker.URL, err)
	}
	log.Println("[setup] broker: OK")

	ngResp, err := broker.GetNodeGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("get nodegroups: %w", err)
	}
	advertisingIDs := UniqueProviderIDs(ngResp.NodeGroups)
	advertisingSet := make(map[string]bool, len(advertisingIDs))
	for _, pid := range advertisingIDs {
		advertisingSet[pid] = true
	}
	for _, prov := range c.Providers {
		if !advertisingSet[prov.ID] {
			return nil, fmt.Errorf("provider %q (region=%s) is not advertising to the broker — expected: %v, advertising: %v",
				prov.ID, prov.Region, providerIDs(c.Providers), advertisingIDs)
		}
	}
	log.Printf("[setup] all %d providers advertising: %v", len(c.Providers), providerIDs(c.Providers))

	consoles := make(map[string]*ConsoleClient, len(c.Consumers))
	for _, cons := range c.Consumers {
		cc := NewConsoleClient(cons.ConsoleURL)
		log.Printf("[setup] checking consumer %s console at %s...", cons.ID, cons.ConsoleURL)
		_, err := cc.getState(ctx)
		if err != nil {
			return nil, fmt.Errorf("consumer %q console not reachable at %s: %w", cons.ID, cons.ConsoleURL, err)
		}
		consoles[cons.ID] = cc
		log.Printf("[setup] consumer %s: OK", cons.ID)
	}

	var eco *MockEcoClient
	if c.MockEco != nil && c.MockEco.URL != "" {
		eco = NewMockEcoClient(c.MockEco.URL)
		log.Printf("[setup] checking mock-eco at %s...", c.MockEco.URL)
		if err := eco.CheckHealthy(ctx); err != nil {
			return nil, fmt.Errorf("mock-eco not reachable at %s: %w", c.MockEco.URL, err)
		}
		log.Println("[setup] mock-eco: OK")
	}

	return &ExperimentClients{
		Identity:   id,
		CertFP:     certFP,
		Broker:     broker,
		Consoles:   consoles,
		MockEco:    eco,
		InitialNGs: ngResp,
	}, nil
}

// ExperimentClients holds all validated, ready-to-use clients.
type ExperimentClients struct {
	Identity   Identity
	CertFP     string
	Broker     *BrokerClient            // primary (consumer-1) for shared queries
	Brokers    map[string]*BrokerClient // per-consumer broker clients for reservations
	Consoles   map[string]*ConsoleClient
	MockEco    *MockEcoClient
	InitialNGs interface{}
}

// BrokerFor returns the BrokerClient for a specific consumer. Falls back to the
// primary Broker if no per-consumer client exists.
func (ec *ExperimentClients) BrokerFor(consumerID string) *BrokerClient {
	if bc, ok := ec.Brokers[consumerID]; ok {
		return bc
	}
	return ec.Broker
}

// SetPolicyAll sets the same policy on ALL consumers and waits for propagation.
func (ec *ExperimentClients) SetPolicyAll(ctx context.Context, policyType string, propagationWait time.Duration) error {
	for consID, cc := range ec.Consoles {
		if err := cc.SetPolicy(ctx, policyType); err != nil {
			return fmt.Errorf("set policy %q on consumer %s: %w", policyType, consID, err)
		}
		log.Printf("[policy] consumer %s → %s", consID, policyType)
	}
	if propagationWait > 0 {
		log.Printf("[policy] waiting %s for propagation...", propagationWait)
		return SleepCtx(ctx, propagationWait)
	}
	return nil
}

// SleepCtx sleeps for d or until ctx is cancelled.
func SleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// EnsureExperimentOutputDir creates the output directory for a specific test type.
func (c *ExperimentConfig) EnsureExperimentOutputDir(testType string) (string, error) {
	return EnsureOutputDir(c.Output.Dir, testType)
}

func providerIDs(providers []ProviderConfig) []string {
	ids := make([]string, len(providers))
	for i, p := range providers {
		ids[i] = p.ID
	}
	return ids
}
