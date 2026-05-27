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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

const (
	testNamespace = "default"
	testResID     = "res-1"
	// kindPeer is used by sibling tests for wrong-kind rejection.
	kindPeer = "Peer"
)

func newFakeKubeClient() ctrlclient.Client {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = autoscalingv1alpha1.AddToScheme(scheme)
	return clientfake.NewClientBuilder().WithScheme(scheme).Build()
}

// stubRun returns a RunFunc that yields a fixed (stderr, err) pair on
// every invocation. stdout is unused by the consumer handlers (Peer
// and Unpeer don't read liqoctl's stdout), so the helper omits it.
func stubRun(stderr string, err error) RunFunc {
	return func(_ context.Context, _ string, _ ...string) ([]byte, []byte, error) {
		return nil, []byte(stderr), err
	}
}

func peerInstruction() *brokerapi.InstructionView {
	return &brokerapi.InstructionView{
		ID:                    "peer-res-1",
		Kind:                  string(autoscalingv1alpha1.ReservationInstructionPeer),
		ReservationID:         testResID,
		ProviderClusterID:     "provider-1",
		ProviderLiqoClusterID: "liqo-provider-1",
		Kubeconfig:            "apiVersion: v1\nkind: Config",
		ResourceSliceResources: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	}
}

// -----------------------------------------------------------------------------
// Happy path
// -----------------------------------------------------------------------------

func TestPeer_HappyPath_PersistsSecretRunsLiqoctlAndCreatesCRs(t *testing.T) {
	c := newFakeKubeClient()
	var ranLiqoctl bool
	h := NewPeerHandler(PeerConfig{
		LocalClient: c,
		Namespace:   testNamespace,
		Run: func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
			ranLiqoctl = true
			if len(args) < 1 || args[0] != "peer" {
				t.Errorf("expected liqoctl peer, got %v", args)
			}
			if len(args) < 3 || args[1] != "--remote-kubeconfig" {
				t.Errorf("expected --remote-kubeconfig flag, got %v", args)
			}
			return nil, nil, nil
		},
	})

	res, err := h(context.Background(), peerInstruction())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != brokerapi.ResultStatusSucceeded {
		t.Fatalf("want Succeeded, got %s", res.Status)
	}
	if res.Payload == nil || res.Payload.Kind != brokerapi.PayloadKindPeer {
		t.Fatalf("want PeerPayload, got %+v", res.Payload)
	}
	if len(res.Payload.ResourceSliceNames) != 1 ||
		res.Payload.ResourceSliceNames[0] != "rs-"+testResID {
		t.Errorf("ResourceSliceNames: want [rs-res-1], got %v", res.Payload.ResourceSliceNames)
	}
	if !ranLiqoctl {
		t.Error("liqoctl peer was not invoked")
	}

	// Kubeconfig Secret persisted.
	sec := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: "kubeconfig-" + testResID, Namespace: testNamespace,
	}, sec); err != nil {
		t.Fatalf("kubeconfig secret not persisted: %v", err)
	}
	if string(sec.Data[KubeconfigSecretDataKey]) != "apiVersion: v1\nkind: Config" {
		t.Errorf("kubeconfig bytes mismatch: %q", sec.Data[KubeconfigSecretDataKey])
	}

	// ResourceSlice created.
	if exists, _ := objectExists(context.Background(), c, resourceSliceGVK, testNamespace, "rs-"+testResID); !exists {
		t.Error("ResourceSlice was not created")
	}
	// VirtualNodeState CR created with the right spec.
	vns, err := getVirtualNodeState(context.Background(), c, testNamespace, testResID)
	if err != nil {
		t.Fatalf("VirtualNodeState not created: %v", err)
	}
	if vns.Spec.ProviderClusterID != "provider-1" {
		t.Errorf("ProviderClusterID: want provider-1, got %q", vns.Spec.ProviderClusterID)
	}
	if vns.Spec.ReservationID != testResID {
		t.Errorf("ReservationID: want %q, got %q", testResID, vns.Spec.ReservationID)
	}
	if vns.Spec.NodeGroupID != "ng-provider-1-standard" {
		t.Errorf("NodeGroupID: want ng-provider-1-standard, got %q", vns.Spec.NodeGroupID)
	}
	if vns.Labels[VirtualNodeStateReservationLabel] != testResID {
		t.Errorf("reservation label: want %q, got %q", testResID, vns.Labels[VirtualNodeStateReservationLabel])
	}
}

// -----------------------------------------------------------------------------
// Idempotency
// -----------------------------------------------------------------------------

func TestPeer_RerunIsIdempotent(t *testing.T) {
	c := newFakeKubeClient()
	h := NewPeerHandler(PeerConfig{
		LocalClient: c,
		Namespace:   testNamespace,
		Run:         stubRun("", nil),
	})

	if _, err := h(context.Background(), peerInstruction()); err != nil {
		t.Fatal(err)
	}
	// Second invocation must succeed without error.
	res, err := h(context.Background(), peerInstruction())
	if err != nil {
		t.Fatalf("second invocation should be idempotent; got %v", err)
	}
	if res.Status != brokerapi.ResultStatusSucceeded {
		t.Errorf("want Succeeded on re-run, got %s", res.Status)
	}
}

// -----------------------------------------------------------------------------
// Failure paths
// -----------------------------------------------------------------------------

func TestPeer_LiqoctlFailure_SurfacesStderr(t *testing.T) {
	c := newFakeKubeClient()
	h := NewPeerHandler(PeerConfig{
		LocalClient: c,
		Namespace:   testNamespace,
		Run:         stubRun("tunnel setup blew up", errors.New("exit status 1")),
	})

	_, err := h(context.Background(), peerInstruction())
	if err == nil {
		t.Fatal("expected error from liqoctl failure")
	}
	if !strings.Contains(err.Error(), "tunnel setup blew up") {
		t.Errorf("error should surface stderr; got %q", err.Error())
	}
}

func TestPeer_RejectsWrongKind(t *testing.T) {
	h := NewPeerHandler(PeerConfig{
		LocalClient: newFakeKubeClient(),
		Namespace:   testNamespace,
		Run:         stubRun("", nil),
	})
	in := peerInstruction()
	in.Kind = "Unpeer"
	_, err := h(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "unexpected kind") {
		t.Fatalf("want unexpected-kind error, got %v", err)
	}
}

func TestPeer_RejectsMissingFields(t *testing.T) {
	mkH := func() func(*brokerapi.InstructionView) error {
		h := NewPeerHandler(PeerConfig{
			LocalClient: newFakeKubeClient(),
			Namespace:   testNamespace,
			Run:         stubRun("", nil),
		})
		return func(in *brokerapi.InstructionView) error {
			_, err := h(context.Background(), in)
			return err
		}
	}

	cases := []struct {
		name   string
		mutate func(in *brokerapi.InstructionView)
		want   string
	}{
		{"nil instruction", nil, "nil instruction"},
		{"missing kubeconfig", func(in *brokerapi.InstructionView) { in.Kubeconfig = "" }, "missing inline kubeconfig"},
		{"missing provider liqo id", func(in *brokerapi.InstructionView) { in.ProviderLiqoClusterID = "" }, "providerLiqoClusterId"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := mkH()
			var in *brokerapi.InstructionView
			if tc.mutate != nil {
				in = peerInstruction()
				tc.mutate(in)
			}
			err := run(in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestPeer_RejectsMissingLocalClient(t *testing.T) {
	h := NewPeerHandler(PeerConfig{
		Namespace: testNamespace,
		Run:       stubRun("", nil),
	})
	_, err := h(context.Background(), peerInstruction())
	if err == nil || !strings.Contains(err.Error(), "LocalClient is nil") {
		t.Fatalf("want LocalClient-nil error, got %v", err)
	}
}

func TestPeer_RejectsMissingNamespace(t *testing.T) {
	h := NewPeerHandler(PeerConfig{
		LocalClient: newFakeKubeClient(),
		Run:         stubRun("", nil),
	})
	_, err := h(context.Background(), peerInstruction())
	if err == nil || !strings.Contains(err.Error(), "Namespace is required") {
		t.Fatalf("want namespace-required error, got %v", err)
	}
}
