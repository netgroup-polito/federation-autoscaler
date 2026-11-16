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
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// amountServer is a fake Ollama answering every call with answer, and
// recording the last system prompt and prompt it received.
func amountServer(t *testing.T, answer string, status int) (*Client, *ollamaRequest) {
	t.Helper()
	var last ollamaRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&last)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("boom"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaResponse{Response: answer, Done: true})
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "test-model"), &last
}

func wantAmount(t *testing.T, got Amount, cpu, mem string, gpu int64) {
	t.Helper()
	if cpu == "" {
		if !got.CPU.IsZero() {
			t.Errorf("cpu = %s, want not stated", got.CPU.String())
		}
	} else if got.CPU.Cmp(resource.MustParse(cpu)) != 0 {
		t.Errorf("cpu = %s, want %s", got.CPU.String(), cpu)
	}
	if mem == "" {
		if !got.Memory.IsZero() {
			t.Errorf("memory = %s, want not stated", got.Memory.String())
		}
	} else if got.Memory.Cmp(resource.MustParse(mem)) != 0 {
		t.Errorf("memory = %s, want %s", got.Memory.String(), mem)
	}
	if got.GPU != gpu {
		t.Errorf("gpu = %d, want %d", got.GPU, gpu)
	}
}

func TestExtractAmount_ReadsTheModelAnswer(t *testing.T) {
	c, last := amountServer(t, `{"cpu": 4, "memoryGiB": 8, "gpu": null}`, http.StatusOK)
	trace, a, err := c.ExtractAmount(context.Background(), "I need 4 CPUs and 8 GB close to me")
	if err != nil {
		t.Fatalf("ExtractAmount: %v", err)
	}
	wantAmount(t, a, "4", "8Gi", 0)
	if last.System != AmountSystemPrompt || !strings.Contains(last.Prompt, "4 CPUs and 8 GB") || last.Format != jsonMode {
		t.Errorf("request sent = %+v, want the amount prompt and the user's text in JSON mode", *last)
	}
	if trace.RawResponse == "" || trace.FinishedAt.IsZero() {
		t.Errorf("trace not filled: %+v", trace)
	}
}

func TestExtractAmount_FractionsAndMiB(t *testing.T) {
	c, _ := amountServer(t, `{"cpu": 0.5, "memoryGiB": 0.5, "gpu": 1}`, http.StatusOK)
	_, a, err := c.ExtractAmount(context.Background(), "half a core, 512 MB and a GPU")
	if err != nil {
		t.Fatalf("ExtractAmount: %v", err)
	}
	wantAmount(t, a, "500m", "512Mi", 1)
}

func TestExtractAmount_RejectsBadAnswers(t *testing.T) {
	for name, answer := range map[string]string{
		"not json":       `four CPUs please`,
		"negative cpu":   `{"cpu": -2, "memoryGiB": null, "gpu": null}`,
		"huge memory":    `{"cpu": 2, "memoryGiB": 1000000, "gpu": null}`,
		"fractional gpu": `{"cpu": null, "memoryGiB": null, "gpu": 1.5}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := amountServer(t, answer, http.StatusOK)
			trace, a, err := c.ExtractAmount(context.Background(), "whatever")
			if err == nil {
				t.Fatalf("answer %q must be rejected, got %v", answer, a)
			}
			if trace.ErrorKind != ErrKindInvalidJSON || trace.Error == "" {
				t.Errorf("trace = kind %q error %q, want invalid_json with a message", trace.ErrorKind, trace.Error)
			}
		})
	}
}

func TestUnderstandAmount_UsesTheModelWhenValid(t *testing.T) {
	c, _ := amountServer(t, `{"cpu": 6, "memoryGiB": 12, "gpu": null}`, http.StatusOK)
	res := UnderstandAmount(context.Background(), c, "6 cores and 12 gigs")
	if res.Source != AmountSourceAI || res.Trace == nil || res.AIError != "" {
		t.Fatalf("result = %+v, want source ai with a trace", res)
	}
	wantAmount(t, res.Amount, "6", "12Gi", 0)
}

func TestUnderstandAmount_ModelSaysNothingStated(t *testing.T) {
	c, _ := amountServer(t, `{"cpu": null, "memoryGiB": null, "gpu": null}`, http.StatusOK)
	res := UnderstandAmount(context.Background(), c, "I want the clusters closest to me.")
	if res.Source != AmountSourceNone || !res.Amount.IsZero() {
		t.Errorf("result = %+v, want source none and a zero amount", res)
	}
}

func TestUnderstandAmount_FallsBackToTheText(t *testing.T) {
	t.Run("model unreachable", func(t *testing.T) {
		c, _ := amountServer(t, "", http.StatusInternalServerError)
		res := UnderstandAmount(context.Background(), c, "2 CPUs and 4 GB, closest to me")
		if res.Source != AmountSourceText || res.AIError == "" {
			t.Fatalf("result = %+v, want source text and the model error", res)
		}
		wantAmount(t, res.Amount, "2", "4Gi", 0)
	})
	t.Run("model answers garbage", func(t *testing.T) {
		c, _ := amountServer(t, `sure, 2 cpus`, http.StatusOK)
		res := UnderstandAmount(context.Background(), c, "2 CPUs")
		if res.Source != AmountSourceText {
			t.Fatalf("result = %+v, want source text", res)
		}
		wantAmount(t, res.Amount, "2", "", 0)
	})
	t.Run("no model configured", func(t *testing.T) {
		res := UnderstandAmount(context.Background(), nil, "closest to me")
		if res.Source != AmountSourceNone || res.Trace != nil || !res.Amount.IsZero() {
			t.Errorf("result = %+v, want source none, no trace", res)
		}
	})
}

func TestParseAmount(t *testing.T) {
	cases := []struct {
		text     string
		cpu, mem string
		gpu      int64
	}{
		{"I need 4 CPUs and 8 GB close to me", "4", "8Gi", 0},
		{"2 cores, 4GiB", "2", "4Gi", 0},
		{"two vCPUs and 512 MB of memory", "2", "512Mi", 0},
		{"16G of RAM and a GPU", "", "16Gi", 1},
		{"2 GPUs, 8 cores, 1 TB", "8", "1Ti", 2},
		{"1.5 cpu", "1500m", "", 0},
		{"I want the clusters closest to me.", "", "", 0},
		{"a big cluster in Cuneo", "", "", 0},
		{"run 5 m away", "", "", 0},          // "m" alone is not memory
		{"9999 CPUs", "", "", 0},             // out of range: ignored
		{"4 CPUs and 999999 GB", "4", "", 0}, // only the bad value is dropped
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			wantAmount(t, ParseAmount(tc.text), tc.cpu, tc.mem, tc.gpu)
		})
	}
}

func TestAmount_ResourceListAndString(t *testing.T) {
	a := ParseAmount("4 CPUs, 8 GB and a GPU")
	rl := a.ResourceList()
	if len(rl) != 3 {
		t.Fatalf("resource list = %v, want cpu, memory and gpu", rl)
	}
	if s := a.String(); s != "4 CPU, 8Gi memory, 1 GPU" {
		t.Errorf("String() = %q", s)
	}
	if s := (Amount{}).String(); s != "not stated" {
		t.Errorf("zero String() = %q", s)
	}
	if len((Amount{}).ResourceList()) != 0 {
		t.Error("zero amount must give an empty resource list")
	}
}

func TestAmount_Nodes(t *testing.T) {
	if a := ParseAmount("I want 3 nodes close to me"); a.Nodes != 3 || !a.CPU.IsZero() {
		t.Errorf("ParseAmount nodes = %+v, want 3 nodes only", a)
	}
	if a := ParseAmount("two machines near Cuneo"); a.Nodes != 2 {
		t.Errorf("ParseAmount machines = %+v, want 2", a)
	}
	if a := ParseAmount("40 servers"); a.Nodes != 0 {
		t.Errorf("40 servers is out of range and must be ignored, got %+v", a)
	}
	c, _ := amountServer(t, `{"cpu": null, "memoryGiB": null, "gpu": null, "nodes": 3}`, http.StatusOK)
	res := UnderstandAmount(context.Background(), c, "I want 3 nodes close to me")
	if res.Source != AmountSourceAI || res.Amount.Nodes != 3 || res.Amount.String() != "3 nodes" {
		t.Errorf("result = %+v (%s), want 3 nodes from the model", res, res.Amount.String())
	}
	c2, _ := amountServer(t, `{"cpu": null, "memoryGiB": null, "gpu": null, "nodes": 2.5}`, http.StatusOK)
	if _, _, err := c2.ExtractAmount(context.Background(), "x"); err == nil {
		t.Error("a fractional node count must be rejected")
	}
}

func TestAmount_LatencyLimit(t *testing.T) {
	a := ParseAmount("I need 3 VMs as Kubernetes workers, at most 3 ms from us.")
	if a.Nodes != 3 || a.MaxLatencyMs != 3 || a.String() != "3 nodes, max 3 ms" {
		t.Errorf("ParseAmount = %+v (%s), want 3 nodes, max 3 ms", a, a.String())
	}
	for text, want := range map[string]int32{
		"within 5ms":           5,
		"max 2.5 milliseconds": 3,
		"under 10 msec please": 10,
		"low latency please":   0,
		"as fast as possible":  0,
		"no more than 20 ms":   20,
		"2 CPUs closest to me": 0,
	} {
		if got := ParseAmount(text).MaxLatencyMs; got != want {
			t.Errorf("%q: maxLatencyMs = %d, want %d", text, got, want)
		}
	}
	if only := ParseAmount("at most 3 ms"); only.HasSize() || only.IsZero() {
		t.Errorf("a limit alone is not a size but is not nothing either: %+v", only)
	}

	c, _ := amountServer(t, `{"cpu": null, "memoryGiB": null, "gpu": null, "nodes": 3, "maxLatencyMs": 2.5}`, http.StatusOK)
	res := UnderstandAmount(context.Background(), c, "3 VMs, max 2.5 ms")
	if res.Source != AmountSourceAI || res.Amount.MaxLatencyMs != 3 {
		t.Errorf("result = %+v, want the AI's limit rounded up to 3 ms", res)
	}
	bad, _ := amountServer(t, `{"cpu": null, "memoryGiB": null, "gpu": null, "nodes": null, "maxLatencyMs": 0}`, http.StatusOK)
	if _, _, err := bad.ExtractAmount(context.Background(), "x"); err == nil {
		t.Error("a 0 ms limit must be rejected")
	}
}

func TestSelect_MeasuredLatencyReachesTheModel(t *testing.T) {
	var prompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollamaRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		prompt = req.Prompt
		_ = json.NewEncoder(w).Encode(ollamaResponse{Response: `{"rankedList": ["provider-1", "provider-2"]}`, Done: true})
	}))
	t.Cleanup(srv.Close)
	groups := []brokerapi.NodeGroupView{
		makeNodeGroup(providerOne, 3, 0, nil, nil, "PIE"),
		makeNodeGroup(providerTwo, 3, 0, nil, nil, "PIE"),
	}
	c := New(srv.URL, "m").WithMeasuredLatencies(map[string]float64{providerOne: 2.04, providerTwo: math.Inf(1)})
	if _, err := c.Select(context.Background(), "lowest latency", groups); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, `"measuredLatencyMs": 2`) {
		t.Errorf("provider-1's measured latency must reach the model:\n%s", prompt)
	}
	if strings.Count(prompt, "measuredLatencyMs") != 1 {
		t.Errorf("an unreachable provider (+Inf) must have no measured latency:\n%s", prompt)
	}
}

// A limit the text does not write as "<number> ms" is dropped, even if the
// model returns one (it can borrow the node count).
func TestUnderstandAmount_DropsAnInventedLatencyLimit(t *testing.T) {
	c, _ := amountServer(t, `{"cpu": null, "memoryGiB": null, "gpu": null, "nodes": 3, "maxLatencyMs": 3}`, http.StatusOK)
	res := UnderstandAmount(context.Background(), c, "I need 3 VMs with the lowest latency to us.")
	if res.Amount.Nodes != 3 || res.Amount.MaxLatencyMs != 0 {
		t.Errorf("amount = %+v, want 3 nodes and no latency limit", res.Amount)
	}
	res = UnderstandAmount(context.Background(), c, "3 VMs, at most 3 ms")
	if res.Amount.MaxLatencyMs != 3 {
		t.Errorf("a written limit must stay: %+v", res.Amount)
	}
}
