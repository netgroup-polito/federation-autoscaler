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

package console

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
)

func TestSizeRequest(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		nodes    int32
		cpu, mem string
		gpu      bool
		wantErr  bool
	}{
		{name: "no amount: one node", text: "closest to me", nodes: 1, cpu: "2", mem: "4Gi"},
		{name: "cpu and memory", text: "4 CPUs and 8 GB", nodes: 2, cpu: "4", mem: "8Gi"},
		{name: "memory decides", text: "2 CPUs and 12 GB", nodes: 3, cpu: "2", mem: "12Gi"},
		{name: "node count", text: "3 nodes near me", nodes: 3, cpu: "6", mem: "12Gi"},
		{name: "node count raised by memory", text: "2 nodes with 16 GB", nodes: 4, cpu: "4", mem: "16Gi"},
		{name: "a GPU", text: "a GPU please", nodes: 1, gpu: true},
		{name: "too big", text: "10 CPUs", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, nodes, block, err := sizeRequest(ollama.ParseAmount(tc.text))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %d nodes", nodes)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if nodes != tc.nodes {
				t.Errorf("nodes = %d, want %d", nodes, tc.nodes)
			}
			if tc.gpu {
				if _, ok := block[corev1.ResourceName("nvidia.com/gpu")]; !ok {
					t.Errorf("a GPU request must use the GPU node size, got %v", block)
				}
				return
			}
			if cpu := res[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse(tc.cpu)) != 0 {
				t.Errorf("cpu = %s, want %s", cpu.String(), tc.cpu)
			}
			if mem := res[corev1.ResourceMemory]; mem.Cmp(resource.MustParse(tc.mem)) != 0 {
				t.Errorf("memory = %s, want %s", mem.String(), tc.mem)
			}
		})
	}
}

// requestServer is a consumer console with an optional AI client.
func requestServer(t *testing.T, llm *ollama.Client) (ctrlclient.Client, *httptest.Server) {
	t.Helper()
	fc := clientfake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, err := New(Options{
		Role: RoleConsumer, BindAddress: ":0", LocalClient: fc, Namespace: testNS,
		ClusterID: testClusterID, LiqoClusterID: testLiqoID, Ollama: llm,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return fc, ts
}

func sendRequest(t *testing.T, ts *httptest.Server, body map[string]string) (int, requestView) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/api/request", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var v requestView
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

func getRequests(t *testing.T, ts *httptest.Server) []requestView {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/request")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var l requestList
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		t.Fatal(err)
	}
	return l.Requests
}

func panelRequests(t *testing.T, fc ctrlclient.Client) []autoscalingv1alpha1.ResourceRequest {
	t.Helper()
	var list autoscalingv1alpha1.ResourceRequestList
	if err := fc.List(context.Background(), &list, ctrlclient.InNamespace(testNS),
		ctrlclient.MatchingLabels{consoleManagedLabel: requestLabelValue}); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// Reserve now: one ResourceRequest carrying the text and the understood
// amount; a second request is refused until the first is released.
func TestRequestReserveNow(t *testing.T) {
	fc, ts := requestServer(t, nil)
	text := "I need 4 CPUs and 8 GB, closest to me"
	code, v := sendRequest(t, ts, map[string]string{"action": "send", "text": text, "mode": RequestModeReserve})
	wantStatus(t, code, http.StatusOK)
	if v.Nodes != 2 || v.Source != ollama.AmountSourceText || v.Understood != "4 CPU, 8Gi memory" || v.Name == "" {
		t.Errorf("view = %+v, want 2 nodes read from the text, and a name", v)
	}
	items := panelRequests(t, fc)
	if len(items) != 1 {
		t.Fatalf("requests = %d, want 1", len(items))
	}
	rr := items[0]
	if rr.Spec.UserPrompt != text {
		t.Errorf("userPrompt = %q, want the text", rr.Spec.UserPrompt)
	}
	if cpu := rr.Spec.Resources[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("resources = %v, want 4 CPU", rr.Spec.Resources)
	}
	// What was understood is kept on the object, not in memory.
	list := getRequests(t, ts)
	if len(list) != 1 || list[0].Name != rr.Name || list[0].Text != text || list[0].Understood != "4 CPU, 8Gi memory" ||
		list[0].Nodes != 2 || list[0].Mode != RequestModeReserve || !list[0].Active {
		t.Errorf("list = %+v, want the request with what was understood", list)
	}
}

// Several Reserve now requests can be active; each is released by name, and
// a release without a name releases all.
func TestRequestSeveralReserveNow(t *testing.T) {
	fc, ts := requestServer(t, nil)
	_, a := sendRequest(t, ts, map[string]string{"action": "send", "text": "I need 2 VMs in Turin", "mode": RequestModeReserve})
	code, b := sendRequest(t, ts, map[string]string{"action": "send", "text": "I need 1 VM in Frankfurt", "mode": RequestModeReserve})
	wantStatus(t, code, http.StatusOK)
	_, c := sendRequest(t, ts, map[string]string{"action": "send", "text": "I need 1 VM, closest to me", "mode": RequestModeReserve})
	if list := getRequests(t, ts); len(list) != 3 {
		t.Fatalf("list = %d requests, want 3", len(list))
	}

	wantStatus(t, post(t, ts, "/api/request", `{"action":"release","name":"`+b.Name+`"}`), http.StatusOK)
	left := panelRequests(t, fc)
	if len(left) != 2 || (left[0].Name != a.Name && left[1].Name != a.Name) || (left[0].Name != c.Name && left[1].Name != c.Name) {
		t.Errorf("after releasing %s: %d left, want %s and %s", b.Name, len(left), a.Name, c.Name)
	}
	wantStatus(t, post(t, ts, "/api/request", `{"action":"release","name":"no-such-request"}`), http.StatusNotFound)

	wantStatus(t, post(t, ts, "/api/request", `{"action":"release"}`), http.StatusOK)
	if len(panelRequests(t, fc)) != 0 || len(getRequests(t, ts)) != 0 {
		t.Error("a release without a name must release every request")
	}
}

// On demand: the policy becomes ConsumerChoice with the text, and a workload
// of one pod per node pinned to borrowed nodes is applied.
func TestRequestOnDemand(t *testing.T) {
	fc, ts := requestServer(t, nil)
	text := "3 nodes, the greenest ones"
	code, v := sendRequest(t, ts, map[string]string{"action": "send", "text": text, "mode": RequestModeOnDemand})
	wantStatus(t, code, http.StatusOK)
	if v.Nodes != 3 {
		t.Errorf("nodes = %d, want 3", v.Nodes)
	}

	var cp autoscalingv1alpha1.ConsumerPolicy
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: consumerPolicyName}, &cp); err != nil {
		t.Fatalf("policy: %v", err)
	}
	if cp.Spec.Placement.Type != autoscalingv1alpha1.PlacementStrategyConsumerChoice || cp.Spec.UserPrompt != text {
		t.Errorf("policy = %+v, want ConsumerChoice with the text", cp.Spec)
	}

	var dep appsv1.Deployment
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: workloadNamespace, Name: onDemandWorkload}, &dep); err != nil {
		t.Fatalf("workload: %v", err)
	}
	pod := dep.Spec.Template.Spec
	if *dep.Spec.Replicas != 3 || pod.NodeSelector["liqo.io/type"] != "virtual-node" || len(pod.Tolerations) != 1 {
		t.Errorf("workload = %d replicas, selector %v, tolerations %v; want 3 pinned to virtual nodes",
			*dep.Spec.Replicas, pod.NodeSelector, pod.Tolerations)
	}
	na := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0]
	if na.Key != autoscalingv1alpha1.HeldByLabel || na.Operator != corev1.NodeSelectorOpDoesNotExist {
		t.Errorf("pods must avoid nodes held for other requests, affinity %+v", na)
	}
	cpu := pod.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if cpu.Cmp(resource.MustParse("1")) <= 0 || cpu.Cmp(resource.MustParse("2")) >= 0 {
		t.Errorf("pod cpu = %s, want more than half a 2-CPU node and less than a whole one", cpu.String())
	}
	list := getRequests(t, ts)
	if len(list) != 1 || list[0].Name != onDemandName || list[0].Mode != RequestModeOnDemand || list[0].Nodes != 3 ||
		list[0].Text != text || list[0].Understood != "3 nodes" {
		t.Errorf("list = %+v, want the On demand request", list)
	}
	code, _ = sendRequest(t, ts, map[string]string{"action": "send", "text": "more", "mode": RequestModeOnDemand})
	wantStatus(t, code, http.StatusConflict)
	// A Reserve now request can still be added next to it.
	code, _ = sendRequest(t, ts, map[string]string{"action": "send", "text": "1 VM", "mode": RequestModeReserve})
	wantStatus(t, code, http.StatusOK)

	wantStatus(t, post(t, ts, "/api/request", `{"action":"release","name":"`+onDemandName+`"}`), http.StatusOK)
	if len(panelRequests(t, fc)) != 1 {
		t.Error("releasing the On demand request must leave the Reserve now one")
	}
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: workloadNamespace, Name: onDemandWorkload}, &dep); err == nil {
		t.Error("release must delete the workload")
	}
}

// The AI's reading of the amount is used when it answers; the panel's fields
// override it.
func TestRequestAmountFromTheAIAndTheFields(t *testing.T) {
	fakeAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": `{"cpu": 6, "memoryGiB": null, "gpu": null, "nodes": null}`, "done": true,
		})
	}))
	t.Cleanup(fakeAI.Close)
	_, ts := requestServer(t, ollama.New(fakeAI.URL, "m"))

	_, v := sendRequest(t, ts, map[string]string{"action": "send", "text": "six cores please", "mode": RequestModeReserve})
	if v.Source != ollama.AmountSourceAI || v.Nodes != 3 {
		t.Errorf("view = %+v, want 6 CPU = 3 nodes read by the AI", v)
	}
	wantStatus(t, post(t, ts, "/api/request", `{"action":"release"}`), http.StatusOK)

	_, v = sendRequest(t, ts, map[string]string{"action": "send", "text": "six cores please", "mode": RequestModeReserve, "cpu": "2"})
	if v.Source != "form" || v.Nodes != 1 {
		t.Errorf("view = %+v, want the CPU field to win: 1 node", v)
	}
}

func TestRequestValidation(t *testing.T) {
	_, ts := requestServer(t, nil)
	for name, body := range map[string]map[string]string{
		"empty text":   {"action": "send", "text": "  ", "mode": RequestModeReserve},
		"bad mode":     {"action": "send", "text": "hi", "mode": "later"},
		"too big":      {"action": "send", "text": "12 CPUs", "mode": RequestModeReserve},
		"bad field":    {"action": "send", "text": "hi", "mode": RequestModeReserve, "cpu": "lots"},
		"bad action":   {"action": "explode"},
		"negative cpu": {"action": "send", "text": "hi", "mode": RequestModeReserve, "cpu": "-2"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _ := sendRequest(t, ts, body)
			wantStatus(t, code, http.StatusBadRequest)
		})
	}
}

// "at most N ms" becomes the latency limit: on the reservation (Reserve now)
// or on the policy (On demand), cleared again on release.
func TestRequestLatencyLimit(t *testing.T) {
	fc, ts := requestServer(t, nil)
	text := "I need 3 VMs as Kubernetes workers, at most 3 ms from us."
	_, v := sendRequest(t, ts, map[string]string{"action": "send", "text": text, "mode": RequestModeReserve})
	if v.Nodes != 3 || v.MaxLatencyMs != 3 || v.Understood != "3 nodes, max 3 ms" {
		t.Errorf("view = %+v, want 3 nodes, max 3 ms", v)
	}
	items := panelRequests(t, fc)
	if len(items) != 1 || items[0].Spec.LatencyBound == nil || items[0].Spec.LatencyBound.MaxLatencyMs != 3 {
		t.Fatalf("reservation bound = %+v, want max 3 ms", items)
	}
	wantStatus(t, post(t, ts, "/api/request", `{"action":"release"}`), http.StatusOK)

	_, _ = sendRequest(t, ts, map[string]string{"action": "send", "text": text, "mode": RequestModeOnDemand})
	var cp autoscalingv1alpha1.ConsumerPolicy
	key := types.NamespacedName{Namespace: testNS, Name: consumerPolicyName}
	if err := fc.Get(context.Background(), key, &cp); err != nil {
		t.Fatal(err)
	}
	if cp.Spec.LatencyBound == nil || cp.Spec.LatencyBound.MaxLatencyMs != 3 {
		t.Errorf("policy bound = %+v, want max 3 ms", cp.Spec.LatencyBound)
	}
	wantStatus(t, post(t, ts, "/api/request", `{"action":"release"}`), http.StatusOK)
	if err := fc.Get(context.Background(), key, &cp); err != nil {
		t.Fatal(err)
	}
	if cp.Spec.LatencyBound != nil {
		t.Errorf("after release the policy bound must be cleared, got %+v", cp.Spec.LatencyBound)
	}
}

// splitAI is a fake model: it splits by place when asked to split, and reads
// "<n> VM(s)" when asked for the amount.
func splitAI(t *testing.T, parts []string) *ollama.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			System string `json:"system"`
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		answer := `{"nodes": null}`
		switch {
		case req.System == ollama.SplitSystemPrompt:
			b, _ := json.Marshal(map[string]any{"parts": parts})
			answer = string(b)
		case strings.Contains(req.Prompt, "2 VMs"):
			answer = `{"nodes": 2}`
		case strings.Contains(req.Prompt, "6 VMs"):
			answer = `{"nodes": 6}`
		case strings.Contains(req.Prompt, "1 VM"):
			answer = `{"nodes": 1}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"response": answer, "done": true})
	}))
	t.Cleanup(srv.Close)
	return ollama.New(srv.URL, "m")
}

// A request asking for several places becomes one Reserve now request per
// place, linked as one group, listed in order and released together.
func TestRequestSplitByPlace(t *testing.T) {
	text := "I need 2 VMs in Turin and 1 VM in Frankfurt"
	fc, ts := requestServer(t, splitAI(t, []string{"2 VMs in Turin", "1 VM in Frankfurt"}))

	code, v := sendRequest(t, ts, map[string]string{"action": "send", "text": text, "mode": RequestModeReserve})
	wantStatus(t, code, http.StatusOK)
	if v.Parts != 2 || v.Group == "" || v.Part != "1/2" || v.Text != "2 VMs in Turin" {
		t.Errorf("view = %+v, want part 1 of 2", v)
	}
	items := panelRequests(t, fc)
	if len(items) != 2 || items[0].Labels[groupLabel] != v.Group || items[1].Labels[groupLabel] != v.Group {
		t.Fatalf("requests = %+v, want 2 in group %s", items, v.Group)
	}
	list := getRequests(t, ts)
	if len(list) != 2 || list[0].Text != "2 VMs in Turin" || list[0].Nodes != 2 || list[0].Part != "1/2" ||
		list[1].Text != "1 VM in Frankfurt" || list[1].Nodes != 1 || list[1].Part != "2/2" || list[1].GroupText != text {
		t.Errorf("list = %+v, want the two parts in order with the whole request", list)
	}

	// Another request stays when the group is released.
	_, other := sendRequest(t, ts, map[string]string{"action": "send", "text": "1 VM, closest to me", "mode": RequestModeReserve})
	wantStatus(t, post(t, ts, "/api/request", `{"action":"release","group":"`+v.Group+`"}`), http.StatusOK)
	left := panelRequests(t, fc)
	if len(left) != 1 || left[0].Name != other.Name {
		t.Errorf("after releasing the group: %+v, want only %s", left, other.Name)
	}
	wantStatus(t, post(t, ts, "/api/request", `{"action":"release","group":"`+v.Group+`"}`), http.StatusNotFound)
}

// A part that cannot be placed rejects the whole request: nothing is created.
func TestRequestSplitAllOrNothing(t *testing.T) {
	fc, ts := requestServer(t, splitAI(t, []string{"2 VMs in Turin", "6 VMs in Frankfurt"}))
	code, _ := sendRequest(t, ts, map[string]string{
		"action": "send", "text": "2 VMs in Turin and 6 VMs in Frankfurt", "mode": RequestModeReserve,
	})
	wantStatus(t, code, http.StatusBadRequest)
	if n := len(panelRequests(t, fc)); n != 0 {
		t.Errorf("%d requests created, want none", n)
	}
}

// The text is not split with the amount fields filled, in On demand mode, or
// without a model.
func TestRequestNotSplit(t *testing.T) {
	text := "I need 2 VMs in Turin and 1 VM in Frankfurt"
	ai := splitAI(t, []string{"2 VMs in Turin", "1 VM in Frankfurt"})

	fc, ts := requestServer(t, ai)
	_, v := sendRequest(t, ts, map[string]string{"action": "send", "text": text, "mode": RequestModeReserve, "cpu": "2"})
	if v.Parts != 1 || len(panelRequests(t, fc)) != 1 {
		t.Errorf("with the amount fields: view %+v, want one request", v)
	}

	_, ts = requestServer(t, ai)
	_, v = sendRequest(t, ts, map[string]string{"action": "send", "text": text, "mode": RequestModeOnDemand})
	if v.Parts != 1 || v.Name != onDemandName {
		t.Errorf("On demand: view %+v, want one request", v)
	}

	fc, ts = requestServer(t, nil)
	_, v = sendRequest(t, ts, map[string]string{"action": "send", "text": text, "mode": RequestModeReserve})
	if v.Parts != 1 || len(panelRequests(t, fc)) != 1 {
		t.Errorf("without a model: view %+v, want one request", v)
	}
}
