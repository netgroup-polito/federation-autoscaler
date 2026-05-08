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

func cleanupInstruction(consumerID string) *brokerapi.InstructionView {
	return &brokerapi.InstructionView{
		ID:                "cleanup-res-1",
		Kind:              string(autoscalingv1alpha1.ProviderInstructionCleanup),
		ReservationID:     "res-1",
		ConsumerClusterID: consumerID,
	}
}

func TestCleanup_HappyPath(t *testing.T) {
	h := NewCleanupHandler(CleanupConfig{
		Run: stubRun("", "", nil),
	})
	res, err := h(context.Background(), cleanupInstruction("consumer-1"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil || res.Status != brokerapi.ResultStatusSucceeded {
		t.Fatalf("want Succeeded, got %+v", res)
	}
	if res.Payload != nil {
		t.Errorf("Cleanup result should have no payload, got %+v", res.Payload)
	}
}

func TestCleanup_PassesArgsToLiqoctl(t *testing.T) {
	var seenArgs []string
	h := NewCleanupHandler(CleanupConfig{
		LiqoctlPath: "/fake/liqoctl",
		Run: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
			seenArgs = args
			return nil, nil, nil
		},
	})
	if _, err := h(context.Background(), cleanupInstruction("consumer-x")); err != nil {
		t.Fatal(err)
	}
	want := []string{"delete", "peering-user", "--consumer-cluster-id", "consumer-x"}
	if len(seenArgs) != len(want) {
		t.Fatalf("args length: want %d, got %d (%v)", len(want), len(seenArgs), seenArgs)
	}
	for i, w := range want {
		if seenArgs[i] != w {
			t.Errorf("args[%d]: want %q, got %q", i, w, seenArgs[i])
		}
	}
}

func TestCleanup_NotFoundStderr_TreatedAsSuccess(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
	}{
		{"lowercase 'not found'", "Error: peering-user 'consumer-1' not found"},
		{"'does not exist'", "the resource does not exist on this cluster"},
		{"'NotFound'", "Error from server (NotFound): serviceaccounts \"x\" not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewCleanupHandler(CleanupConfig{
				Run: stubRun("", tc.stderr, errors.New("exit status 1")),
			})
			res, err := h(context.Background(), cleanupInstruction("consumer-1"))
			if err != nil {
				t.Fatalf("not-found stderr should be a success; got err %v", err)
			}
			if res.Status != brokerapi.ResultStatusSucceeded {
				t.Errorf("want Succeeded, got %s", res.Status)
			}
		})
	}
}

func TestCleanup_HardFailure_PropagatesStderr(t *testing.T) {
	h := NewCleanupHandler(CleanupConfig{
		Run: stubRun("", "permission denied: insufficient RBAC", errors.New("exit status 1")),
	})
	_, err := h(context.Background(), cleanupInstruction("consumer-1"))
	if err == nil {
		t.Fatal("expected error from hard failure")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error should surface stderr; got %q", err.Error())
	}
}

func TestCleanup_RejectsWrongKind(t *testing.T) {
	h := NewCleanupHandler(CleanupConfig{
		Run: stubRun("", "", nil),
	})
	in := cleanupInstruction("c")
	in.Kind = "GenerateKubeconfig"
	_, err := h(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "unexpected kind") {
		t.Fatalf("want unexpected-kind error, got %v", err)
	}
}

func TestCleanup_RejectsMissingConsumerClusterID(t *testing.T) {
	h := NewCleanupHandler(CleanupConfig{
		Run: stubRun("", "", nil),
	})
	_, err := h(context.Background(), cleanupInstruction(""))
	if err == nil || !strings.Contains(err.Error(), "consumerClusterId") {
		t.Fatalf("want missing-consumerClusterId error, got %v", err)
	}
}

func TestCleanup_RejectsNilInstruction(t *testing.T) {
	h := NewCleanupHandler(CleanupConfig{
		Run: stubRun("", "", nil),
	})
	if _, err := h(context.Background(), nil); err == nil {
		t.Fatal("want error for nil instruction")
	}
}
