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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

func cleanupInstruction() *brokerapi.InstructionView {
	return &brokerapi.InstructionView{
		ID:            "cleanup-res-1",
		Kind:          string(autoscalingv1alpha1.ReservationInstructionCleanup),
		ReservationID: testResID,
	}
}

func TestCleanup_HappyPath_DeletesAllLocalState(t *testing.T) {
	ctx := context.Background()
	c := newFakeKubeClient()

	// Seed prior peer state.
	if _, err := NewPeerHandler(PeerConfig{
		LocalClient: c,
		Namespace:   testNamespace,
		Run:         stubRun("", nil),
	})(ctx, peerInstruction()); err != nil {
		t.Fatal(err)
	}

	h := NewCleanupHandler(CleanupConfig{
		LocalClient: c,
		Namespace:   testNamespace,
	})
	res, err := h(ctx, cleanupInstruction())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != brokerapi.ResultStatusSucceeded {
		t.Errorf("want Succeeded, got %s", res.Status)
	}
	if res.Payload != nil {
		t.Errorf("Cleanup result should have no payload, got %+v", res.Payload)
	}

	if exists, _ := objectExists(ctx, c, resourceSliceGVK, testNamespace, "rs-"+testResID); exists {
		t.Error("ResourceSlice still present after Cleanup")
	}
	if _, err := getVirtualNodeState(ctx, c, testNamespace, testResID); err == nil {
		t.Error("VirtualNodeState still present after Cleanup")
	}
	if err := c.Get(ctx, types.NamespacedName{
		Name: "kubeconfig-" + testResID, Namespace: testNamespace,
	}, &corev1.Secret{}); err == nil {
		t.Error("kubeconfig Secret should be gone after Cleanup")
	}
}

func TestCleanup_AlreadyGone_IsSuccess(t *testing.T) {
	ctx := context.Background()
	c := newFakeKubeClient()
	h := NewCleanupHandler(CleanupConfig{
		LocalClient: c,
		Namespace:   testNamespace,
	})
	res, err := h(ctx, cleanupInstruction())
	if err != nil {
		t.Fatalf("Cleanup with no prior state should succeed; got %v", err)
	}
	if res.Status != brokerapi.ResultStatusSucceeded {
		t.Errorf("want Succeeded, got %s", res.Status)
	}
}

func TestCleanup_PartialState_StillSucceeds(t *testing.T) {
	ctx := context.Background()
	c := newFakeKubeClient()
	// Only a kubeconfig Secret present; CRs absent.
	_ = c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubeconfig-" + testResID, Namespace: testNamespace,
		},
		Data: map[string][]byte{KubeconfigSecretDataKey: []byte("kc")},
	})

	h := NewCleanupHandler(CleanupConfig{
		LocalClient: c,
		Namespace:   testNamespace,
	})
	if _, err := h(ctx, cleanupInstruction()); err != nil {
		t.Fatalf("Cleanup with partial state should succeed; got %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{
		Name: "kubeconfig-" + testResID, Namespace: testNamespace,
	}, &corev1.Secret{}); err == nil {
		t.Error("kubeconfig Secret should be gone after Cleanup")
	}
}

func TestCleanup_RejectsWrongKind(t *testing.T) {
	h := NewCleanupHandler(CleanupConfig{
		LocalClient: newFakeKubeClient(),
		Namespace:   testNamespace,
	})
	in := cleanupInstruction()
	in.Kind = kindPeer
	_, err := h(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "unexpected kind") {
		t.Fatalf("want unexpected-kind error, got %v", err)
	}
}

func TestCleanup_RejectsNilInstruction(t *testing.T) {
	h := NewCleanupHandler(CleanupConfig{
		LocalClient: newFakeKubeClient(),
		Namespace:   testNamespace,
	})
	if _, err := h(context.Background(), nil); err == nil {
		t.Fatal("want error for nil instruction")
	}
}

func TestCleanup_RejectsMissingLocalClient(t *testing.T) {
	h := NewCleanupHandler(CleanupConfig{Namespace: testNamespace})
	_, err := h(context.Background(), cleanupInstruction())
	if err == nil || !strings.Contains(err.Error(), "LocalClient is nil") {
		t.Fatalf("want LocalClient-nil error, got %v", err)
	}
}

func TestCleanup_RejectsMissingNamespace(t *testing.T) {
	h := NewCleanupHandler(CleanupConfig{LocalClient: newFakeKubeClient()})
	_, err := h(context.Background(), cleanupInstruction())
	if err == nil || !strings.Contains(err.Error(), "Namespace is required") {
		t.Fatalf("want namespace-required error, got %v", err)
	}
}
