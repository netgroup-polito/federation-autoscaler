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

// Command aibench measures how well an AI model and engine serve the consumer
// agent's plain-language requests, with the agent's own code
// (internal/agent/ollama): the same prompts, the same answer checks, the same
// fallbacks. For each model it runs a fixed set of requests against three
// demo-like providers and reports:
//
//   - choice: how often the provider the agent would grow is the right one;
//   - amount: how often the amount and latency limit are read right;
//   - fallback: how often the model's answer could not be used;
//   - time per request (both AI calls), median and 90th percentile;
//   - with -parallel N: the same with N requests at once.
//
// Both AI jobs can be measured: "rank" (the model ranks the providers) and
// "fields" (the model only reads the request; the agent ranks in code).
//
//	go run ./federation-tests/aibench -url http://host:11434 -models qwen2.5:7b,qwen3:4b
//	go run ./federation-tests/aibench -url http://host:8000 -api openai -models Qwen/Qwen3-4B
//
// Results go to stdout as a Markdown table, one row per model and job, and
// every single answer to -out as JSON lines.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// consumer is where the requests come from (Mondovì, as in the demo).
var consumer = &ollama.Location{Latitude: 44.3912, Longitude: 7.8197, Region: "PIE"}

// rtts are the round-trip times the consumer measured to each provider.
var rtts = map[string]float64{"provider-1": 1.3, "provider-2": 5.2, "provider-3": 10.4}

// providers are three demo-like providers whose data make every request
// below have one clear answer:
//   - provider-1, Cuneo: nearest and fastest, dearest, dirty now but the
//     cleanest over the next hours, guarantees 4 h;
//   - provider-2, Turin: renewable, the cleanest now, guarantees 72 h;
//   - provider-3, Frankfurt: far and slow, the cheapest, the dirtiest, the
//     most free capacity, no guarantee.
func providers() []brokerapi.NodeGroupView {
	view := func(id, city, region string, lat, lon, cost, carbon float64, forecast []float64,
		renewable bool, cpuPrice, memPrice string, guaranteeH int32, free int32) brokerapi.NodeGroupView {
		c := carbon
		return brokerapi.NodeGroupView{
			ID: id + "-standard", ProviderClusterID: id, Type: brokerv1alpha1.ChunkTypeStandard, MaxSize: free,
			ChunkResources: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
			Cost:              resource.NewMilliQuantity(int64(math.Round(cost*1000)), resource.DecimalSI),
			Topology:          &brokerv1alpha1.Topology{City: city, Region: region, Latitude: lat, Longitude: lon},
			CarbonIntensity:   &c,
			CarbonForecast:    forecast,
			Renewable:         renewable,
			UnitPrices:        corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuPrice), corev1.ResourceMemory: resource.MustParse(memPrice)},
			GuaranteedSeconds: guaranteeH * 3600,
			ProbeEndpoint:     id + ":30100",
		}
	}
	return []brokerapi.NodeGroupView{
		view("provider-1", "Cuneo", "PIE", 44.3845, 7.5427, 0.076, 178, []float64{120, 90, 80, 80, 85, 85}, false, "30m", "4m", 4, 3),
		view("provider-2", "Turin", "PIE", 45.0703, 7.6869, 0.064, 120, []float64{130, 140, 140, 145, 150, 150}, true, "25m", "3500u", 72, 5),
		view("provider-3", "Frankfurt", "HE", 50.1109, 8.6821, 0.028, 316, []float64{310, 300, 300, 305, 300, 300}, false, "10m", "2m", 0, 8),
	}
}

// question is one request and its right answers.
type question struct {
	Text string
	// Want is the set of right providers (any of them is right).
	Want []string
	// Nodes, CPU, MemGiB, MaxMs are the amount and limit to read; 0 is "not
	// stated". Nodes 1 also accepts "not stated" (one node is the default).
	Nodes, CPU, MemGiB, MaxMs int64
}

var questions = []question{
	{Text: "I need 3 VMs as Kubernetes workers, at most 3 ms from us.", Want: []string{"provider-1"}, Nodes: 3, MaxMs: 3},
	{Text: "I need 4 CPUs and 8 GB with the lowest latency to us.", Want: []string{"provider-1"}, CPU: 4, MemGiB: 8},
	{Text: "I need 2 VMs in Turin.", Want: []string{"provider-2"}, Nodes: 2},
	{Text: "I need 1 VM in Frankfurt.", Want: []string{"provider-3"}, Nodes: 1},
	{Text: "I need a VM from the cheapest provider.", Want: []string{"provider-3"}, Nodes: 1},
	{Text: "I need 2 VMs on renewable energy only.", Want: []string{"provider-2"}, Nodes: 2},
	{Text: "I need 1 VM, the greenest over the next hours.", Want: []string{"provider-1"}, Nodes: 1},
	{Text: "The greenest provider, please.", Want: []string{"provider-2"}},
	{Text: "I want the clusters closest to me.", Want: []string{"provider-1"}},
	{Text: "1 VM, not in Frankfurt, the cheapest.", Want: []string{"provider-2"}, Nodes: 1},
	{Text: "2 VMs, cheapest per CPU.", Want: []string{"provider-3"}, Nodes: 2},
	{Text: "I need 2 VMs for two days.", Want: []string{"provider-2"}, Nodes: 2},
	{Text: "Two machines near Cuneo, please.", Want: []string{"provider-1"}, Nodes: 2},
	{Text: "I need two machines, as fast as possible.", Want: []string{"provider-1"}, Nodes: 2},
	{Text: "8 GB of memory and 2 cores, except in Turin, the greenest.", Want: []string{"provider-1"}, CPU: 2, MemGiB: 8},
	{Text: "Give me 2 nodes with renewables, at most 6 ms.", Want: []string{"provider-2"}, Nodes: 2, MaxMs: 6},
	{Text: "Need 1 server, low cost, outside Frankfurt.", Want: []string{"provider-2"}, Nodes: 1},
	{Text: "I need 16 GB of RAM in Frankfurt.", Want: []string{"provider-3"}, MemGiB: 16},
	{Text: "3 VMs with the lowest latency, at most 12 ms.", Want: []string{"provider-1"}, Nodes: 3, MaxMs: 12},
	{Text: "4 vCPUs from the provider with the most free capacity.", Want: []string{"provider-3"}, CPU: 4},
}

// answer is one request's outcome.
type answer struct {
	Model, API, Job, Question string
	Run                       int
	Parallel                  int     `json:",omitempty"`
	Chosen                    string  `json:"chosen"`
	ChoiceOK                  bool    `json:"choiceOk"`
	AmountOK                  bool    `json:"amountOk"`
	Amount                    string  `json:"amount"`
	Understood                string  `json:"understood,omitempty"`
	Fallback                  bool    `json:"fallback"`
	Seconds                   float64 `json:"seconds"`
	Error                     string  `json:"error,omitempty"`
}

func main() {
	url := flag.String("url", "http://localhost:11434", "AI server URL (with -api openai it may end in /v1)")
	api := flag.String("api", ollama.APIOllama, "ollama or openai")
	models := flag.String("models", "qwen2.5:7b", "comma-separated model names")
	jobs := flag.String("jobs", ollama.JobRank+","+ollama.JobFields, "comma-separated AI jobs: rank, fields")
	runs := flag.Int("runs", 3, "times each request is asked")
	parallel := flag.Int("parallel", 10, "requests at once for the parallel test (0 skips it)")
	timeout := flag.Duration("timeout", 120*time.Second, "bound on one AI call")
	out := flag.String("out", "aibench.jsonl", "file for every single answer (JSON lines)")
	split := flag.Bool("split", false, "check only how requests naming several places are split")
	flag.Parse()

	if *split {
		for _, model := range split2(*models) {
			checkSplit(ollama.NewWithOptions(*url, model, ollama.Options{API: *api, Timeout: *timeout}), model, *runs)
		}
		return
	}

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)

	fmt.Println("| model | api | job | choice ok | amount ok | fallback | median s | p90 s | parallel | parallel median s | parallel req/min |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|---|")
	for _, model := range split2(*models) {
		client := ollama.NewWithOptions(*url, model, ollama.Options{API: *api, Timeout: *timeout})
		warmUp(client)
		for _, job := range split2(*jobs) {
			var all []answer
			for run := 1; run <= *runs; run++ {
				for _, q := range questions {
					a := ask(client, model, *api, job, q)
					a.Run = run
					_ = enc.Encode(a)
					all = append(all, a)
				}
			}
			row := summarize(all)
			pStr, pMed, pRate := "-", "-", "-"
			if *parallel > 0 {
				par, wall := askParallel(client, model, *api, job, *parallel)
				for _, a := range par {
					_ = enc.Encode(a)
				}
				pStr = fmt.Sprintf("%d", *parallel)
				pMed = fmt.Sprintf("%.1f", percentile(seconds(par), 50))
				pRate = fmt.Sprintf("%.0f", float64(len(par))/wall.Minutes())
			}
			fmt.Printf("| %s | %s | %s | %s | %s | %s | %.1f | %.1f | %s | %s | %s |\n",
				model, *api, job, row.choice, row.amount, row.fallback, row.median, row.p90, pStr, pMed, pRate)
		}
	}
}

// warmUp loads the model, so the first timed request does not pay for it.
func warmUp(c *ollama.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_ = ollama.UnderstandAmount(ctx, c, "I need 1 VM.")
}

// ask runs one request the way the consumer agent does: read the amount and
// limit, drop providers over the limit, then rank (rank job: excluded places
// removed in code first, then the model ranks; fields job: the model reads
// the preferences and the agent ranks in code). The chosen provider is the
// first-ranked one.
func ask(c *ollama.Client, model, api, job string, q question) answer {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*c.Timeout()+time.Minute)
	defer cancel()
	a := answer{Model: model, API: api, Job: job, Question: q.Text}

	amt := ollama.UnderstandAmount(ctx, c, q.Text)
	a.Amount = amt.Amount.String()
	a.AmountOK = amountMatches(amt.Amount, q)
	if amt.AIError != "" {
		a.Fallback = true
		a.Error = amt.AIError
	}

	groups := providers()
	if ms := amt.Amount.MaxLatencyMs; ms > 0 {
		groups = slices.DeleteFunc(groups, func(ng brokerapi.NodeGroupView) bool { return rtts[ng.ProviderClusterID] > float64(ms) })
	}

	var ranked []string
	if job == ollama.JobFields {
		res := ollama.UnderstandIntent(ctx, c, q.Text, ollama.KnownPlaces(groups))
		if res.AIError != "" {
			a.Fallback = true
			a.Error = res.AIError
		}
		a.Understood = res.Intent.String()
		ranked = ollama.RankByIntent(groups, res.Intent, rtts, consumer)
	} else {
		if excl := ollama.ParseIntent(q.Text, ollama.KnownPlaces(groups)).Exclude; len(excl) > 0 {
			groups = ollama.WithoutPlaces(groups, excl)
		}
		trace, err := c.WithConsumerLocation(consumer).WithMeasuredLatencies(rtts).SelectDetailed(ctx, q.Text, groups)
		ranked = trace.Ranked
		if err != nil || len(ranked) == 0 {
			a.Fallback = true
			if err != nil {
				a.Error = err.Error()
			}
			ranked, _ = ollama.DeterministicFallback(groups)
		}
	}
	if len(ranked) > 0 {
		a.Chosen = ranked[0]
	}
	a.ChoiceOK = slices.Contains(q.Want, a.Chosen)
	a.Seconds = time.Since(start).Seconds()
	return a
}

// askParallel asks n requests at once (the questions in turn) and returns
// their answers and the wall time for all of them.
func askParallel(c *ollama.Client, model, api, job string, n int) ([]answer, time.Duration) {
	out := make([]answer, n)
	var wg sync.WaitGroup
	start := time.Now()
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := ask(c, model, api, job, questions[i%len(questions)])
			a.Parallel = n
			out[i] = a
		}(i)
	}
	wg.Wait()
	return out, time.Since(start)
}

func amountMatches(got ollama.Amount, q question) bool {
	nodes := got.Nodes
	if q.Nodes == 1 && nodes == 0 {
		nodes = 1
	}
	return nodes == q.Nodes &&
		got.CPU.Value() == q.CPU &&
		got.Memory.Value() == q.MemGiB*1024*1024*1024 &&
		int64(got.MaxLatencyMs) == q.MaxMs
}

type summary struct {
	choice, amount, fallback string
	median, p90              float64
}

func summarize(all []answer) summary {
	var ok, amt, fb int
	for _, a := range all {
		if a.ChoiceOK {
			ok++
		}
		if a.AmountOK {
			amt++
		}
		if a.Fallback {
			fb++
		}
	}
	pct := func(n int) string { return fmt.Sprintf("%d/%d", n, len(all)) }
	s := seconds(all)
	return summary{choice: pct(ok), amount: pct(amt), fallback: pct(fb), median: percentile(s, 50), p90: percentile(s, 90)}
}

func seconds(all []answer) []float64 {
	out := make([]float64, 0, len(all))
	for _, a := range all {
		out = append(out, a.Seconds)
	}
	return out
}

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[int(math.Ceil(p/100*float64(len(s))))-1]
}

func split2(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitQuestions are requests and the parts they must be split into (one
// part: not split).
var splitQuestions = []struct {
	text string
	want int
}{
	{"I need 2 VMs in Turin and 1 VM in Frankfurt.", 2},
	{"2 VMs in Turin and 1 in Frankfurt, at most 12 ms.", 2},
	{"One VM in Cuneo, two in Turin and one in Frankfurt.", 3},
	{"4 CPUs and 8 GB in Turin.", 1},
	{"I need 3 VMs in Turin, the cheapest, and nothing in Frankfurt.", 1},
	{"A VM in Turin or in Cuneo, whichever is cheaper.", 1},
}

// checkSplit asks each split question runs times and prints the parts.
func checkSplit(c *ollama.Client, model string, runs int) {
	ok, total := 0, 0
	for range runs {
		for _, q := range splitQuestions {
			start := time.Now()
			res := ollama.UnderstandSplit(context.Background(), c, q.text)
			total++
			good := len(res.Parts) == q.want
			if good {
				ok++
			}
			fmt.Printf("%s | %v | %.1fs | %q -> %q %s\n", model, good, time.Since(start).Seconds(), q.text, res.Parts, res.AIError)
		}
	}
	fmt.Printf("SPLIT %s: %d/%d right\n", model, ok, total)
}
