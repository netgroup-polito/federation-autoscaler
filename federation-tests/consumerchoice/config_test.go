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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netgroup-polito/federation-autoscaler/federation-tests/testlib"
)

// Every shipped config must load: a broken one would only surface after
// minutes of cluster creation on the test server.
func TestShippedConfigsLoad(t *testing.T) {
	paths, err := filepath.Glob("configs/*.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no configs found: %v", err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			shared, cc, err := loadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if shared.Consumers != 1 || shared.Providers != 9 {
				t.Errorf("standard topology is 1 consumer / 9 providers, got %d / %d", shared.Consumers, shared.Providers)
			}
			if cc.Ollama.Model != "llama3.2" || !cc.Ollama.IsManaged() {
				t.Errorf("unexpected Ollama settings: %+v", cc.Ollama)
			}
			if !cc.AgentPath.IsEnabled() {
				t.Error("shipped configs must exercise the agent's own path")
			}
		})
	}
}

func TestDefaultConfigIsTheSmokeTest(t *testing.T) {
	_, cc, err := loadConfig("configs/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cc.Repetitions != 1 || len(cc.Scenarios) != 1 || cc.Scenarios[0].Criterion.Type != criterionEco {
		t.Errorf("default.yaml should be one eco-oriented decision: %d reps, %+v", cc.Repetitions, cc.Scenarios)
	}
}

func validBase(t *testing.T) (*testlib.AutoConfig, *ChoiceConfig) {
	t.Helper()
	shared, cc, err := loadConfig("configs/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return shared, cc
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testlib.AutoConfig, *ChoiceConfig)
		want   string
	}{
		{"another policy's mode",
			func(_ *testlib.AutoConfig, c *ChoiceConfig) { c.Mode = "reserve" }, "only runs with mode: consumerchoice"},
		{"profile count mismatch",
			func(_ *testlib.AutoConfig, c *ChoiceConfig) { c.ProviderProfiles = c.ProviderProfiles[:3] },
			"must match one to one"},
		{"unknown region",
			func(s *testlib.AutoConfig, _ *ChoiceConfig) { s.ProviderRegions[4] = "ATLANTIS" }, "not a known region"},
		{"duplicate region",
			func(s *testlib.AutoConfig, _ *ChoiceConfig) { s.ProviderRegions[4] = s.ProviderRegions[0] }, "repeats"},
		{"partial prices",
			func(_ *testlib.AutoConfig, c *ChoiceConfig) { delete(c.ProviderProfiles[0].Prices, "memory") },
			"needs both cpu and memory"},
		{"sub-chunk capacity",
			func(_ *testlib.AutoConfig, c *ChoiceConfig) { c.ProviderProfiles[0].Capacity.CPU = "1000m" }, "below one chunk"},
		{"bad fallback",
			func(_ *testlib.AutoConfig, c *ChoiceConfig) { c.Fallback.Mode = "random" }, "fallback.mode"},
		{"bad scenario name",
			func(_ *testlib.AutoConfig, c *ChoiceConfig) { c.Scenarios[0].Name = "Eco Oriented" }, "lowercase"},
		{"bad criterion",
			func(_ *testlib.AutoConfig, c *ChoiceConfig) { c.Scenarios[0].Criterion.Type = "fastest" }, "criterion.type"},
		{"empty request",
			func(_ *testlib.AutoConfig, c *ChoiceConfig) { c.Scenarios[0].UserRequest = "" }, "userRequest is empty"},
		{"no scenarios",
			func(_ *testlib.AutoConfig, c *ChoiceConfig) { c.Scenarios = nil }, "at least one scenario"},
		{"agent path with an unmanaged Ollama the agent cannot reach",
			func(_ *testlib.AutoConfig, c *ChoiceConfig) { no := false; c.Ollama.Managed = &no }, "agentBaseUrl"},
		{"agent path request larger than one chunk",
			func(_ *testlib.AutoConfig, c *ChoiceConfig) { c.AgentPath.Memory = "6Gi" }, "fit in one chunk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shared, cc := validBase(t)
			tc.mutate(shared, cc)
			err := cc.validate(shared, true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateRequiresExplicitRegions(t *testing.T) {
	shared, cc := validBase(t)
	err := cc.validate(shared, false)
	if err == nil || !strings.Contains(err.Error(), "providerRegions must be listed explicitly") {
		t.Errorf("auto-generated regions must be rejected, got %v", err)
	}
}

func TestLoadConfigRejectsForeignSuiteConfig(t *testing.T) {
	// A comparative-eco config has no consumerChoice section: it must be
	// refused, never run as if it were a ConsumerChoice experiment.
	dir := t.TempDir()
	path := filepath.Join(dir, "eco.yaml")
	data := "consumers: 1\nproviders: 2\nproviderRegions: [LOM, CH]\nexperiment:\n  mode: reserve\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Errorf("a config without consumerChoice.mode must be rejected, got %v", err)
	}
}

// The model is queried as the agent queries it, so generation settings no longer
// exist -- and a config that still sets one must be refused, not have it
// silently ignored while the reader believes it applied.
func TestLoadConfigRejectsUnknownKeys(t *testing.T) {
	data, err := os.ReadFile("configs/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The third key is misspelled on purpose: a typo must be refused
	// just like a removed key, instead of being silently ignored.
	for _, key := range []string{"temperature: 0", "jsonSchema: true", "tempreature: 0"} { //nolint:misspell // deliberate typo, see above
		t.Run(key, func(t *testing.T) {
			text := strings.Replace(string(data), "    warmupTimeout: 5m", "    warmupTimeout: 5m\n    "+key, 1)
			path := filepath.Join(t.TempDir(), "cfg.yaml")
			if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "not found") {
				t.Errorf("unknown key %q must be rejected, got %v", key, err)
			}
		})
	}
}

func TestCapacityChunks(t *testing.T) {
	for spec, want := range map[CapacitySpec]int32{
		{CPU: "4000m", Memory: "8Gi"}:  2,
		{CPU: "8", Memory: "16Gi"}:     4,
		{CPU: "6000m", Memory: "8Gi"}:  2, // memory is the binding limit
		{CPU: "2000m", Memory: "32Gi"}: 1, // CPU is
	} {
		if got, err := spec.Chunks(); err != nil || got != want {
			t.Errorf("%+v -> %d chunks (err %v), want %d", spec, got, err, want)
		}
	}
}
