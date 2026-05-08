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

package instructions

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

func reconcileInstruction() *brokerapi.InstructionView {
	return &brokerapi.InstructionView{
		ID:            "reconcile-1",
		Kind:          string(autoscalingv1alpha1.ProviderInstructionReconcile),
		ReservationID: "res-1",
	}
}

func reconcileFakeClient(nodes ...*corev1.Node) ctrlclient.Client {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	objs := make([]ctrlclient.Object, 0, len(nodes))
	for _, n := range nodes {
		objs = append(objs, n)
	}
	return clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func reconcileReadyNode(name string, alloc corev1.ResourceList) *corev1.Node {
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

func TestReconcile_HappyPath_ReturnsAdvertisement(t *testing.T) {
	c := reconcileFakeClient(reconcileReadyNode("worker-1", corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
	}))
	h := NewReconcileHandler(ReconcileConfig{
		LocalClient:   c,
		ClusterID:     "provider-int",
		LiqoClusterID: "liqo-provider-int",
	})

	res, err := h(context.Background(), reconcileInstruction())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != brokerapi.ResultStatusSucceeded {
		t.Fatalf("want Succeeded, got %s", res.Status)
	}
	if res.Payload == nil || res.Payload.Kind != brokerapi.PayloadKindReconcile {
		t.Fatalf("want ReconcilePayload, got %+v", res.Payload)
	}
	adv := res.Payload.Advertisement
	if adv == nil {
		t.Fatal("Advertisement payload missing")
	}
	if adv.ClusterID != "provider-int" || adv.LiqoClusterID != "liqo-provider-int" {
		t.Errorf("clusterID/liqoID mismatch: %+v", adv)
	}
	if got := adv.Resources[corev1.ResourceCPU]; got.Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("CPU: want 4, got %s", got.String())
	}
}

func TestReconcile_RejectsWrongKind(t *testing.T) {
	h := NewReconcileHandler(ReconcileConfig{
		LocalClient:   reconcileFakeClient(),
		ClusterID:     "p",
		LiqoClusterID: "l",
	})
	in := reconcileInstruction()
	in.Kind = "GenerateKubeconfig"
	_, err := h(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "unexpected kind") {
		t.Fatalf("want unexpected-kind error, got %v", err)
	}
}

func TestReconcile_RejectsNilInstruction(t *testing.T) {
	h := NewReconcileHandler(ReconcileConfig{
		LocalClient:   reconcileFakeClient(),
		ClusterID:     "p",
		LiqoClusterID: "l",
	})
	if _, err := h(context.Background(), nil); err == nil {
		t.Fatal("want error for nil instruction")
	}
}

func TestReconcile_RejectsMissingLocalClient(t *testing.T) {
	h := NewReconcileHandler(ReconcileConfig{
		ClusterID:     "p",
		LiqoClusterID: "l",
	})
	_, err := h(context.Background(), reconcileInstruction())
	if err == nil || !strings.Contains(err.Error(), "LocalClient is nil") {
		t.Fatalf("want LocalClient-nil error, got %v", err)
	}
}

func TestReconcile_RejectsMissingClusterIDs(t *testing.T) {
	h := NewReconcileHandler(ReconcileConfig{
		LocalClient: reconcileFakeClient(),
		ClusterID:   "p",
		// LiqoClusterID intentionally empty
	})
	_, err := h(context.Background(), reconcileInstruction())
	if err == nil || !strings.Contains(err.Error(), "ClusterID and LiqoClusterID") {
		t.Fatalf("want missing-IDs error, got %v", err)
	}
}

// reconcileFailingClient simulates a List failure so we can exercise the
// snapshot-error wrap.
type reconcileFailingClient struct {
	ctrlclient.Client
}

func (reconcileFailingClient) List(_ context.Context, _ ctrlclient.ObjectList, _ ...ctrlclient.ListOption) error {
	return errors.New("api server gone")
}

func TestReconcile_SnapshotFailure_Wraps(t *testing.T) {
	h := NewReconcileHandler(ReconcileConfig{
		LocalClient:   reconcileFailingClient{},
		ClusterID:     "p",
		LiqoClusterID: "l",
	})
	_, err := h(context.Background(), reconcileInstruction())
	if err == nil || !strings.Contains(err.Error(), "api server gone") {
		t.Fatalf("want snapshot-wrap error, got %v", err)
	}
}
