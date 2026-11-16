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

func getRequest(t *testing.T, ts *httptest.Server) requestView {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/request")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var v requestView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
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
	if v.Nodes != 2 || v.Source != ollama.AmountSourceText || v.Understood != "4 CPU, 8Gi memory" {
		t.Errorf("view = %+v, want 2 nodes read from the text", v)
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
	if st := getRequest(t, ts); !st.Active || st.Mode != RequestModeReserve || st.Text != text {
		t.Errorf("state = %+v, want the active Reserve now request", st)
	}

	code, _ = sendRequest(t, ts, map[string]string{"action": "send", "text": "more", "mode": RequestModeReserve})
	wantStatus(t, code, http.StatusConflict)

	wantStatus(t, post(t, ts, "/api/request", `{"action":"release"}`), http.StatusOK)
	if len(panelRequests(t, fc)) != 0 {
		t.Error("release must delete the request")
	}
	if st := getRequest(t, ts); st.Active {
		t.Errorf("after release: state = %+v, want inactive", st)
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
	cpu := pod.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if cpu.Cmp(resource.MustParse("1")) <= 0 || cpu.Cmp(resource.MustParse("2")) >= 0 {
		t.Errorf("pod cpu = %s, want more than half a 2-CPU node and less than a whole one", cpu.String())
	}
	if st := getRequest(t, ts); !st.Active || st.Mode != RequestModeOnDemand || st.Nodes != 3 {
		t.Errorf("state = %+v, want the active On demand request", st)
	}

	wantStatus(t, post(t, ts, "/api/request", `{"action":"release"}`), http.StatusOK)
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
