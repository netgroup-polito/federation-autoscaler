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

package heartbeat

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
)

const policyTestNS = "federation-autoscaler-system"

func policyFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := autoscalingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func pricePolicyObj(name string) *autoscalingv1alpha1.ConsumerPolicy {
	return &autoscalingv1alpha1.ConsumerPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: policyTestNS},
		Spec: autoscalingv1alpha1.ConsumerPolicySpec{
			Placement: autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyPrice},
		},
	}
}

func TestCurrentPlacement(t *testing.T) {
	t.Run("nil local client → nil", func(t *testing.T) {
		h := &Heartbeater{log: logr.Discard()}
		if got := h.currentPlacement(context.Background()); got != nil {
			t.Errorf("nil localClient must return nil; got %+v", got)
		}
	})

	t.Run("no ConsumerPolicy → nil (Broker default)", func(t *testing.T) {
		h := &Heartbeater{log: logr.Discard(), localClient: policyFakeClient(t), namespace: policyTestNS}
		if got := h.currentPlacement(context.Background()); got != nil {
			t.Errorf("missing ConsumerPolicy must return nil; got %+v", got)
		}
	})

	t.Run("price ConsumerPolicy → Price placement", func(t *testing.T) {
		h := &Heartbeater{
			log:         logr.Discard(),
			localClient: policyFakeClient(t, pricePolicyObj("default")),
			namespace:   policyTestNS,
		}
		got := h.currentPlacement(context.Background())
		if got == nil || got.Type != autoscalingv1alpha1.PlacementStrategyPrice {
			t.Errorf("want Price placement; got %+v", got)
		}
	})

	t.Run("multiple policies → deterministic lowest name", func(t *testing.T) {
		// "aaa" has no type; "bbb" is Price. Lowest name ("aaa") wins.
		aaa := &autoscalingv1alpha1.ConsumerPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "aaa", Namespace: policyTestNS},
		}
		h := &Heartbeater{
			log:         logr.Discard(),
			localClient: policyFakeClient(t, aaa, pricePolicyObj("bbb")),
			namespace:   policyTestNS,
		}
		got := h.currentPlacement(context.Background())
		if got == nil || got.Type != "" {
			t.Errorf("lowest-name policy (empty type) must win; got %+v", got)
		}
	})
}

// TestHeartbeater_PostsPlacementPolicy proves the policy travels end-to-end on
// the wire: the heartbeat body the Broker receives carries the Price placement.
func TestHeartbeater_PostsPlacementPolicy(t *testing.T) {
	fb := newFakeBroker(t)
	h, err := New(Options{
		Client:        fb.buildClient(t),
		ClusterID:     "c",
		LiqoClusterID: "l",
		LocalClient:   policyFakeClient(t, pricePolicyObj("default")),
		Namespace:     policyTestNS,
		Interval:      time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	posted := fb.snapshotPosted()
	if len(posted) != 1 {
		t.Fatalf("want 1 heartbeat, got %d", len(posted))
	}
	if posted[0].Placement == nil || posted[0].Placement.Type != autoscalingv1alpha1.PlacementStrategyPrice {
		t.Errorf("heartbeat must carry the Price placement; got %+v", posted[0].Placement)
	}
}
