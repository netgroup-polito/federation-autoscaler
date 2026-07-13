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

package nodeip

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newClient(objs ...runtime.Object) *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...)
}

func node(name string, addrs ...corev1.NodeAddress) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Addresses: addrs},
	}
}

func TestResolveOverrideWins(t *testing.T) {
	// Override short-circuits — the node is never consulted (nil client is fine).
	got, err := Resolve(context.Background(), nil, "worker-1", "203.0.113.9")
	if err != nil || got != "203.0.113.9" {
		t.Fatalf("got %q, err %v; want override", got, err)
	}
}

func TestResolvePrefersExternalIP(t *testing.T) {
	c := newClient(node("worker-1",
		corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "172.23.4.10"},
		corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "203.0.113.9"},
	)).Build()

	got, err := Resolve(context.Background(), c, "worker-1", "")
	if err != nil || got != "203.0.113.9" {
		t.Fatalf("got %q, err %v; want ExternalIP", got, err)
	}
}

func TestResolveFallsBackToInternalIP(t *testing.T) {
	c := newClient(node("worker-1",
		corev1.NodeAddress{Type: corev1.NodeHostName, Address: "worker-1"},
		corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "172.23.4.10"},
	)).Build()

	got, err := Resolve(context.Background(), c, "worker-1", "")
	if err != nil || got != "172.23.4.10" {
		t.Fatalf("got %q, err %v; want InternalIP", got, err)
	}
}

func TestResolveNoNodeName(t *testing.T) {
	c := newClient().Build()
	got, err := Resolve(context.Background(), c, "", "")
	if err != nil || got != "" {
		t.Fatalf("got %q, err %v; want empty/nil", got, err)
	}
}

func TestResolveMissingNode(t *testing.T) {
	c := newClient().Build()
	got, err := Resolve(context.Background(), c, "ghost", "")
	if err == nil {
		t.Fatal("expected error for missing node")
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestResolveNodeWithoutIP(t *testing.T) {
	c := newClient(node("worker-1",
		corev1.NodeAddress{Type: corev1.NodeHostName, Address: "worker-1"},
	)).Build()
	got, err := Resolve(context.Background(), c, "worker-1", "")
	if err != nil || got != "" {
		t.Fatalf("got %q, err %v; want empty/nil", got, err)
	}
}
