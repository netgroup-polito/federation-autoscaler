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
