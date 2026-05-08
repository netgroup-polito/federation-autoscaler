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

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// stubRun produces a closed-over RunFunc with controllable outputs.
func stubRun(stdout, stderr string, err error) RunFunc {
	return func(_ context.Context, _ string, _ ...string) ([]byte, []byte, error) {
		return []byte(stdout), []byte(stderr), err
	}
}

func gkInstruction(consumerID string) *brokerapi.InstructionView {
	return &brokerapi.InstructionView{
		ID:                "gk-res-1",
		Kind:              string(autoscalingv1alpha1.ProviderInstructionGenerateKubeconfig),
		ReservationID:     "res-1",
		ConsumerClusterID: consumerID,
	}
}

// -----------------------------------------------------------------------------
// Happy path
// -----------------------------------------------------------------------------

func TestGenerateKubeconfig_HappyPath_ReturnsKubeconfigPayload(t *testing.T) {
	const fake = "apiVersion: v1\nkind: Config\nclusters: []"
	h := NewGenerateKubeconfigHandler(GenerateKubeconfigConfig{
		Run: stubRun(fake, "", nil),
	})

	res, err := h(context.Background(), gkInstruction("consumer-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.Status != brokerapi.ResultStatusSucceeded {
		t.Fatalf("want Succeeded result, got %+v", res)
	}
	if res.Payload == nil || res.Payload.Kind != brokerapi.PayloadKindKubeconfig {
		t.Fatalf("want KubeconfigPayload, got %+v", res.Payload)
	}
	if res.Payload.Kubeconfig != fake {
		t.Errorf("kubeconfig: want %q, got %q", fake, res.Payload.Kubeconfig)
	}
}

func TestGenerateKubeconfig_TrimsWhitespace(t *testing.T) {
	const trimmed = "apiVersion: v1\nclusters: []"
	h := NewGenerateKubeconfigHandler(GenerateKubeconfigConfig{
		Run: stubRun("\n  "+trimmed+"  \n\n", "", nil),
	})

	res, err := h(context.Background(), gkInstruction("c"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Payload.Kubeconfig != trimmed {
		t.Errorf("kubeconfig should be trimmed; got %q", res.Payload.Kubeconfig)
	}
}

// -----------------------------------------------------------------------------
// Argument plumbing
// -----------------------------------------------------------------------------

func TestGenerateKubeconfig_PassesConsumerClusterIDToLiqoctl(t *testing.T) {
	var seenName string
	var seenArgs []string
	h := NewGenerateKubeconfigHandler(GenerateKubeconfigConfig{
		LiqoctlPath: "/fake/liqoctl",
		Run: func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
			seenName = name
			seenArgs = args
			return []byte("kubeconfig-bytes"), nil, nil
		},
	})

	if _, err := h(context.Background(), gkInstruction("consumer-xyz")); err != nil {
		t.Fatal(err)
	}
	if seenName != "/fake/liqoctl" {
		t.Errorf("liqoctl path: want /fake/liqoctl, got %q", seenName)
	}
	want := []string{"generate", "peering-user", "--consumer-cluster-id", "consumer-xyz"}
	if len(seenArgs) != len(want) {
		t.Fatalf("args length: want %d, got %d (%v)", len(want), len(seenArgs), seenArgs)
	}
	for i := range want {
		if seenArgs[i] != want[i] {
			t.Errorf("args[%d]: want %q, got %q", i, want[i], seenArgs[i])
		}
	}
}

// -----------------------------------------------------------------------------
// Failure modes
// -----------------------------------------------------------------------------

func TestGenerateKubeconfig_NonZeroExit_SurfaceStderr(t *testing.T) {
	h := NewGenerateKubeconfigHandler(GenerateKubeconfigConfig{
		Run: stubRun("", "Error from server: forbidden", errors.New("exit status 1")),
	})

	_, err := h(context.Background(), gkInstruction("c"))
	if err == nil {
		t.Fatal("expected error from non-zero exit")
	}
	msg := err.Error()
	if !strings.Contains(msg, "forbidden") {
		t.Errorf("error should surface stderr, got %q", msg)
	}
	if !strings.Contains(msg, "exit status 1") {
		t.Errorf("error should wrap the underlying exec err, got %q", msg)
	}
}

func TestGenerateKubeconfig_EmptyStdout_Fails(t *testing.T) {
	h := NewGenerateKubeconfigHandler(GenerateKubeconfigConfig{
		Run: stubRun("   \n\n  ", "", nil),
	})
	_, err := h(context.Background(), gkInstruction("c"))
	if err == nil || !strings.Contains(err.Error(), "empty kubeconfig") {
		t.Fatalf("want empty-kubeconfig error, got %v", err)
	}
}

func TestGenerateKubeconfig_RejectsWrongKind(t *testing.T) {
	h := NewGenerateKubeconfigHandler(GenerateKubeconfigConfig{
		Run: stubRun("ok", "", nil),
	})
	in := gkInstruction("c")
	in.Kind = "Cleanup"
	_, err := h(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "unexpected kind") {
		t.Fatalf("want unexpected-kind error, got %v", err)
	}
}

func TestGenerateKubeconfig_RejectsMissingConsumerClusterID(t *testing.T) {
	h := NewGenerateKubeconfigHandler(GenerateKubeconfigConfig{
		Run: stubRun("ok", "", nil),
	})
	_, err := h(context.Background(), gkInstruction(""))
	if err == nil || !strings.Contains(err.Error(), "consumerClusterId") {
		t.Fatalf("want missing-consumerClusterId error, got %v", err)
	}
}

func TestGenerateKubeconfig_RejectsNilInstruction(t *testing.T) {
	h := NewGenerateKubeconfigHandler(GenerateKubeconfigConfig{
		Run: stubRun("ok", "", nil),
	})
	_, err := h(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "nil instruction") {
		t.Fatalf("want nil-instruction error, got %v", err)
	}
}
