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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// --- helpers ---

// Provider IDs shared by the fixtures below.
const (
	providerOne = "provider-1"
	providerTwo = "provider-2"
)

func makeNodeGroup(providerID string, maxSize, reserved int32, cost *float64, carbon *float64, region string) brokerapi.NodeGroupView {
	ng := brokerapi.NodeGroupView{
		ID:                providerID + "-standard",
		ProviderClusterID: providerID,
		Type:              "standard",
		MinSize:           0,
		MaxSize:           maxSize,
		CurrentReserved:   reserved,
		ChunkResources: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	}
	if cost != nil {
		q := resource.NewMilliQuantity(int64(*cost*1000), resource.DecimalSI)
		ng.Cost = q
	}
	ng.CarbonIntensity = carbon
	if region != "" {
		ng.Topology = &brokerv1alpha1.Topology{
			Region:    region,
			Latitude:  48.85,
			Longitude: 2.35,
		}
	}
	return ng
}

func floatPtr(v float64) *float64 { return &v }

// --- Select tests ---

func TestSelect_ValidResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := ollamaResponse{
			Response: `{"providerId": "provider-2"}`,
			Done:     true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model")
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup(providerOne, 5, 2, floatPtr(0.10), floatPtr(100), "IDF"),
		makeNodeGroup(providerTwo, 5, 1, floatPtr(0.05), floatPtr(40), "QC"),
	}

	ranked, err := c.Select(context.Background(), "cheapest provider", groups)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ranked) == 0 || ranked[0] != providerTwo {
		t.Fatalf("expected [provider-2, ...], got %v", ranked)
	}
}

func TestSelect_InvalidProviderID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := ollamaResponse{
			Response: `{"providerId": "nonexistent"}`,
			Done:     true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model")
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup(providerOne, 5, 2, nil, nil, ""),
		makeNodeGroup(providerTwo, 5, 0, nil, nil, ""),
	}

	_, err := c.Select(context.Background(), "any", groups)
	if err == nil {
		t.Fatal("expected error for unknown provider ID, got nil")
	}
}

func TestSelect_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := ollamaResponse{
			Response: `not json at all`,
			Done:     true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model")
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup(providerOne, 5, 0, nil, nil, ""),
		makeNodeGroup(providerTwo, 5, 0, nil, nil, ""),
	}

	_, err := c.Select(context.Background(), "any", groups)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestSelect_EmptyProviderID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := ollamaResponse{
			Response: `{"providerId": ""}`,
			Done:     true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model")
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup(providerOne, 5, 0, nil, nil, ""),
		makeNodeGroup(providerTwo, 5, 0, nil, nil, ""),
	}

	_, err := c.Select(context.Background(), "any", groups)
	if err == nil {
		t.Fatal("expected error for empty providerId, got nil")
	}
}

func TestSelect_SingleProvider_SkipsAI(t *testing.T) {
	// Server should NOT be called when there's only one provider.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model")
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup(providerOne, 5, 0, nil, nil, ""),
	}

	ranked, err := c.Select(context.Background(), "any", groups)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ranked) != 1 || ranked[0] != providerOne {
		t.Fatalf("expected [provider-1], got %v", ranked)
	}
	if called {
		t.Fatal("AI should not have been called for single provider")
	}
}

func TestSelect_NoCapacity(t *testing.T) {
	c := New("http://localhost:11434", "test-model")
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup(providerOne, 5, 5, nil, nil, ""), // full
	}

	_, err := c.Select(context.Background(), "any", groups)
	if err == nil {
		t.Fatal("expected error for no capacity, got nil")
	}
}

func TestSelect_OllamaDown(t *testing.T) {
	// Use a server that immediately closes the connection.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model")
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup(providerOne, 5, 0, nil, nil, ""),
		makeNodeGroup(providerTwo, 5, 0, nil, nil, ""),
	}

	_, err := c.Select(context.Background(), "any", groups)
	if err == nil {
		t.Fatal("expected error for Ollama down, got nil")
	}
}

// --- DeterministicFallback tests ---

func TestDeterministicFallback_CheapestWins(t *testing.T) {
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup("expensive", 5, 0, floatPtr(0.50), nil, ""),
		makeNodeGroup("cheap", 5, 0, floatPtr(0.05), nil, ""),
	}

	ranked, ok := DeterministicFallback(groups)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(ranked) == 0 || ranked[0] != "cheap" {
		t.Fatalf("expected cheap first, got %v", ranked)
	}
}

func TestDeterministicFallback_CarbonTiebreaker(t *testing.T) {
	cost := 0.10
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup("dirty", 5, 0, &cost, floatPtr(200), ""),
		makeNodeGroup("green", 5, 0, &cost, floatPtr(30), ""),
	}

	ranked, ok := DeterministicFallback(groups)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(ranked) == 0 || ranked[0] != "green" {
		t.Fatalf("expected green first, got %v", ranked)
	}
}

func TestDeterministicFallback_MostCapacityLast(t *testing.T) {
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup("small", 3, 0, nil, nil, ""),
		makeNodeGroup("big", 10, 0, nil, nil, ""),
	}

	ranked, ok := DeterministicFallback(groups)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(ranked) == 0 || ranked[0] != "big" {
		t.Fatalf("expected big (most capacity) first, got %v", ranked)
	}
}

func TestDeterministicFallback_NoCapacity(t *testing.T) {
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup("full", 5, 5, nil, nil, ""),
	}

	_, ok := DeterministicFallback(groups)
	if ok {
		t.Fatal("expected ok=false for no available capacity")
	}
}

func TestDeterministicFallback_PricedPreferred(t *testing.T) {
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup("unpriced", 10, 0, nil, nil, ""),
		makeNodeGroup("priced", 5, 0, floatPtr(0.10), nil, ""),
	}

	ranked, ok := DeterministicFallback(groups)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(ranked) == 0 || ranked[0] != "priced" {
		t.Fatalf("expected priced provider first, got %v", ranked)
	}
}

func TestSelect_RankedList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := ollamaResponse{
			Response: `{"rankedList": ["provider-2", "provider-1"], "providerId": "provider-2"}`,
			Done:     true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model")
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup(providerOne, 5, 0, floatPtr(0.10), nil, ""),
		makeNodeGroup(providerTwo, 5, 0, floatPtr(0.05), nil, ""),
	}

	ranked, err := c.Select(context.Background(), "cheapest", groups)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ranked) != 2 || ranked[0] != providerTwo || ranked[1] != providerOne {
		t.Fatalf("expected [provider-2 provider-1], got %v", ranked)
	}
}

func TestSelect_RankedListFiltersUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := ollamaResponse{
			Response: `{"rankedList": ["bogus", "provider-1"], "providerId": "bogus"}`,
			Done:     true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model")
	// Two candidates, not one: with a single provider Select skips the model
	// entirely, so a one-provider fixture would pass without ever reaching the
	// filtering this test is named after.
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup(providerOne, 5, 0, nil, nil, ""),
		makeNodeGroup(providerTwo, 5, 0, nil, nil, ""),
	}

	ranked, err := c.Select(context.Background(), "any", groups)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ranked) != 1 || ranked[0] != providerOne {
		t.Fatalf("expected [provider-1] after filtering unknown IDs, got %v", ranked)
	}
}

// A small model can loop and repeat IDs until the JSON closes; the ranking is
// the first occurrence of each.
func TestSelect_RankedListDropsRepeatedIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := ollamaResponse{
			Response: `{"rankedList": ["provider-2", "provider-1", "provider-2", "provider-1", "provider-2"], ` +
				`"providerId": "provider-2"}`,
			Done: true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model")
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup(providerOne, 5, 0, nil, nil, ""),
		makeNodeGroup(providerTwo, 5, 0, nil, nil, ""),
	}

	ranked, err := c.Select(context.Background(), "any", groups)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ranked) != 2 || ranked[0] != providerTwo || ranked[1] != providerOne {
		t.Fatalf("expected [provider-2 provider-1] without repeats, got %v", ranked)
	}
}

// --- NodeGroupViewToProviderInfo tests ---

func TestNodeGroupViewToProviderInfo_BasicMapping(t *testing.T) {
	cost := 0.076
	carbon := 102.5
	ng := makeNodeGroup("prov-1", 5, 2, &cost, &carbon, "IDF")

	info := NodeGroupViewToProviderInfo(ng)

	if info.ProviderID != "prov-1" {
		t.Fatalf("expected prov-1, got %q", info.ProviderID)
	}
	if info.AvailableChunks != 3 {
		t.Fatalf("expected 3 available chunks, got %d", info.AvailableChunks)
	}
	if info.CPUPerChunk != "2" {
		t.Fatalf("expected CPU 2, got %q", info.CPUPerChunk)
	}
	if info.CostPerChunk == nil {
		t.Fatal("expected non-nil cost")
	}
	if info.CarbonIntensity == nil || *info.CarbonIntensity != 102.5 {
		t.Fatalf("expected carbon 102.5, got %v", info.CarbonIntensity)
	}
	if info.Region != "IDF" {
		t.Fatalf("expected region IDF, got %q", info.Region)
	}
}

func TestNodeGroupViewToProviderInfo_NilTopology(t *testing.T) {
	ng := makeNodeGroup("prov-2", 3, 0, nil, nil, "")

	info := NodeGroupViewToProviderInfo(ng)

	if info.Region != "" {
		t.Fatalf("expected empty region, got %q", info.Region)
	}
	if info.CostPerChunk != nil {
		t.Fatalf("expected nil cost, got %v", info.CostPerChunk)
	}
}

// --- BuildUserPrompt tests ---

func TestBuildUserPrompt_ContainsRequest(t *testing.T) {
	providers := []ProviderInfo{
		{ProviderID: "p1", AvailableChunks: 3},
	}
	prompt := BuildUserPrompt("give me the cheapest", nil, providers)

	if !contains(prompt, "give me the cheapest") {
		t.Fatal("prompt should contain the user request")
	}
	if !contains(prompt, "AVAILABLE PROVIDERS") {
		t.Fatal("prompt should contain AVAILABLE PROVIDERS section")
	}
	if !contains(prompt, `"p1"`) {
		t.Fatal("prompt should contain provider ID")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- OpenAI-style API ---

// openAIServer answers /v1/chat/completions with content and records the
// path and request it got.
func openAIServer(t *testing.T, content string, gotPath *string, gotReq *openAIRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(gotReq); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen","choices":[{"message":{"role":"assistant","content":` +
			mustJSON(t, content) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":30}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSelect_OpenAIAPI(t *testing.T) {
	var path string
	var req openAIRequest
	srv := openAIServer(t, `{"rankedList":["provider-2","provider-1"]}`, &path, &req)

	c := NewWithOptions(srv.URL, "qwen", Options{API: APIOpenAI})
	trace, err := c.SelectDetailed(context.Background(), "cheapest", []brokerapi.NodeGroupView{
		makeNodeGroup(providerOne, 3, 0, nil, nil, ""),
		makeNodeGroup(providerTwo, 3, 0, nil, nil, ""),
	})
	if err != nil {
		t.Fatalf("SelectDetailed: %v", err)
	}
	if path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", path)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" ||
		req.Messages[0].Content != SystemPrompt || req.ResponseFormat["type"] != "json_object" || req.Model != "qwen" ||
		req.ReasoningEffort != "none" || req.ChatTemplateKwargs["enable_thinking"] != false {
		t.Errorf("unexpected request: %+v", req)
	}
	if len(trace.Ranked) != 2 || trace.Ranked[0] != providerTwo {
		t.Errorf("ranked = %v, want provider-2 first", trace.Ranked)
	}
	if trace.API != APIOpenAI || trace.Envelope.EvalCount != 30 || trace.Envelope.DoneReason != "stop" {
		t.Errorf("trace: api=%q envelope=%+v", trace.API, trace.Envelope)
	}
}

// A base URL that already ends in /v1 (KubeAI's /openai/v1) is not doubled.
func TestSelect_OpenAIAPI_BaseWithV1(t *testing.T) {
	var path string
	var req openAIRequest
	srv := openAIServer(t, `{"rankedList":["provider-1","provider-2"]}`, &path, &req)

	c := NewWithOptions(srv.URL+"/openai/v1/", "qwen", Options{API: APIOpenAI})
	if _, err := c.Select(context.Background(), "any", []brokerapi.NodeGroupView{
		makeNodeGroup(providerOne, 3, 0, nil, nil, ""),
		makeNodeGroup(providerTwo, 3, 0, nil, nil, ""),
	}); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if path != "/openai/v1/chat/completions" {
		t.Errorf("path = %q, want /openai/v1/chat/completions", path)
	}
}

func TestSelect_OpenAIAPI_NoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	c := NewWithOptions(srv.URL, "qwen", Options{API: APIOpenAI})
	_, err := c.Select(context.Background(), "any", []brokerapi.NodeGroupView{
		makeNodeGroup(providerOne, 3, 0, nil, nil, ""),
		makeNodeGroup(providerTwo, 3, 0, nil, nil, ""),
	})
	var selErr *SelectError
	if !errors.As(err, &selErr) || selErr.Kind != ErrKindEnvelopeDecode {
		t.Errorf("err = %v, want kind envelope_decode", err)
	}
}

// The amount reader goes through the same API.
func TestExtractAmount_OpenAIAPI(t *testing.T) {
	var path string
	var req openAIRequest
	srv := openAIServer(t, `{"nodes":3,"maxLatencyMs":3}`, &path, &req)

	c := NewWithOptions(srv.URL, "qwen", Options{API: APIOpenAI})
	_, amt, err := c.ExtractAmount(context.Background(), "3 VMs, at most 3 ms from us")
	if err != nil {
		t.Fatalf("ExtractAmount: %v", err)
	}
	if path != "/v1/chat/completions" || req.Messages[0].Content != AmountSystemPrompt {
		t.Errorf("path=%q system=%q", path, req.Messages[0].Content)
	}
	if amt.Nodes != 3 {
		t.Errorf("amount = %+v, want 3 nodes", amt)
	}
}

func TestValidAPI(t *testing.T) {
	for api, want := range map[string]bool{"": true, APIOllama: true, APIOpenAI: true, "vllm": false} {
		if got := ValidAPI(api); got != want {
			t.Errorf("ValidAPI(%q) = %v, want %v", api, got, want)
		}
	}
}

// Ollama's own API always asks the model not to think.
func TestSelect_OllamaAPI_ThinkingOff(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(ollamaResponse{Response: `{"rankedList":["provider-1","provider-2"]}`, Done: true})
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "qwen3:4b").Select(context.Background(), "any", []brokerapi.NodeGroupView{
		makeNodeGroup(providerOne, 3, 0, nil, nil, ""), makeNodeGroup(providerTwo, 3, 0, nil, nil, ""),
	}); err != nil {
		t.Fatal(err)
	}
	if think, ok := got["think"]; !ok || think != false {
		t.Errorf("think = %v (present %v), want false", think, ok)
	}
}
