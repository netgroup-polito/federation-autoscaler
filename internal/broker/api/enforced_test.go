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

package api

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

func TestStaleEnforced(t *testing.T) {
	now := time.Now()
	s := newStaleEnforced()
	s.record("ri/ns/peer-1", "100", now)

	if !s.isStale("ri/ns/peer-1", "100", now) {
		t.Error("the copy with the replaced version is the stale one and must be hidden")
	}
	if !s.isStale("ri/ns/peer-1", "100", now.Add(time.Second)) {
		t.Error("it stays hidden on every poll until the cache catches up")
	}
	if s.isStale("ri/ns/peer-1", "101", now) {
		t.Error("a newer version (cache caught up, or re-armed) must be shown")
	}
	if s.isStale("ri/ns/peer-1", "100", now) {
		t.Error("once a newer version was seen the entry is gone")
	}
	if s.isStale("ri/ns/other", "100", now) {
		t.Error("an unrecorded instruction is never hidden")
	}

	s.record("pi/ns/gk-1", "7", now)
	if s.isStale("pi/ns/gk-1", "7", now.Add(enforcedForgetAfter+time.Second)) {
		t.Error("entries are forgotten after enforcedForgetAfter")
	}

	var nilSet *staleEnforced
	nilSet.record("k", "1", now)
	if nilSet.isStale("k", "1", now) {
		t.Error("a nil set hides nothing")
	}
}

// staleListClient serves List from a frozen snapshot, like an informer cache
// that has not yet seen the latest write; every other call goes to the
// underlying (up to date) client.
type staleListClient struct {
	client.Client
	snapshot *autoscalingv1alpha1.ReservationInstructionList
}

func (c staleListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if l, ok := list.(*autoscalingv1alpha1.ReservationInstructionList); ok {
		c.snapshot.DeepCopyInto(l)
		return nil
	}
	return c.Client.List(ctx, list, opts...)
}

// TestInstructionNotRedeliveredFromStaleCache: once a result marks an
// instruction Enforced, a poll answered from a cache that still holds the
// un-Enforced copy must not hand the instruction out again.
func TestInstructionNotRedeliveredFromStaleCache(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = autoscalingv1alpha1.AddToScheme(scheme)
	_ = brokerv1alpha1.AddToScheme(scheme)
	ri := &autoscalingv1alpha1.ReservationInstruction{
		ObjectMeta: metav1.ObjectMeta{Name: "peer-res-1", Namespace: "fa"},
		Spec: autoscalingv1alpha1.ReservationInstructionSpec{
			ReservationID: "res-1", Kind: autoscalingv1alpha1.ReservationInstructionUnpeer,
			TargetClusterID: "consumer-1", ProviderClusterID: "provider-1",
		},
	}
	live := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ri).
		WithStatusSubresource(&autoscalingv1alpha1.ReservationInstruction{}).Build()

	var stored autoscalingv1alpha1.ReservationInstruction
	if err := live.Get(context.Background(), client.ObjectKeyFromObject(ri), &stored); err != nil {
		t.Fatal(err)
	}
	stale := &autoscalingv1alpha1.ReservationInstructionList{Items: []autoscalingv1alpha1.ReservationInstruction{*stored.DeepCopy()}}

	s := NewServer(ServerOptions{Client: staleListClient{Client: live, snapshot: stale}, Namespace: "fa"})

	views, err := s.collectInstructionsForCaller(context.Background(), "consumer-1")
	if err != nil || len(views) != 1 {
		t.Fatalf("before the result the instruction is delivered: %d views, %v", len(views), err)
	}

	// The result handler reads the instruction (the poll above touched its
	// status, so re-read it), and the cache still serves that same copy.
	if err := live.Get(context.Background(), client.ObjectKeyFromObject(ri), &stored); err != nil {
		t.Fatal(err)
	}
	stale.Items = []autoscalingv1alpha1.ReservationInstruction{*stored.DeepCopy()}
	if err := s.markReservationInstructionEnforced(context.Background(), &stored, &InstructionResultRequest{}); err != nil {
		t.Fatal(err)
	}
	views, err = s.collectInstructionsForCaller(context.Background(), "consumer-1")
	if err != nil || len(views) != 0 {
		t.Fatalf("the stale cached copy must not be redelivered after the result: %d views, %v", len(views), err)
	}

	// The instruction is re-armed later (a new version, un-Enforced): it must
	// be delivered again.
	rearmed := stored.DeepCopy()
	rearmed.ResourceVersion = "999999"
	stale.Items = []autoscalingv1alpha1.ReservationInstruction{*rearmed}
	views, err = s.collectInstructionsForCaller(context.Background(), "consumer-1")
	if err != nil || len(views) != 1 {
		t.Fatalf("a re-armed instruction must be delivered: %d views, %v", len(views), err)
	}
}
