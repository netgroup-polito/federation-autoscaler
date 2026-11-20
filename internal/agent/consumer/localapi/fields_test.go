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

package localapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// cityResp is an unmasked ConsumerChoice list of three providers in three
// cities: p1 Cuneo, p2 Turin, p3 Frankfurt.
func cityResp() brokerapi.NodeGroupListResponse {
	view := func(id, city, region string) brokerapi.NodeGroupView {
		return brokerapi.NodeGroupView{
			ID: "ng-" + id + "-standard", ProviderClusterID: id, Type: brokerv1alpha1.ChunkTypeStandard, MaxSize: 3,
			Topology: &brokerv1alpha1.Topology{City: city, Region: region},
		}
	}
	return brokerapi.NodeGroupListResponse{
		AppliedPlacement: autoscalingv1alpha1.PlacementStrategyConsumerChoice,
		NodeGroups: []brokerapi.NodeGroupView{
			view("p1", "Cuneo", "PIE"), view("p2", "Turin", "PIE"), view("p3", "Frankfurt", "HE"),
		},
	}
}

// fieldsTestServer is a local API in the "fields" job, over cityResp.
func fieldsTestServer(t *testing.T, llm *ollama.Client) (*Server, *httptest.Server) {
	t.Helper()
	return jobTestServer(t, llm, ollama.JobFields)
}

// jobTestServer is a local API in the given AI job, over cityResp.
func jobTestServer(t *testing.T, llm *ollama.Client, job string) (*Server, *httptest.Server) {
	t.Helper()
	fb := newFakeBroker(t)
	fb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(cityResp())
	})
	s, err := New(Options{
		BindAddress:  "127.0.0.1:0",
		Client:       fb.buildClient(t),
		LocalClient:  newFakeKubeClient(),
		Namespace:    ccNamespace,
		OllamaClient: llm,
		AIJob:        job,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// In the "fields" job the model is asked only for the request's fields (its
// prompt is the intent prompt, with no provider in it), and the agent keeps
// growable the provider the fields select.
func TestFieldsJob_ModelReadsTheTextCodeRanks(t *testing.T) {
	llm := &fakeOllama{answer: func(int) string {
		return `{"places":["Turin"],"exclude":[],"renewableOnly":false,"prefer":null,"minGuaranteedHours":null}`
	}}
	s, ts := fieldsTestServer(t, llm.client(t))

	q := NodeGroupQuery{Prompt: "I need 2 VMs in Turin."}
	eventuallyQ(t, ts, q, onlyGrowable("p2"))

	if got := llm.prompt(0); got != "Request: I need 2 VMs in Turin." || strings.Contains(got, "p1") {
		t.Errorf("the model must get only the request; prompt:\n%s", got)
	}
	sel := s.LastSelection()
	if sel == nil || sel.Source != SelectionSourceAI || sel.Understood != "in Turin" || len(sel.Ranked) != 1 {
		t.Errorf("report %+v", sel)
	}
}

// Without a usable answer the text is read with fixed patterns, against the
// providers' own cities: "not in Frankfurt" drops p3.
func TestFieldsJob_FallsBackToTheText(t *testing.T) {
	llm := &fakeOllama{answer: func(int) string { return "not json" }}
	s, ts := fieldsTestServer(t, llm.client(t))

	q := NodeGroupQuery{Prompt: "1 VM, not in Frankfurt"}
	eventuallyQ(t, ts, q, func(hr map[string]int32) bool { return hr["p1"]+hr["p2"] > 0 })

	sel := s.LastSelection()
	if sel == nil || sel.Source != SelectionSourceFallback || sel.Understood != "not in Frankfurt" {
		t.Fatalf("report %+v", sel)
	}
	for _, id := range sel.Ranked {
		if id == "p3" {
			t.Errorf("excluded p3 is ranked: %v", sel.Ranked)
		}
	}
}

func TestNew_RejectsUnknownAIJob(t *testing.T) {
	fb := newFakeBroker(t)
	if _, err := New(Options{
		BindAddress: "127.0.0.1:0", Client: fb.buildClient(t), LocalClient: newFakeKubeClient(), AIJob: "both",
	}); err == nil {
		t.Error("an unknown AIJob must be rejected")
	}
}

// In the "rank" job, "not in X" is applied in code before the model sees the
// list: even a model that ranks the excluded provider first cannot pick it.
func TestRankJob_ExclusionAppliedInCode(t *testing.T) {
	llm := &fakeOllama{answer: func(int) string { return ranking("p3", "p1", "p2") }}
	s, ts := jobTestServer(t, llm.client(t), ollama.JobRank)

	q := NodeGroupQuery{Prompt: "1 VM, not in Frankfurt"}
	eventuallyQ(t, ts, q, onlyGrowable("p1"))

	if got := llm.prompt(0); strings.Contains(got, `"p3"`) {
		t.Errorf("the excluded provider must not be sent to the model; prompt:\n%s", got)
	}
	sel := s.LastSelection()
	if sel == nil || sel.Understood != "not in Frankfurt" {
		t.Fatalf("report %+v", sel)
	}
	for _, id := range sel.Ranked {
		if id == "p3" {
			t.Errorf("excluded p3 is ranked: %v", sel.Ranked)
		}
	}
}
