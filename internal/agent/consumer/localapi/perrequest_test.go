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
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/latency"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

const closest = "I want the clusters closest to me."

func TestNodeGroupQuery_RoundTrip(t *testing.T) {
	in := NodeGroupQuery{
		Bound:  &autoscalingv1alpha1.LatencyBound{MaxLatencyMs: 3},
		Prompt: closest,
		Need: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
		},
	}
	v := EncodeNodeGroupQuery(in)
	out, err := DecodeNodeGroupQuery(v)
	if err != nil {
		t.Fatalf("decode %v: %v", v, err)
	}
	if out.Bound == nil || out.Bound.MaxLatencyMs != 3 || out.Prompt != closest {
		t.Errorf("round trip = %+v, want bound 3 ms and the prompt", out)
	}
	if cpu, mem := out.Need[corev1.ResourceCPU], out.Need[corev1.ResourceMemory]; cpu.Cmp(resource.MustParse("4")) != 0 ||
		mem.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Errorf("need = %v, want 4 CPU and 8Gi", out.Need)
	}
	if EncodeNodeGroupQuery(NodeGroupQuery{}) != nil {
		t.Error("the zero query must encode to nil (the plain read)")
	}
	for name, raw := range map[string]string{
		"bad cpu":      QueryNeedCPU + "=lots",
		"negative mem": QueryNeedMemory + "=-1Gi",
		"long prompt":  QueryPrompt + "=" + strings.Repeat("x", maxQueryPromptLen+1),
	} {
		if _, err := DecodeNodeGroupQuery(urlValues(raw)); err == nil {
			t.Errorf("%s: %q must be rejected", name, raw)
		}
	}
}

// sizedResp is a ConsumerChoice list whose providers have chunks of 2 CPU /
// 4Gi: p1 with free standard chunks, p2 with free standard chunks, p3 with
// one free GPU chunk only.
func sizedResp(p1Free, p2Free int32) brokerapi.NodeGroupListResponse {
	std := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")}
	gpu := corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi"),
		corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
	}
	return brokerapi.NodeGroupListResponse{
		AppliedPlacement: autoscalingv1alpha1.PlacementStrategyConsumerChoice,
		NodeGroups: []brokerapi.NodeGroupView{
			{ID: "p1/standard", ProviderClusterID: "p1", Type: brokerv1alpha1.ChunkTypeStandard, MaxSize: p1Free, ChunkResources: std},
			{ID: "p2/standard", ProviderClusterID: "p2", Type: brokerv1alpha1.ChunkTypeStandard, MaxSize: p2Free, ChunkResources: std},
			{ID: "p3/gpu", ProviderClusterID: "p3", Type: brokerv1alpha1.ChunkTypeGPU, MaxSize: 1, ChunkResources: gpu},
		},
	}
}

func serveResp(fb *fakeBroker, resp brokerapi.NodeGroupListResponse) {
	fb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(resp)
	})
}

// getQuery calls GET /local/nodegroups with q.
func getQuery(t *testing.T, ts *httptest.Server, q NodeGroupQuery) map[string]int32 {
	t.Helper()
	url := ts.URL + "/local/nodegroups"
	if v := EncodeNodeGroupQuery(q); v != nil {
		url += "?" + v.Encode()
	}
	resp, err := ts.Client().Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out brokerapi.NodeGroupListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return headroom(out)
}

// eventuallyQ polls with q until done holds, failing after 5 s.
func eventuallyQ(t *testing.T, ts *httptest.Server, q NodeGroupQuery, done func(map[string]int32) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		hr := getQuery(t, ts, q)
		if done(hr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not reached; last head-room %+v", hr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A ResourceRequest's own text is ranked by the AI even when the consumer has
// no ConsumerChoice policy, and the decision is reported as per-request.
func TestPerRequestPrompt_RankedWithoutConsumerChoicePolicy(t *testing.T) {
	llm := &fakeOllama{answer: func(int) string { return ranking("p2", "p1") }}
	fb := newFakeBroker(t)
	s, ts := newAITestAPI(t, fb, newFakeKubeClient(), llm.client(t), logr.Logger{})
	serveResp(fb, sizedResp(3, 3))

	eventuallyQ(t, ts, NodeGroupQuery{Prompt: closest}, onlyGrowable("p2"))
	if !strings.Contains(llm.prompt(0), closest) {
		t.Errorf("the model must receive the request's own text:\n%s", llm.prompt(0))
	}
	got := s.LastSelection()
	if got == nil || !got.PerRequest || got.Prompt != closest {
		t.Errorf("LastSelection = %+v, want the per-request decision", got)
	}
}

// In central mode under another policy the Broker already masked the list;
// the per-request text cannot re-rank it, so the view passes through and the
// model is not called.
func TestPerRequestPrompt_CentralModeOtherPolicyPassesThrough(t *testing.T) {
	llm := &fakeOllama{answer: func(int) string { return ranking("p2") }}
	fb := newFakeBroker(t)
	_, ts := newAITestAPI(t, fb, newFakeKubeClient(), llm.client(t), logr.Logger{})
	priceView := sizedResp(3, 0) // Price left only p1 growable
	priceView.AppliedPlacement = autoscalingv1alpha1.PlacementStrategyPrice
	serveResp(fb, priceView)

	if hr := getQuery(t, ts, NodeGroupQuery{Prompt: closest}); hr["p1"] != 3 || hr["p2"] != 0 {
		t.Errorf("head-room = %+v, want the Broker's Price view unchanged", hr)
	}
	if llm.prompt(0) != "" {
		t.Error("the model must not be called")
	}
}

// Each text has its own ranking: two texts, two model calls, two answers; the
// first text is then served from its cache without a third call.
func TestPerRequestPrompt_RankingCachedPerText(t *testing.T) {
	llm := &fakeOllama{answer: func(call int) string {
		if call == 1 {
			return ranking("p1", "p2")
		}
		return ranking("p2", "p1")
	}}
	fb := newFakeBroker(t)
	_, ts := newAITestAPI(t, fb, newFakeKubeClient(), llm.client(t), logr.Logger{})
	serveResp(fb, sizedResp(3, 3))

	eventuallyQ(t, ts, NodeGroupQuery{Prompt: "text A"}, onlyGrowable("p1"))
	eventuallyQ(t, ts, NodeGroupQuery{Prompt: "text B"}, onlyGrowable("p2"))
	if hr := getQuery(t, ts, NodeGroupQuery{Prompt: "text A"}); !onlyGrowable("p1")(hr) {
		t.Errorf("text A must keep its own ranking, head-room %+v", hr)
	}
	llm.mu.Lock()
	calls := len(llm.prompts)
	llm.mu.Unlock()
	if calls != 2 {
		t.Errorf("model calls = %d, want 2 (one per text)", calls)
	}
}

// The first-ranked provider that cannot hold the request is skipped: too
// little free room, or the wrong chunk type.
func TestPerRequestPrompt_SizeAwareChoice(t *testing.T) {
	llm := &fakeOllama{answer: func(int) string { return ranking("p1", "p2", "p3") }}
	fb := newFakeBroker(t)
	_, ts := newAITestAPI(t, fb, newFakeKubeClient(), llm.client(t), logr.Logger{})
	serveResp(fb, sizedResp(1, 2)) // p1: 1 free chunk, p2: 2 free chunks

	four := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")} // 2 chunks
	eventuallyQ(t, ts, NodeGroupQuery{Prompt: closest, Need: four}, onlyGrowable("p2"))

	two := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")} // 1 chunk: p1 fits
	if hr := getQuery(t, ts, NodeGroupQuery{Prompt: closest, Need: two}); !onlyGrowable("p1")(hr) {
		t.Errorf("a 1-chunk request must go to the first-ranked p1, head-room %+v", hr)
	}

	gpu := corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")}
	if hr := getQuery(t, ts, NodeGroupQuery{Prompt: closest, Need: gpu}); !onlyGrowable("p3")(hr) {
		t.Errorf("a GPU request must skip the standard-only providers, head-room %+v", hr)
	}

	tooBig := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("16")}
	for p, h := range getQuery(t, ts, NodeGroupQuery{Prompt: closest, Need: tooBig}) {
		if h != 0 {
			t.Errorf("nobody can hold 16 CPU, yet %s has head-room %d", p, h)
		}
	}
}

func urlValues(raw string) map[string][]string {
	k, v, _ := strings.Cut(raw, "=")
	return map[string][]string{k: {v}}
}

// While the model is answering, the masked list says why (the AI is
// choosing), so a ResourceRequest does not report "no capacity"; once the
// ranking is in, the note is gone.
func TestWaitingNoteWhileTheAIChooses(t *testing.T) {
	llm := &fakeOllama{answer: func(int) string { return ranking("p2", "p1") }, hold: make(chan struct{})}
	fb := newFakeBroker(t)
	_, ts := newAITestAPI(t, fb, newFakeKubeClient(), llm.client(t), logr.Logger{})
	serveResp(fb, sizedResp(3, 3))

	if got := getQueryResp(t, ts, NodeGroupQuery{Prompt: closest}); got.Waiting != brokerapi.WaitingAIChoosing {
		t.Errorf("while the model answers: waiting = %q, want %q", got.Waiting, brokerapi.WaitingAIChoosing)
	}
	close(llm.hold)
	eventuallyQ(t, ts, NodeGroupQuery{Prompt: closest}, onlyGrowable("p2"))
	if got := getQueryResp(t, ts, NodeGroupQuery{Prompt: closest}); got.Waiting != "" {
		t.Errorf("after the decision: waiting = %q, want empty", got.Waiting)
	}
}

// The ConsumerPolicy switched to ConsumerChoice but the Broker still applies
// the old policy: the note says so.
func TestWaitingNoteWhileThePolicyTakesEffect(t *testing.T) {
	llm := &fakeOllama{answer: func(int) string { return ranking("p1") }}
	fb := newFakeBroker(t)
	_, ts := newAITestAPI(t, fb, newFakeKubeClient(consumerChoicePolicy(closest)), llm.client(t), logr.Logger{})
	old := sizedResp(3, 0)
	old.AppliedPlacement = autoscalingv1alpha1.PlacementStrategyPrice
	serveResp(fb, old)

	if got := getQueryResp(t, ts, NodeGroupQuery{}); got.Waiting != brokerapi.WaitingPolicyApplying {
		t.Errorf("waiting = %q, want %q", got.Waiting, brokerapi.WaitingPolicyApplying)
	}
}

func getQueryResp(t *testing.T, ts *httptest.Server, q NodeGroupQuery) brokerapi.NodeGroupListResponse {
	t.Helper()
	url := ts.URL + "/local/nodegroups"
	if v := EncodeNodeGroupQuery(q); v != nil {
		url += "?" + v.Encode()
	}
	resp, err := ts.Client().Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out brokerapi.NodeGroupListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A decision for a reservation reports the size it asked for; a second
// reservation with the same text (served from the cache) updates it.
func TestPerRequestPrompt_ReportsTheRequestedSize(t *testing.T) {
	llm := &fakeOllama{answer: func(int) string { return ranking("p2", "p1") }}
	fb := newFakeBroker(t)
	s, ts := newAITestAPI(t, fb, newFakeKubeClient(), llm.client(t), logr.Logger{})
	serveResp(fb, sizedResp(3, 3))

	four := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}
	eventuallyQ(t, ts, NodeGroupQuery{Prompt: closest, Need: four}, onlyGrowable("p2"))
	if got := s.LastSelection(); got == nil || got.Requested != "4 CPU, 8Gi memory" {
		t.Fatalf("LastSelection = %+v, want requested 4 CPU, 8Gi memory", got)
	}
	two := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
	getQuery(t, ts, NodeGroupQuery{Prompt: closest, Need: two})
	if got := s.LastSelection(); got.Requested != "2 CPU" {
		t.Errorf("after a cached decision for 2 CPU: requested = %q", got.Requested)
	}
}

// measuringProber also measures without picking, like *latency.Prober, and
// records which endpoints it was asked to measure.
type measuringProber struct {
	rtts  map[string]float64 // by endpoint
	asked []string
}

func (m *measuringProber) MeasureAndPick(context.Context, []latency.Candidate) latency.Result {
	return latency.Result{}
}

func (m *measuringProber) Measure(_ context.Context, endpoints []string) map[string]float64 {
	m.asked = append(m.asked, endpoints...)
	out := map[string]float64{}
	for _, ep := range endpoints {
		out[ep] = m.rtts[ep]
	}
	return out
}

// For the AI, only the nearest providers (by distance from the consumer) are
// measured, like the Latency policy's shortlist; an unreachable one is left
// out.
func TestMeasureNearest(t *testing.T) {
	at := func(id string, lat, lon float64, ep string) brokerapi.NodeGroupView {
		return brokerapi.NodeGroupView{ID: id + "/std", ProviderClusterID: id, MaxSize: 3, ProbeEndpoint: ep,
			Topology: &brokerv1alpha1.Topology{Latitude: lat, Longitude: lon}}
	}
	groups := []brokerapi.NodeGroupView{
		at("frankfurt", 50.1, 8.7, "10.0.0.4:30100"),
		at("turin", 45.07, 7.69, "10.0.0.2:30100"),
		at("paris", 48.86, 2.35, "10.0.0.5:30100"),
		at("cuneo", 44.38, 7.54, "10.0.0.1:30100"),
		at("nowhere", 44.4, 7.5, ""), // no probe endpoint
	}
	pr := &measuringProber{rtts: map[string]float64{
		"10.0.0.1:30100": 2.1, "10.0.0.2:30100": 5.2, "10.0.0.5:30100": math.Inf(1),
	}}
	s, err := New(Options{
		BindAddress: "127.0.0.1:0", Client: newFakeBroker(t).buildClient(t), LocalClient: newFakeKubeClient(),
		Namespace: ccNamespace, Prober: pr,
		ConsumerLocation: func() *ollama.Location { return &ollama.Location{Latitude: 44.38, Longitude: 7.54} },
	})
	if err != nil {
		t.Fatal(err)
	}
	got := s.measureNearest(context.Background(), groups)
	if len(pr.asked) != 3 || strings.Contains(strings.Join(pr.asked, ","), "10.0.0.4") {
		t.Errorf("measured %v, want the 3 nearest (Cuneo, Turin, Paris), not Frankfurt", pr.asked)
	}
	if got["cuneo"] != 2.1 || got["turin"] != 5.2 || len(got) != 2 {
		t.Errorf("rtts = %v, want Cuneo and Turin (Paris unreachable)", got)
	}
}

// A latency limit on the request is shown on the dashboard with the size.
func TestRequestedSizeWithLimit(t *testing.T) {
	need := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("6"), corev1.ResourceMemory: resource.MustParse("12Gi")}
	if got := requestedSize(need, &autoscalingv1alpha1.LatencyBound{MaxLatencyMs: 3}); got != "6 CPU, 12Gi memory, max 3 ms" {
		t.Errorf("requestedSize = %q", got)
	}
	if got := requestedSize(nil, nil); got != "" {
		t.Errorf("nothing given: requestedSize = %q, want empty", got)
	}
}

// When no provider has room (all full, or all excluded by a latency limit),
// the AI is not asked, the list says nothing about choosing, and asking again
// does not call the model either.
func TestNoGrowableProviderSkipsTheAI(t *testing.T) {
	llm := &fakeOllama{answer: func(int) string { return ranking("p1") }}
	fb := newFakeBroker(t)
	_, ts := newAITestAPI(t, fb, newFakeKubeClient(), llm.client(t), logr.Logger{})
	serveResp(fb, sizedResp(0, 0)) // p1, p2 full; p3 is GPU-only and also full below
	full := sizedResp(0, 0)
	full.NodeGroups[2].CurrentReserved = full.NodeGroups[2].MaxSize
	serveResp(fb, full)

	for range 3 {
		if got := getQueryResp(t, ts, NodeGroupQuery{Prompt: closest}); got.Waiting != "" {
			t.Fatalf("waiting = %q, want empty (nothing to choose from)", got.Waiting)
		}
	}
	if llm.prompt(0) != "" {
		t.Error("the model must not be asked when no provider has room")
	}
}

// An empty decision (no provider could be ranked) is cached like any other,
// so polling does not re-run the model every time.
func TestEmptyDecisionIsCached(t *testing.T) {
	llm := &fakeOllama{answer: func(int) string { return "not json" }}
	fb := newFakeBroker(t)
	s, ts := newAITestAPI(t, fb, newFakeKubeClient(), llm.client(t), logr.Logger{})
	// p1 and p2 have one free chunk each, the request needs two: the model
	// is asked once (it answers garbage, so the fallback ranks), nothing
	// fits, and the decision must still be reused.
	serveResp(fb, sizedResp(1, 1))
	four := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}
	eventuallyQ(t, ts, NodeGroupQuery{Prompt: "anything", Need: four},
		func(map[string]int32) bool { return s.LastSelection() != nil })
	for range 5 {
		if hr := getQuery(t, ts, NodeGroupQuery{Prompt: "anything", Need: four}); hr["p1"] != 0 || hr["p2"] != 0 {
			t.Fatalf("nothing fits 4 CPU, yet head-room %v", hr)
		}
	}
	llm.mu.Lock()
	calls := len(llm.prompts)
	llm.mu.Unlock()
	if calls != 1 {
		t.Errorf("model calls = %d, want 1 (the decision is reused)", calls)
	}
}
