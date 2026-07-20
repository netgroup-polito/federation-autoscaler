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
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

func unpeerInstruction(lastChunk bool) *brokerapi.InstructionView {
	return &brokerapi.InstructionView{
		ID:                    "unpeer-res-1",
		Kind:                  string(autoscalingv1alpha1.ReservationInstructionUnpeer),
		ReservationID:         testResID,
		ProviderClusterID:     "provider-1",
		ProviderLiqoClusterID: "liqo-provider-1",
		ChunkCount:            1,
		LastChunk:             lastChunk,
	}
}

// -----------------------------------------------------------------------------
// Happy path
// -----------------------------------------------------------------------------

func TestUnpeer_HappyPath_DeletesEverythingOnLastChunk(t *testing.T) {
	ctx := context.Background()
	c := newFakeKubeClient()

	// Seed peer state by running the Peer handler first.
	if _, err := NewPeerHandler(PeerConfig{
		LocalClient: c,
		Namespace:   testNamespace,
		Run:         stubRun("", nil),
	})(ctx, peerInstruction()); err != nil {
		t.Fatal(err)
	}

	// Seed the ForeignCluster shell liqoctl would have left behind, named
	// after the provider's Liqo cluster ID (see unpeerInstruction).
	fc := &unstructured.Unstructured{}
	fc.SetGroupVersionKind(foreignClusterGVK)
	fc.SetName("liqo-provider-1")
	if err := c.Create(ctx, fc); err != nil {
		t.Fatalf("seed ForeignCluster: %v", err)
	}

	var ranLiqoctl atomic.Bool
	h := NewUnpeerHandler(UnpeerConfig{
		LocalClient: c,
		Namespace:   testNamespace,
		Run: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
			ranLiqoctl.Store(true)
			if len(args) < 1 || args[0] != "unpeer" {
				t.Errorf("want liqoctl unpeer, got %v", args)
			}
			return nil, nil, nil
		},
	})

	res, err := h(ctx, unpeerInstruction(true))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != brokerapi.ResultStatusSucceeded {
		t.Errorf("want Succeeded, got %s", res.Status)
	}
	if res.Payload.Kind != brokerapi.PayloadKindUnpeer {
		t.Errorf("want UnpeerPayload, got %s", res.Payload.Kind)
	}
	if !res.Payload.TunnelDropped {
		t.Errorf("want TunnelDropped=true on LastChunk, got false")
	}

	// ResourceSlice is per-reservation and gets deleted.
	if exists, _ := objectExists(ctx, c, resourceSliceGVK, testNamespace, "rs-"+testResID); exists {
		t.Error("ResourceSlice still present after Unpeer")
	}
	// VirtualNodeState CR gone.
	if _, err := getVirtualNodeState(ctx, c, testNamespace, testResID); err == nil {
		t.Error("VirtualNodeState still present after Unpeer")
	}
	// Kubeconfig Secret deleted (LastChunk=true).
	if err := c.Get(ctx, types.NamespacedName{
		Name: "kubeconfig-" + testResID, Namespace: testNamespace,
	}, &corev1.Secret{}); err == nil {
		t.Error("kubeconfig Secret should be gone after LastChunk Unpeer")
	}
	// ForeignCluster shell deleted (LastChunk=true) — otherwise liqoctl
	// info keeps reporting the peering with Authentication Healthy.
	if exists, _ := objectExists(ctx, c, foreignClusterGVK, "", "liqo-provider-1"); exists {
		t.Error("ForeignCluster should be gone after LastChunk Unpeer")
	}
	if !ranLiqoctl.Load() {
		t.Error("liqoctl unpeer was not invoked")
	}
}

func TestUnpeer_KubeconfigKept_WhenNotLastChunk(t *testing.T) {
	ctx := context.Background()
	c := newFakeKubeClient()

	// Seed the kubeconfig Secret directly so Unpeer has something to
	// reach for. ResourceSlice and NamespaceOffloading aren't required
	// for this test — Unpeer must tolerate them being absent.
	_ = c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubeconfig-" + testResID, Namespace: testNamespace,
		},
		Data: map[string][]byte{KubeconfigSecretDataKey: []byte("apiVersion: v1")},
	})
	// Seed the ForeignCluster too — it must survive a non-last-chunk unpeer
	// since siblings to the same provider still share it.
	fc := &unstructured.Unstructured{}
	fc.SetGroupVersionKind(foreignClusterGVK)
	fc.SetName("liqo-provider-1")
	_ = c.Create(ctx, fc)

	var ranLiqoctl bool
	h := NewUnpeerHandler(UnpeerConfig{
		LocalClient: c,
		Namespace:   testNamespace,
		Run: func(_ context.Context, _ string, _ ...string) ([]byte, []byte, error) {
			ranLiqoctl = true
			return nil, nil, nil
		},
	})

	res, err := h(ctx, unpeerInstruction(false))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Payload.TunnelDropped {
		t.Errorf("want TunnelDropped=false when !LastChunk")
	}
	// `liqoctl unpeer` must NOT run: it tears down the networking, auth and
	// gateway that EVERY chunk borrowed from this provider rides on, so running
	// it here would strand the sibling reservations still holding the provider.
	if ranLiqoctl {
		t.Error("liqoctl unpeer must not run while siblings still hold this provider")
	}
	// ForeignCluster retained when !LastChunk — it is shared per provider.
	if exists, _ := objectExists(ctx, c, foreignClusterGVK, "", "liqo-provider-1"); !exists {
		t.Error("ForeignCluster should persist when !LastChunk")
	}
	// The kubeconfig Secret, by contrast, is named per RESERVATION
	// (kubeconfig-<reservationID>) and is useless once this handler has run, so
	// it is dropped unconditionally. Keeping it would leak one Secret for every
	// non-last reservation, since the normal Unpeer -> Released path never emits
	// a Cleanup instruction to collect it.
	if err := c.Get(ctx, types.NamespacedName{
		Name: "kubeconfig-" + testResID, Namespace: testNamespace,
	}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("per-reservation kubeconfig Secret should be deleted even when !LastChunk; got %v", err)
	}
}

// -----------------------------------------------------------------------------
// Idempotency
// -----------------------------------------------------------------------------

func TestUnpeer_AlreadyGone_IsSuccess(t *testing.T) {
	c := newFakeKubeClient()
	h := NewUnpeerHandler(UnpeerConfig{
		LocalClient: c,
		Namespace:   testNamespace,
		Run:         stubRun("", nil),
	})
	// No prior peer state. Unpeer must still succeed.
	res, err := h(context.Background(), unpeerInstruction(true))
	if err != nil {
		t.Fatalf("Unpeer with no prior state should succeed; got %v", err)
	}
	if res.Status != brokerapi.ResultStatusSucceeded {
		t.Errorf("want Succeeded, got %s", res.Status)
	}
}

func TestUnpeer_LiqoctlAlreadyUnpeeredStderr_TreatedAsSuccess(t *testing.T) {
	ctx := context.Background()
	c := newFakeKubeClient()
	// Seed kubeconfig so liqoctl path is exercised.
	_ = c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubeconfig-" + testResID, Namespace: testNamespace,
		},
		Data: map[string][]byte{KubeconfigSecretDataKey: []byte("kc")},
	})

	h := NewUnpeerHandler(UnpeerConfig{
		LocalClient: c,
		Namespace:   testNamespace,
		Run:         stubRun("Error: not peered with foo", errors.New("exit status 1")),
	})

	res, err := h(ctx, unpeerInstruction(true))
	if err != nil {
		t.Fatalf("'not peered' stderr should be a success; got %v", err)
	}
	if res.Status != brokerapi.ResultStatusSucceeded {
		t.Errorf("want Succeeded, got %s", res.Status)
	}
}

func TestUnpeer_LiqoctlHardFailure_Propagates(t *testing.T) {
	ctx := context.Background()
	c := newFakeKubeClient()
	_ = c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubeconfig-" + testResID, Namespace: testNamespace,
		},
		Data: map[string][]byte{KubeconfigSecretDataKey: []byte("kc")},
	})

	h := NewUnpeerHandler(UnpeerConfig{
		LocalClient: c,
		Namespace:   testNamespace,
		Run:         stubRun("tunnel teardown crashed", errors.New("exit status 1")),
	})

	_, err := h(ctx, unpeerInstruction(true))
	if err == nil {
		t.Fatal("expected error from hard failure")
	}
	if !strings.Contains(err.Error(), "tunnel teardown crashed") {
		t.Errorf("error should surface stderr; got %q", err.Error())
	}
}

// -----------------------------------------------------------------------------
// Validation
// -----------------------------------------------------------------------------

func TestUnpeer_RejectsWrongKind(t *testing.T) {
	h := NewUnpeerHandler(UnpeerConfig{
		LocalClient: newFakeKubeClient(),
		Namespace:   testNamespace,
		Run:         stubRun("", nil),
	})
	in := unpeerInstruction(true)
	in.Kind = kindPeer
	_, err := h(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "unexpected kind") {
		t.Fatalf("want unexpected-kind error, got %v", err)
	}
}

func TestUnpeer_RejectsNilInstruction(t *testing.T) {
	h := NewUnpeerHandler(UnpeerConfig{
		LocalClient: newFakeKubeClient(),
		Namespace:   testNamespace,
		Run:         stubRun("", nil),
	})
	if _, err := h(context.Background(), nil); err == nil {
		t.Fatal("want error for nil instruction")
	}
}
