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

package snapshot

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// readyNode helper builds a Node that the snapshot will count, with
// Ready=True, Unschedulable=false, and the given allocatable map.
func readyNode(name string, alloc corev1.ResourceList) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: alloc,
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func cordonedNode(name string, alloc corev1.ResourceList) *corev1.Node {
	n := readyNode(name, alloc)
	n.Spec.Unschedulable = true
	return n
}

func notReadyNode(name string, alloc corev1.ResourceList) *corev1.Node {
	n := readyNode(name, alloc)
	n.Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
	}
	return n
}

func newClientWith(objs ...ctrlclient.Object) ctrlclient.Client {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	return clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestTake_NilClientErrors(t *testing.T) {
	if _, err := Take(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestTake_EmptyCluster(t *testing.T) {
	snap, err := Take(context.Background(), newClientWith())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if snap.CountedNodes != 0 {
		t.Errorf("CountedNodes: want 0, got %d", snap.CountedNodes)
	}
	if len(snap.Allocatable) != 0 {
		t.Errorf("Allocatable: want empty, got %+v", snap.Allocatable)
	}
}

func TestTake_SumsAcrossReadyNodes(t *testing.T) {
	c := newClientWith(
		readyNode("worker-1", corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
		}),
		readyNode("worker-2", corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
			"nvidia.com/gpu":      resource.MustParse("1"),
		}),
	)

	snap, err := Take(context.Background(), c)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if snap.CountedNodes != 2 {
		t.Errorf("CountedNodes: want 2, got %d", snap.CountedNodes)
	}
	if got := snap.Allocatable[corev1.ResourceCPU]; got.Cmp(resource.MustParse("6")) != 0 {
		t.Errorf("CPU: want 6, got %s", got.String())
	}
	if got := snap.Allocatable[corev1.ResourceMemory]; got.Cmp(resource.MustParse("12Gi")) != 0 {
		t.Errorf("Memory: want 12Gi, got %s", got.String())
	}
	if got := snap.Allocatable["nvidia.com/gpu"]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("GPU: want 1, got %s", got.String())
	}
	if len(snap.SkippedNodes) != 0 {
		t.Errorf("SkippedNodes: want empty, got %+v", snap.SkippedNodes)
	}
}

func TestTake_SkipsCordonedNodes(t *testing.T) {
	c := newClientWith(
		readyNode("worker-1", corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("4"),
		}),
		cordonedNode("drain-me", corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("8"),
		}),
	)

	snap, err := Take(context.Background(), c)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if snap.CountedNodes != 1 {
		t.Errorf("CountedNodes: want 1, got %d", snap.CountedNodes)
	}
	if got := snap.Allocatable[corev1.ResourceCPU]; got.Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("CPU: want 4 (cordoned skipped), got %s", got.String())
	}
	if reason, ok := snap.SkippedNodes["drain-me"]; !ok || reason != "cordoned" {
		t.Errorf("SkippedNodes[drain-me]: want 'cordoned', got %q (present=%v)", reason, ok)
	}
}

func TestTake_SkipsNotReadyNodes(t *testing.T) {
	c := newClientWith(
		readyNode("worker-1", corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"),
		}),
		notReadyNode("rebooting", corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("8"),
		}),
	)

	snap, err := Take(context.Background(), c)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if snap.CountedNodes != 1 {
		t.Errorf("CountedNodes: want 1, got %d", snap.CountedNodes)
	}
	if got := snap.Allocatable[corev1.ResourceCPU]; got.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("CPU: want 2 (not-ready skipped), got %s", got.String())
	}
	if reason, ok := snap.SkippedNodes["rebooting"]; !ok || reason == "" {
		t.Errorf("SkippedNodes[rebooting]: want non-empty reason, got %q (present=%v)", reason, ok)
	}
}

// erroringClient lets us simulate a List failure without involving envtest.
type erroringClient struct {
	ctrlclient.Client
}

func (erroringClient) List(_ context.Context, _ ctrlclient.ObjectList, _ ...ctrlclient.ListOption) error {
	return errors.New("api server unreachable")
}

func TestTake_ListFailureWraps(t *testing.T) {
	_, err := Take(context.Background(), erroringClient{})
	if err == nil {
		t.Fatal("expected error from failing List")
	}
	want := "api server unreachable"
	if err.Error() == "" || err.Error()[len(err.Error())-len(want):] != want {
		t.Errorf("expected wrapped 'api server unreachable', got %q", err.Error())
	}
}
