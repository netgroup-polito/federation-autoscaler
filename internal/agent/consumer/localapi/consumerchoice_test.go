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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

const ccNamespace = "federation-autoscaler-system"

// fakeOllama is an Ollama /api/generate endpoint that answers call n with
// answer(n) and records every prompt. When hold is set, the first call does
// not answer until hold is closed -- a model still thinking.
type fakeOllama struct {
	answer func(call int) string
	hold   chan struct{}

	mu      sync.Mutex
	prompts []string
}

func (f *fakeOllama) client(t *testing.T) *ollama.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.prompts = append(f.prompts, req.Prompt)
		call := len(f.prompts)
		f.mu.Unlock()
		if call == 1 && f.hold != nil {
			<-f.hold
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"response": f.answer(call), "done": true})
	}))
	t.Cleanup(srv.Close)
	return ollama.New(srv.URL, "test-model")
}

func (f *fakeOllama) prompt(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.prompts) {
		return ""
	}
	return f.prompts[i]
}

func ranking(ids ...string) string {
	out, _ := json.Marshal(map[string]any{"rankedList": ids, "providerId": ids[0]})
	return string(out)
}

func consumerChoicePolicy(prompt string) *autoscalingv1alpha1.ConsumerPolicy {
	return &autoscalingv1alpha1.ConsumerPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ccNamespace},
		Spec: autoscalingv1alpha1.ConsumerPolicySpec{
			Placement:  autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyConsumerChoice},
			UserPrompt: prompt,
		},
	}
}

// aiTestServer serves the Broker's unmasked ConsumerChoice list and delegates
// the choice to llm, with the consumer located in Milan.
func aiTestServer(t *testing.T, fb *fakeBroker, kube ctrlclient.Client, llm *ollama.Client) *httptest.Server {
	t.Helper()
	return aiTestServerWithLogger(t, fb, kube, llm, logr.Logger{})
}

func aiTestServerWithLogger(t *testing.T, fb *fakeBroker, kube ctrlclient.Client, llm *ollama.Client,
	logger logr.Logger) *httptest.Server {
	t.Helper()
	fb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(unmaskedResp(autoscalingv1alpha1.PlacementStrategyConsumerChoice))
	})
	s, err := New(Options{
		BindAddress:  "127.0.0.1:0",
		Client:       fb.buildClient(t),
		LocalClient:  kube,
		Namespace:    ccNamespace,
		OllamaClient: llm,
		ConsumerLocation: func() *ollama.Location {
			return &ollama.Location{Latitude: 45.4642, Longitude: 9.19, Region: "LOM"}
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// eventually polls GET /local/nodegroups until done reports true, failing the
// test when every response keeps failing it for 5 s. check sees each response
// on the way and may flag one that must never happen.
func eventually(t *testing.T, ts *httptest.Server, check func(hr map[string]int32), done func(hr map[string]int32) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		hr := headroom(getNodeGroups(t, ts))
		check(hr)
		if done(hr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not reached; last head-room %+v", hr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func onlyGrowable(id string) func(map[string]int32) bool {
	return func(hr map[string]int32) bool {
		for p, h := range hr {
			if (p == id) != (h > 0) {
				return false
			}
		}
		return true
	}
}

func TestConsumerChoiceAI_MasksWhileThinkingThenGrowsTheChoice(t *testing.T) {
	llm := &fakeOllama{answer: func(int) string { return ranking("p3", "p1", "p2") }, hold: make(chan struct{})}
	ts := aiTestServer(t, newFakeBroker(t), newFakeKubeClient(consumerChoicePolicy("close to me")), llm.client(t))

	for p, h := range headroom(getNodeGroups(t, ts)) {
		if h != 0 {
			t.Errorf("while the model is thinking nothing may be growable; %s has %d", p, h)
		}
	}
	close(llm.hold)
	eventually(t, ts, func(map[string]int32) {}, onlyGrowable("p3"))

	prompt := llm.prompt(0)
	if !strings.Contains(prompt, "close to me") || !strings.Contains(prompt, `"region":"LOM"`) {
		t.Errorf("the model must receive the request and the consumer's location:\n%s", prompt)
	}
}

// A ranking computed for an old prompt must never be applied to the new one.
func TestConsumerChoiceAI_DiscardsRankingForSupersededPrompt(t *testing.T) {
	llm := &fakeOllama{
		answer: func(call int) string {
			if call == 1 {
				return ranking("p1", "p2", "p3") // the answer to the old prompt
			}
			return ranking("p3", "p2", "p1")
		},
		hold: make(chan struct{}),
	}
	kube := newFakeKubeClient(consumerChoicePolicy("the dirtiest please"))
	ts := aiTestServer(t, newFakeBroker(t), kube, llm.client(t))

	_ = getNodeGroups(t, ts) // starts the selection for the old prompt, which stays pending

	policy := &autoscalingv1alpha1.ConsumerPolicy{}
	if err := kube.Get(context.Background(), ctrlclient.ObjectKey{Namespace: ccNamespace, Name: "default"}, policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.UserPrompt = "the greenest please"
	if err := kube.Update(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	_ = getNodeGroups(t, ts) // sees the new prompt while the old selection is still running
	close(llm.hold)

	eventually(t, ts, func(hr map[string]int32) {
		if hr["p1"] > 0 {
			t.Fatalf("the old prompt's choice p1 was applied to the new prompt: %+v", hr)
		}
	}, onlyGrowable("p3"))
	if got := llm.prompt(1); !strings.Contains(got, "the greenest please") {
		t.Errorf("a fresh selection must be made for the new prompt; second prompt:\n%s", got)
	}
}

// capturedLog collects the JSON log lines a funcr logger writes.
type capturedLog struct {
	mu    sync.Mutex
	lines []map[string]any
}

func (c *capturedLog) logger() logr.Logger {
	return funcr.NewJSON(func(obj string) {
		var line map[string]any
		if json.Unmarshal([]byte(obj), &line) == nil {
			c.mu.Lock()
			c.lines = append(c.lines, line)
			c.mu.Unlock()
		}
	}, funcr.Options{})
}

// finished returns the first "selection finished" line, or nil.
func (c *capturedLog) finished() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if l["msg"] == SelectionFinishedMessage {
			return l
		}
	}
	return nil
}

// Every decision leaves one Info line saying whether the model decided or the
// fallback did, with what the model answered. Operators read it, and so does the
// end-to-end suite to learn what the agent's own LLM call chose.
func TestConsumerChoiceAI_LogsEachDecision(t *testing.T) {
	waitFinished := func(t *testing.T, logs *capturedLog, ts *httptest.Server) map[string]any {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for logs.finished() == nil {
			if time.Now().After(deadline) {
				t.Fatal("no selection-finished log line")
			}
			_ = getNodeGroups(t, ts)
			time.Sleep(20 * time.Millisecond)
		}
		return logs.finished()
	}

	t.Run("the model decided", func(t *testing.T) {
		logs := &capturedLog{}
		llm := &fakeOllama{answer: func(int) string { return ranking("p3", "p1", "p2") }}
		ts := aiTestServerWithLogger(t, newFakeBroker(t), newFakeKubeClient(consumerChoicePolicy("close to me")),
			llm.client(t), logs.logger())

		line := waitFinished(t, logs, ts)
		ranked, _ := line["ranked"].([]any)
		if line["source"] != SelectionSourceAI || line["prompt"] != "close to me" || len(ranked) == 0 || ranked[0] != "p3" {
			t.Errorf("log line = %v; want source ai, the prompt, and p3 first", line)
		}
		if raw, _ := line["rawResponse"].(string); !strings.Contains(raw, "p3") {
			t.Errorf("the model's raw answer must be logged: %v", line)
		}
	})

	t.Run("the model could not be used", func(t *testing.T) {
		logs := &capturedLog{}
		broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "model not loaded", http.StatusInternalServerError)
		}))
		t.Cleanup(broken.Close)
		ts := aiTestServerWithLogger(t, newFakeBroker(t), newFakeKubeClient(consumerChoicePolicy("anything")),
			ollama.New(broken.URL, "m"), logs.logger())

		line := waitFinished(t, logs, ts)
		if line["source"] != SelectionSourceFallback || line["errorKind"] != string(ollama.ErrKindHTTPStatus) {
			t.Errorf("log line = %v; want source fallback with errorKind http_status", line)
		}
	})
}

// A model that ranks only some providers must not stall the Cluster Autoscaler
// once those fill up: the rest stay reachable in the deterministic order.
func TestConsumerChoiceAI_IncompleteRankingIsCompleted(t *testing.T) {
	llm := &fakeOllama{answer: func(int) string { return ranking("p1") }}
	fb := newFakeBroker(t)
	ts := aiTestServer(t, fb, newFakeKubeClient(consumerChoicePolicy("anything")), llm.client(t))

	eventually(t, ts, func(map[string]int32) {}, onlyGrowable("p1"))

	// p1 fills up while the ranking is still cached.
	fb.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		resp := unmaskedResp(autoscalingv1alpha1.PlacementStrategyConsumerChoice)
		resp.NodeGroups[0].CurrentReserved = resp.NodeGroups[0].MaxSize
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(resp)
	})
	// DeterministicFallback puts p2 (lowest carbon) next, ahead of p3 (no carbon).
	if hr := headroom(getNodeGroups(t, ts)); hr["p2"] != 3 || hr["p3"] != 0 {
		t.Errorf("after p1 filled, the deterministic runner-up p2 must be growable; got %+v", hr)
	}
}
