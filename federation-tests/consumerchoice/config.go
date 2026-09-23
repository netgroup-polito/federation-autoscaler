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
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/netgroup-polito/federation-autoscaler/federation-tests/testlib"
)

const (
	// testType names the output directory: results/consumerchoice/<timestamp>.
	testType = "consumerchoice"
	// requiredMode is the only accepted consumerChoice.mode. The check exists so
	// that a config copied from another suite fails loudly instead of quietly
	// running some other policy under this suite's name.
	requiredMode = "consumerchoice"

	fallbackFail         = "fail"
	fallbackAgentDefault = "agent-default"
	fallbackCheapest     = "cheapest-eligible"
	fallbackLowestCarbon = "lowest-carbon-eligible"

	criterionEco       = "eco"
	criterionProximity = "proximity"
	criterionBalanced  = "balanced"
	criterionNone      = "none"

	// defaultOllamaImage is pinned so a run is reproducible: "latest" would let
	// the model runtime change underneath an otherwise identical experiment.
	defaultOllamaImage = "ollama/ollama:0.34.0"
	defaultModel       = "llama3.2" // the agent's own --ollama-model default
	// agentDefaultOllamaTimeout is the agent's --ollama-timeout default.
	agentDefaultOllamaTimeout = 120 * time.Second

	// Chunk size the Broker's default sizer uses: 2 CPU / 4 GiB.
	chunkMilliCPU = 2000
	chunkMemBytes = 4 * 1024 * 1024 * 1024
)

// scenarioName keeps names short, lowercase and safe as directory names. They
// are not part of reservation IDs (see reservationID), which must stay within
// Kubernetes' 63-character label limit whatever the names are.
var scenarioName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,18}[a-z0-9])?$`)

type flags struct {
	configPath   string
	keepClusters bool
	skipBuild    bool
	runID        string
}

func parseFlags() flags {
	var f flags
	flag.StringVar(&f.configPath, "config", "", "Path to the ConsumerChoice YAML config file.")
	flag.BoolVar(&f.keepClusters, "keep-clusters", false,
		"Keep Kind clusters and the Ollama container after the run (same as cleanup: false).")
	flag.BoolVar(&f.skipBuild, "skip-build", false,
		"Skip make docker-build and use pre-built images. Do not use after changing Broker or agent code.")
	flag.StringVar(&f.runID, "run-id", "", "Override the run ID (default: auto-generated).")
	flag.Parse()
	if f.configPath == "" {
		fmt.Fprintln(os.Stderr,
			"usage: consumerchoice --config <config.yaml> [--keep-clusters] [--skip-build] [--run-id <id>]")
		os.Exit(2)
	}
	return f
}

// ChoiceConfig is the consumerChoice: section of the config file. The rest of
// the file is the shared schema every comparative suite reads through
// testlib.LoadAutoConfig.
type ChoiceConfig struct {
	Mode             string            `yaml:"mode" json:"mode"`
	Repetitions      int               `yaml:"repetitions" json:"repetitions"`
	MetadataTimeout  time.Duration     `yaml:"metadataTimeout" json:"metadataTimeout"`
	PolicyTimeout    time.Duration     `yaml:"policyTimeout" json:"policyTimeout"`
	Ollama           OllamaConfig      `yaml:"ollama" json:"ollama"`
	AgentPath        AgentPathConfig   `yaml:"agentPath" json:"agentPath"`
	Fallback         FallbackConfig    `yaml:"fallback" json:"fallback"`
	ProviderProfiles []ProviderProfile `yaml:"providerProfiles" json:"providerProfiles"`
	Scenarios        []Scenario        `yaml:"scenarios" json:"scenarios"`
}

// OllamaConfig controls the model runtime. The model is queried exactly as the
// consumer agent queries it -- plain JSON mode, the server's default sampling,
// the same timeout -- so there are deliberately no generation settings here.
// Pointers mark settings whose zero value is a meaningful choice.
type OllamaConfig struct {
	Managed *bool  `yaml:"managed" json:"managed"`
	Image   string `yaml:"image" json:"image"`
	BaseURL string `yaml:"baseUrl" json:"baseUrl,omitempty"`
	// AgentBaseURL is how the consumer agent, inside its Kind cluster, reaches
	// an unmanaged Ollama (baseUrl is how the harness on the host does). Unused
	// when managed: the harness then attaches its container to the Kind network.
	AgentBaseURL     string        `yaml:"agentBaseUrl" json:"agentBaseUrl,omitempty"`
	Model            string        `yaml:"model" json:"model"`
	PullIfMissing    *bool         `yaml:"pullIfMissing" json:"pullIfMissing"`
	PullTimeout      time.Duration `yaml:"pullTimeout" json:"pullTimeout"`
	ModelCacheVolume string        `yaml:"modelCacheVolume" json:"modelCacheVolume"`
	RemoveModelCache bool          `yaml:"removeModelCache" json:"removeModelCache"`
	GPU              bool          `yaml:"gpu" json:"gpu"`
	// Timeout bounds one decision. The consumer agent gets the same value as
	// --ollama-timeout, so the harness's decisions and the agent's own obey
	// the same limit.
	Timeout       time.Duration `yaml:"timeout" json:"timeout"`
	WarmupTimeout time.Duration `yaml:"warmupTimeout" json:"warmupTimeout"`
}

// IsManaged reports whether this run provisions its own Ollama container.
func (o OllamaConfig) IsManaged() bool { return o.Managed == nil || *o.Managed }

// AgentPathConfig controls the check of the consumer agent's own ConsumerChoice
// path: a manual reservation that the agent places after asking the LLM itself.
type AgentPathConfig struct {
	Enabled *bool `yaml:"enabled" json:"enabled"`
	// CPU and Memory size the manual reservation; they must fit in one chunk.
	CPU     string        `yaml:"cpu" json:"cpu"`
	Memory  string        `yaml:"memory" json:"memory"`
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
}

// IsEnabled reports whether each scenario also exercises the agent's own path.
func (a AgentPathConfig) IsEnabled() bool { return a.Enabled == nil || *a.Enabled }

// FallbackConfig selects what happens when the model's choice fails validation.
type FallbackConfig struct {
	Mode string `yaml:"mode" json:"mode"`
}

// ProviderProfile is what one provider advertises during the run.
type ProviderProfile struct {
	CarbonIntensity int               `yaml:"carbonIntensity" json:"carbonIntensity"`
	Prices          map[string]string `yaml:"prices" json:"prices"`
	Capacity        CapacitySpec      `yaml:"capacity" json:"capacity"`
}

// CapacitySpec is the provider's advertised capacity cap.
type CapacitySpec struct {
	CPU    string `yaml:"cpu" json:"cpu"`
	Memory string `yaml:"memory" json:"memory"`
}

// Chunks is how many standard chunks this capacity yields.
func (c CapacitySpec) Chunks() (int32, error) {
	cpu, err := resource.ParseQuantity(c.CPU)
	if err != nil {
		return 0, fmt.Errorf("cpu %q: %w", c.CPU, err)
	}
	mem, err := resource.ParseQuantity(c.Memory)
	if err != nil {
		return 0, fmt.Errorf("memory %q: %w", c.Memory, err)
	}
	return int32(min(cpu.MilliValue()/chunkMilliCPU, mem.Value()/chunkMemBytes)), nil
}

// Scenario is one natural-language request plus the rule used to judge the
// resulting choice afterwards.
type Scenario struct {
	Name             string    `yaml:"name" json:"name"`
	UserRequest      string    `yaml:"userRequest" json:"userRequest"`
	Criterion        Criterion `yaml:"criterion" json:"criterion"`
	ReferenceWeights *Weights  `yaml:"referenceWeights" json:"referenceWeights,omitempty"`
}

// Criterion is the post-hoc, transparent acceptance rule for a scenario.
type Criterion struct {
	Type            string  `yaml:"type" json:"type"`
	MaxCarbonRank   int     `yaml:"maxCarbonRank" json:"maxCarbonRank,omitempty"`
	MaxDistanceRank int     `yaml:"maxDistanceRank" json:"maxDistanceRank,omitempty"`
	WorstQuantile   float64 `yaml:"worstQuantile" json:"worstQuantile,omitempty"`
}

// Weights for the optional reference score (min-max normalised, lower is better).
type Weights struct {
	Carbon   float64 `yaml:"carbon" json:"carbon"`
	Distance float64 `yaml:"distance" json:"distance"`
	Cost     float64 `yaml:"cost" json:"cost"`
}

// NeedsDistance reports whether judging this scenario requires the consumer's location.
func (s Scenario) NeedsDistance() bool {
	return s.Criterion.Type == criterionProximity || s.Criterion.Type == criterionBalanced ||
		(s.ReferenceWeights != nil && s.ReferenceWeights.Distance > 0)
}

func (a AgentPathConfig) validate(o OllamaConfig) []error {
	if !a.IsEnabled() {
		return nil
	}
	var errs []error
	if !o.IsManaged() && o.AgentBaseURL == "" {
		errs = append(errs, errors.New("ollama.agentBaseUrl is required for agentPath when ollama.managed is false: "+
			"the consumer agent runs inside Kind and cannot reach the host's baseUrl"))
	}
	// One chunk at most: the manual-reservation controller does not split a
	// request across chunks, so a larger one would stay Pending forever.
	for _, r := range []struct {
		name, value string
		max         int64
		milli       bool
	}{{"cpu", a.CPU, chunkMilliCPU, true}, {"memory", a.Memory, chunkMemBytes, false}} {
		q, err := resource.ParseQuantity(r.value)
		if err != nil {
			errs = append(errs, fmt.Errorf("agentPath.%s %q: %w", r.name, r.value, err))
			continue
		}
		v := q.Value()
		if r.milli {
			v = q.MilliValue()
		}
		if v <= 0 || v > r.max {
			errs = append(errs, fmt.Errorf("agentPath.%s %q must be positive and fit in one chunk (2 CPU / 4Gi)",
				r.name, r.value))
		}
	}
	return errs
}

// loadConfig reads the shared schema and the consumerChoice section from one file.
func loadConfig(path string) (*testlib.AutoConfig, *ChoiceConfig, error) {
	shared, err := testlib.LoadAutoConfig(path)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read config: %w", err)
	}
	var file struct {
		ProviderRegions []string  `yaml:"providerRegions"`
		ConsumerChoice  yaml.Node `yaml:"consumerChoice"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, nil, fmt.Errorf("parse config: %w", err)
	}
	cc, err := decodeChoiceSection(&file.ConsumerChoice)
	if err != nil {
		return nil, nil, fmt.Errorf("parse consumerChoice section: %w", err)
	}
	cc.applyDefaults()
	if err := cc.validate(shared, len(file.ProviderRegions) > 0); err != nil {
		return nil, nil, fmt.Errorf("invalid consumerChoice config: %w", err)
	}
	return shared, &cc, nil
}

// decodeChoiceSection decodes the consumerChoice section strictly: an unknown
// key is an error. A misspelt setting -- or one this suite no longer supports,
// such as a sampling option -- must not be silently ignored while the run
// behaves as if it had never been written.
func decodeChoiceSection(node *yaml.Node) (ChoiceConfig, error) {
	var cc ChoiceConfig
	if node.Kind == 0 { // no consumerChoice section at all
		return cc, nil
	}
	raw, err := yaml.Marshal(node)
	if err != nil {
		return cc, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cc); err != nil && !errors.Is(err, io.EOF) {
		return cc, err
	}
	return cc, nil
}

func (c *ChoiceConfig) applyDefaults() {
	if c.Repetitions <= 0 {
		c.Repetitions = 1
	}
	if c.MetadataTimeout <= 0 {
		c.MetadataTimeout = 5 * time.Minute
	}
	if c.PolicyTimeout <= 0 {
		c.PolicyTimeout = 2 * time.Minute
	}
	o := &c.Ollama
	if o.Image == "" {
		o.Image = defaultOllamaImage
	}
	if o.BaseURL == "" {
		o.BaseURL = "http://localhost:11434"
	}
	if o.Model == "" {
		o.Model = defaultModel
	}
	if o.PullIfMissing == nil {
		yes := true
		o.PullIfMissing = &yes
	}
	if o.PullTimeout <= 0 {
		o.PullTimeout = 30 * time.Minute
	}
	if o.ModelCacheVolume == "" {
		o.ModelCacheVolume = "federation-autoscaler-ollama"
	}
	if o.Timeout <= 0 {
		o.Timeout = agentDefaultOllamaTimeout
	}
	if o.WarmupTimeout <= 0 {
		o.WarmupTimeout = 5 * time.Minute
	}
	a := &c.AgentPath
	if a.CPU == "" {
		a.CPU = "1"
	}
	if a.Memory == "" {
		a.Memory = "1Gi"
	}
	if a.Timeout <= 0 {
		a.Timeout = 10 * time.Minute
	}
	if c.Fallback.Mode == "" {
		c.Fallback.Mode = fallbackAgentDefault
	}
	for i := range c.Scenarios {
		cr := &c.Scenarios[i].Criterion
		if cr.Type == "" {
			cr.Type = criterionNone
		}
		if cr.MaxCarbonRank <= 0 {
			cr.MaxCarbonRank = 2
		}
		if cr.MaxDistanceRank <= 0 {
			cr.MaxDistanceRank = 3
		}
		if cr.WorstQuantile <= 0 {
			cr.WorstQuantile = 0.25
		}
	}
}

func (c *ChoiceConfig) validate(shared *testlib.AutoConfig, regionsExplicit bool) error {
	var errs []error
	if c.Mode != requiredMode {
		errs = append(errs, fmt.Errorf("mode is %q; this suite only runs with mode: %s", c.Mode, requiredMode))
	}
	if shared.Consumers < 1 {
		errs = append(errs, errors.New("consumers must be at least 1"))
	}
	if !regionsExplicit {
		// testlib would invent random regions, which would make distances --
		// and so every proximity result -- differ from run to run.
		errs = append(errs, errors.New("providerRegions must be listed explicitly: distances are part of the experiment"))
	}
	if len(c.ProviderProfiles) != shared.Providers {
		errs = append(errs, fmt.Errorf("providerProfiles has %d entries, providers is %d: they must match one to one",
			len(c.ProviderProfiles), shared.Providers))
	}
	errs = append(errs, c.validateProviders(shared)...)
	errs = append(errs, c.validateModelSettings()...)
	errs = append(errs, c.validateScenarios()...)
	return errors.Join(errs...)
}

// validateProviders checks the provider catalogue: the regions it places
// providers in, and each profile's carbon, prices and capacity.
func (c *ChoiceConfig) validateProviders(shared *testlib.AutoConfig) []error {
	var errs []error
	seen := map[string]int{}
	for i, region := range shared.ProviderRegions {
		if _, _, _, ok := testlib.RegionLocation(region); !ok {
			errs = append(errs, fmt.Errorf("providerRegions[%d] %q is not a known region", i, region))
		}
		if prev, dup := seen[region]; dup {
			// Carbon intensity is set per region, so two providers sharing one
			// could not advertise the two different values their profiles ask for.
			errs = append(errs, fmt.Errorf("providerRegions[%d] repeats %q from providerRegions[%d]", i, region, prev))
		}
		seen[region] = i
	}
	for i, p := range c.ProviderProfiles {
		if p.CarbonIntensity <= 0 {
			errs = append(errs, fmt.Errorf("providerProfiles[%d].carbonIntensity must be positive", i))
		}
		if p.Prices["cpu"] == "" || p.Prices["memory"] == "" {
			// The Broker only computes a per-chunk cost when every chunk
			// resource is priced; a partial price list silently means "no cost".
			errs = append(errs, fmt.Errorf("providerProfiles[%d].prices needs both cpu and memory", i))
		}
		for name, v := range p.Prices {
			if _, err := resource.ParseQuantity(v); err != nil {
				errs = append(errs, fmt.Errorf("providerProfiles[%d].prices.%s %q: %w", i, name, v, err))
			}
		}
		if chunks, err := p.Capacity.Chunks(); err != nil {
			errs = append(errs, fmt.Errorf("providerProfiles[%d].capacity: %w", i, err))
		} else if chunks < 1 {
			errs = append(errs, fmt.Errorf("providerProfiles[%d].capacity is below one chunk (2 CPU / 4Gi)", i))
		}
	}
	return errs
}

// validateModelSettings checks how the model is reached and what happens
// when it cannot be used.
func (c *ChoiceConfig) validateModelSettings() []error {
	var errs []error
	switch c.Fallback.Mode {
	case fallbackFail, fallbackAgentDefault, fallbackCheapest, fallbackLowestCarbon:
	default:
		errs = append(errs, fmt.Errorf("fallback.mode %q is not one of %s, %s, %s, %s",
			c.Fallback.Mode, fallbackFail, fallbackAgentDefault, fallbackCheapest, fallbackLowestCarbon))
	}
	if !c.Ollama.IsManaged() && c.Ollama.BaseURL == "" {
		errs = append(errs, errors.New("ollama.baseUrl is required when ollama.managed is false"))
	}
	errs = append(errs, c.AgentPath.validate(c.Ollama)...)
	return errs
}

// validateScenarios checks each scenario's name, request and success criterion.
func (c *ChoiceConfig) validateScenarios() []error {
	var errs []error
	if len(c.Scenarios) == 0 {
		errs = append(errs, errors.New("at least one scenario is required"))
	}
	names := map[string]bool{}
	for i, s := range c.Scenarios {
		if !scenarioName.MatchString(s.Name) {
			errs = append(errs, fmt.Errorf("scenarios[%d].name %q must be 1-20 lowercase letters, digits or '-'", i, s.Name))
		}
		if names[s.Name] {
			errs = append(errs, fmt.Errorf("scenario name %q is used twice", s.Name))
		}
		names[s.Name] = true
		if s.UserRequest == "" {
			errs = append(errs, fmt.Errorf("scenarios[%d].userRequest is empty", i))
		}
		switch s.Criterion.Type {
		case criterionEco, criterionProximity, criterionBalanced, criterionNone:
		default:
			errs = append(errs, fmt.Errorf("scenarios[%d].criterion.type %q is not one of eco, proximity, balanced, none",
				i, s.Criterion.Type))
		}
		if q := s.Criterion.WorstQuantile; q >= 1 {
			errs = append(errs, fmt.Errorf("scenarios[%d].criterion.worstQuantile %v must be below 1", i, q))
		}
		if w := s.ReferenceWeights; w != nil &&
			(w.Carbon < 0 || w.Distance < 0 || w.Cost < 0 || w.Carbon+w.Distance+w.Cost == 0) {
			errs = append(errs, fmt.Errorf("scenarios[%d].referenceWeights must be non-negative and not all zero", i))
		}
	}
	return errs
}
