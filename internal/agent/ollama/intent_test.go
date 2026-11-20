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

package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// demoGroups mirrors the demo: Cuneo (near, dearest), Turin (renewable,
// greenest), Frankfurt (far, cheapest, dirtiest).
func demoGroups() []brokerapi.NodeGroupView {
	cuneo := makeNodeGroup("provider-1", 3, 0, floatPtr(0.076), floatPtr(178), "PIE")
	cuneo.Topology.City, cuneo.Topology.Latitude, cuneo.Topology.Longitude = "Cuneo", 44.3845, 7.5427
	cuneo.UnitPrices = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("30m"), corev1.ResourceMemory: resource.MustParse("4m")}
	cuneo.CarbonForecast = []float64{200, 200, 200, 200, 200, 200}
	turin := makeNodeGroup("provider-2", 3, 0, floatPtr(0.064), floatPtr(150), "PIE")
	turin.Topology.City, turin.Topology.Latitude, turin.Topology.Longitude = "Turin", 45.0703, 7.6869
	turin.Renewable = true
	turin.UnitPrices = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m"), corev1.ResourceMemory: resource.MustParse("3500u")}
	turin.CarbonForecast = []float64{100, 100, 100, 100, 100, 100}
	frankfurt := makeNodeGroup("provider-3", 3, 0, floatPtr(0.028), floatPtr(316), "HE")
	frankfurt.Topology.City, frankfurt.Topology.Latitude, frankfurt.Topology.Longitude = "Frankfurt", 50.1109, 8.6821
	frankfurt.UnitPrices = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("2m")}
	return []brokerapi.NodeGroupView{cuneo, turin, frankfurt}
}

var mondovi = &Location{Latitude: 44.3912, Longitude: 7.8197, Region: "PIE"}

func TestParseIntent(t *testing.T) {
	known := KnownPlaces(demoGroups())
	for _, c := range []struct {
		text string
		want Intent
	}{
		{"I need 2 VMs in Turin.", Intent{Places: []string{"Turin"}}},
		{"I need 1 VM, not in Frankfurt.", Intent{Exclude: []string{"Frankfurt"}}},
		{"I need a VM from the cheapest provider.", Intent{Prefer: PreferPrice}},
		{"4 CPUs, cheapest per CPU", Intent{Prefer: PreferPricePerCPU}},
		{"I need 2 VMs on renewable energy only.", Intent{RenewableOnly: true}},
		{"1 VM, the greenest over the next hours", Intent{Prefer: PreferGreenForecast}},
		{"the greenest please", Intent{Prefer: PreferGreen}},
		{"4 CPUs and 8 GB with the lowest latency to us.", Intent{Prefer: PreferLatency}},
		{"I want the clusters closest to me", Intent{Prefer: PreferNearest}},
		{"2 VMs for 3 hours", Intent{MinGuaranteedHours: 3}},
		{"1 VM for two days", Intent{MinGuaranteedHours: 48}},
		{"I need 3 VMs as Kubernetes workers, at most 3 ms from us.", Intent{}},
		{"he wants 1 VM in HE", Intent{Places: []string{"HE"}}},
	} {
		if got := ParseIntent(c.text, known); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseIntent(%q) = %+v, want %+v", c.text, got, c.want)
		}
	}
}

func TestIntentResponse_DropsPlacesNotInText(t *testing.T) {
	prefer := PreferPrice
	in, err := intentResponse{
		Places:  []string{"Turin", "Milan"},
		Exclude: []string{"Germany"},
		Prefer:  &prefer,
	}.toIntent("2 cheap VMs in Turin")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in.Places, []string{"Turin"}) || len(in.Exclude) != 0 || in.Prefer != PreferPrice {
		t.Errorf("got %+v", in)
	}
	bad := "fastest-ever"
	if _, err := (intentResponse{Prefer: &bad}).toIntent("x"); err == nil {
		t.Error("an unknown preference must be rejected")
	}
}

func TestRankByIntent(t *testing.T) {
	groups := demoGroups()
	rtts := map[string]float64{"provider-1": 1.3, "provider-2": 5.2} // provider-3 not measured
	for _, c := range []struct {
		name string
		in   Intent
		want []string
	}{
		{"place", Intent{Places: []string{"Turin"}}, []string{"provider-2"}},
		{"region", Intent{Places: []string{"pie"}, Prefer: PreferPrice}, []string{"provider-2", "provider-1"}},
		{"exclude", Intent{Exclude: []string{"Frankfurt"}, Prefer: PreferPrice}, []string{"provider-2", "provider-1"}},
		{"renewable only", Intent{RenewableOnly: true}, []string{"provider-2"}},
		{"price", Intent{Prefer: PreferPrice}, []string{"provider-3", "provider-2", "provider-1"}},
		{"price per CPU", Intent{Prefer: PreferPricePerCPU}, []string{"provider-3", "provider-2", "provider-1"}},
		{"green", Intent{Prefer: PreferGreen}, []string{"provider-2", "provider-1", "provider-3"}},
		{"forecast, falling back to carbon now", Intent{Prefer: PreferGreenForecast}, []string{"provider-2", "provider-1", "provider-3"}},
		{"latency, unmeasured last", Intent{Prefer: PreferLatency}, []string{"provider-1", "provider-2", "provider-3"}},
		{"nearest", Intent{Prefer: PreferNearest}, []string{"provider-1", "provider-2", "provider-3"}},
		{"no preference = fallback order", Intent{}, []string{"provider-3", "provider-2", "provider-1"}},
		{"nothing matches", Intent{Places: []string{"Paris"}}, []string{}},
	} {
		got := RankByIntent(groups, c.in, rtts, mondovi)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// A provider whose guarantee covers the asked duration comes first; a full
// provider stays in the ranking (the caller's fit check skips it).
func TestRankByIntent_GuaranteeAndFullProviders(t *testing.T) {
	groups := demoGroups()
	groups[0].GuaranteedSeconds = 4 * 3600 // Cuneo guarantees 4 h
	groups[2].MaxSize = 0                  // Frankfurt is full
	got := RankByIntent(groups, Intent{MinGuaranteedHours: 3, Prefer: PreferPrice}, nil, nil)
	// Cuneo covers 3 h; then by price. Frankfurt is full but listed: its
	// place does not matter, the fit check skips it.
	if !reflect.DeepEqual(got, []string{"provider-1", "provider-3", "provider-2"}) {
		t.Errorf("got %v", got)
	}
}

func TestExtractIntent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollamaRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.System != IntentSystemPrompt || req.Prompt != "Request: 2 VMs in Turin, cheapest" {
			t.Errorf("unexpected request %+v", req)
		}
		_ = json.NewEncoder(w).Encode(ollamaResponse{
			Response: `{"places":["Turin","Milan"],"exclude":[],"renewableOnly":false,"prefer":"price","minGuaranteedHours":null}`,
			Done:     true,
		})
	}))
	defer srv.Close()

	res := UnderstandIntent(context.Background(), New(srv.URL, "qwen"), "2 VMs in Turin, cheapest", nil)
	if res.Source != AmountSourceAI || res.AIError != "" {
		t.Fatalf("result %+v", res)
	}
	if !reflect.DeepEqual(res.Intent, Intent{Places: []string{"Turin"}, Prefer: PreferPrice}) {
		t.Errorf("intent %+v (Milan is not in the text and must be dropped)", res.Intent)
	}
	if got := res.Intent.String(); got != "in Turin, prefer price" {
		t.Errorf("String() = %q", got)
	}
}

// Without the model, UnderstandIntent reads the text itself.
func TestUnderstandIntent_FallsBackToText(t *testing.T) {
	res := UnderstandIntent(context.Background(), nil, "1 VM not in Frankfurt", KnownPlaces(demoGroups()))
	if res.Source != AmountSourceText || !reflect.DeepEqual(res.Intent.Exclude, []string{"Frankfurt"}) {
		t.Errorf("result %+v", res)
	}
}

func TestValidJob(t *testing.T) {
	for job, want := range map[string]bool{"": true, JobRank: true, JobFields: true, "both": false} {
		if got := ValidJob(job); got != want {
			t.Errorf("ValidJob(%q) = %v, want %v", job, got, want)
		}
	}
}

var _ = brokerv1alpha1.ChunkTypeStandard
