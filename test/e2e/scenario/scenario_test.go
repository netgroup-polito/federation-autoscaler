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

package scenario

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRenderWorkloadManifest_Defaults(t *testing.T) {
	out, err := renderWorkloadManifest(WorkloadOptions{Namespace: "test-ns"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, s := range []string{
		"name: federation-scaleup-driver",
		"namespace: test-ns",
		"replicas: 1",
		"cpu: 1500m",
		"memory: 1Gi",
		"registry.k8s.io/pause:3.10",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("manifest missing %q", s)
		}
	}
}

func TestRenderWorkloadManifest_HonorsOverrides(t *testing.T) {
	out, err := renderWorkloadManifest(WorkloadOptions{
		Namespace:     "alt",
		Replicas:      3,
		CPURequest:    "500m",
		MemoryRequest: "256Mi",
		Image:         "alt/img:v1",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, s := range []string{"replicas: 3", "cpu: 500m", "memory: 256Mi", "alt/img:v1"} {
		if !strings.Contains(out, s) {
			t.Errorf("manifest missing override %q", s)
		}
	}
}

func TestFirstReservationInPhase(t *testing.T) {
	const raw = `{"items":[
		{"metadata":{"name":"res-a"},"status":{"phase":"Pending"}},
		{"metadata":{"name":"res-b"},"status":{"phase":"Peered"}},
		{"metadata":{"name":"res-c"},"status":{"phase":"Peered"}}
	]}`
	name, ok, err := firstReservationInPhase(raw, "Peered")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok || name != "res-b" {
		t.Errorf("want first Peered = res-b, got name=%q ok=%v", name, ok)
	}

	if _, ok, _ := firstReservationInPhase(raw, "Released"); ok {
		t.Error("Released should not match — none present in fixture")
	}
}

func TestFirstReservationInPhase_BadJSON(t *testing.T) {
	if _, _, err := firstReservationInPhase("not json", "Peered"); err == nil {
		t.Error("expected decode error for bad JSON")
	}
}

func TestFirstReadyVirtualNode(t *testing.T) {
	const raw = `{"items":[
		{"metadata":{"name":"vn-a"},"status":{"conditions":[
			{"type":"Node","status":"Creating"}
		]}},
		{"metadata":{"name":"vn-b"},"status":{"conditions":[
			{"type":"VirtualKubelet","status":"Running"},
			{"type":"Node","status":"Running"}
		]}}
	]}`
	name, ok, err := firstReadyVirtualNode(raw)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok || name != "vn-b" {
		t.Errorf("want vn-b, got name=%q ok=%v", name, ok)
	}
}

func TestCountScheduledPods(t *testing.T) {
	// Three of the four pods have spec.nodeName set — those count as
	// scheduled regardless of whether the kubelet has reported a phase.
	// Without a NamespaceOffloading for the workload namespace (the suite
	// doesn't stamp one yet — see WaitForPodsScheduled), Liqo leaves a
	// scheduled pod Pending rather than reflecting it, so phase alone is
	// too strict; we just want CA to have produced a Liqo virtual node the
	// scheduler could bind to.
	const raw = `{"items":[
		{"spec":{"nodeName":""}},
		{"spec":{"nodeName":"fa-provider-1"}},
		{"spec":{"nodeName":"fa-provider-1"}},
		{"spec":{"nodeName":"fa-provider-2"}}
	]}`
	n, err := countScheduledPods(raw)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 3 {
		t.Errorf("want 3 scheduled pods, got %d", n)
	}
}

func TestWaitUntil_ReturnsImmediatelyWhenCheckPasses(t *testing.T) {
	calls := 0
	err := waitUntil(context.Background(),
		WaitOptions{Timeout: 5 * time.Second, Interval: time.Millisecond},
		func() (bool, error) {
			calls++
			return true, nil
		})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call on immediate success, got %d", calls)
	}
}

func TestWaitUntil_TimesOutWithLastError(t *testing.T) {
	want := errors.New("transient kubectl failure")
	err := waitUntil(context.Background(),
		WaitOptions{Timeout: 50 * time.Millisecond, Interval: 5 * time.Millisecond},
		func() (bool, error) {
			return false, want
		})
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("want wrapped %v, got %v", want, err)
	}
}

func TestWaitUntil_RespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitUntil(ctx, WaitOptions{Timeout: time.Second, Interval: time.Millisecond},
		func() (bool, error) { return false, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestRunHappyPath_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		opts HappyPathOptions
		want string
	}{
		{"missing central", HappyPathOptions{KubeconfigConsumer: "kc"}, "KubeconfigCentral"},
		{"missing consumer", HappyPathOptions{KubeconfigCentral: "kc"}, "KubeconfigConsumer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RunHappyPath(t.Context(), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q; got %v", tc.want, err)
			}
		})
	}
}

func TestCreateUnschedulableWorkload_RequiresKubeconfigAndNamespace(t *testing.T) {
	err := CreateUnschedulableWorkload(t.Context(), WorkloadOptions{Namespace: "ns"})
	if err == nil || !strings.Contains(err.Error(), "Kubeconfig") {
		t.Fatalf("want Kubeconfig error; got %v", err)
	}
	err = CreateUnschedulableWorkload(t.Context(), WorkloadOptions{Kubeconfig: "kc"})
	if err == nil || !strings.Contains(err.Error(), "Namespace") {
		t.Fatalf("want Namespace error; got %v", err)
	}
}
