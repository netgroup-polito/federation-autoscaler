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
	"sync"
	"testing"
	"time"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
)

// TestConsumerRegistry exercises Touch/Lookup, the overwrite semantics, and
// concurrent access (the RW mutex inside the registry is the contract that
// matters most here).
func TestConsumerRegistry(t *testing.T) {
	r := NewConsumerRegistry()

	if _, ok := r.Lookup("missing"); ok {
		t.Fatalf("Lookup on empty registry must return ok=false")
	}

	before := time.Now()
	r.Touch(consumerCluster, "liqo-a-1", autoscalingv1alpha1.PlacementPolicy{}, "", "", nil, nil)

	got, ok := r.Lookup(consumerCluster)
	if !ok {
		t.Fatalf("Lookup after Touch must return ok=true")
	}
	if got.ClusterID != consumerCluster || got.LiqoClusterID != "liqo-a-1" {
		t.Errorf("entry mismatch: got %+v", got)
	}
	if got.LastSeen.Before(before) {
		t.Errorf("LastSeen %v should be >= before %v", got.LastSeen, before)
	}
	if got.Placement.Type != "" {
		t.Errorf("default Placement.Type should be empty; got %q", got.Placement.Type)
	}

	// Overwrite semantics: same clusterID with new liqo id + a price policy +
	// a location wins.
	lat, lon := 45.5, -73.5
	r.Touch(consumerCluster, "liqo-a-2",
		autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyPrice},
		"QC", "", &lat, &lon)
	got, _ = r.Lookup(consumerCluster)
	if got.LiqoClusterID != "liqo-a-2" {
		t.Errorf("Touch must overwrite; got %q", got.LiqoClusterID)
	}
	if got.Placement.Type != autoscalingv1alpha1.PlacementStrategyPrice {
		t.Errorf("Touch must store the placement policy; got %q", got.Placement.Type)
	}
	if got.Region != "QC" || !got.HasLocation || got.Latitude != lat || got.Longitude != lon {
		t.Errorf("Touch must store region + location; got region=%q hasLoc=%v lat=%v lon=%v",
			got.Region, got.HasLocation, got.Latitude, got.Longitude)
	}

	// A Touch with nil coordinates clears HasLocation (no stale location).
	r.Touch(consumerCluster, "liqo-a-3", autoscalingv1alpha1.PlacementPolicy{}, "QC", "", nil, nil)
	if got, _ = r.Lookup(consumerCluster); got.HasLocation {
		t.Errorf("Touch with nil coords must leave HasLocation=false; got %+v", got)
	}

	// Concurrent stress: many writers + readers, no panic / race / corruption.
	var wg sync.WaitGroup
	const writers = 16
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Touch("consumer-b", "liqo-b", autoscalingv1alpha1.PlacementPolicy{}, "", "", nil, nil)
			_, _ = r.Lookup(consumerCluster)
		}()
	}
	wg.Wait()
	if _, ok := r.Lookup("consumer-b"); !ok {
		t.Errorf("expected consumer-b to be present after concurrent Touch")
	}
}

// TestConsumerRegistrySnapshot verifies the read-only accessor the dashboard
// relies on: it returns every entry, sorted by ClusterID, as copies (so the
// caller cannot mutate registry state), and is safe to call concurrently with
// Touch.
func TestConsumerRegistrySnapshot(t *testing.T) {
	r := NewConsumerRegistry()

	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("Snapshot on empty registry len = %d, want 0", len(got))
	}

	// Insert out of order; Snapshot must return them sorted.
	r.Touch("consumer-c", "liqo-c", autoscalingv1alpha1.PlacementPolicy{}, "", "", nil, nil)
	r.Touch(consumerCluster, "liqo-a", autoscalingv1alpha1.PlacementPolicy{}, "", "", nil, nil)
	r.Touch("consumer-b", "liqo-b", autoscalingv1alpha1.PlacementPolicy{}, "", "", nil, nil)

	got := r.Snapshot()
	if len(got) != 3 {
		t.Fatalf("Snapshot len = %d, want 3", len(got))
	}
	if got[0].ClusterID != consumerCluster || got[1].ClusterID != "consumer-b" || got[2].ClusterID != "consumer-c" {
		t.Errorf("Snapshot not sorted by ClusterID: %+v", got)
	}

	// Entries are copies: mutating the returned slice must not touch state.
	got[0].LiqoClusterID = "mutated"
	if e, _ := r.Lookup(consumerCluster); e.LiqoClusterID != "liqo-a" {
		t.Errorf("Snapshot returned an aliased entry; registry mutated to %q", e.LiqoClusterID)
	}

	// Concurrent Snapshot alongside Touch must not race or panic.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Touch("consumer-x", "liqo-x", autoscalingv1alpha1.PlacementPolicy{}, "", "", nil, nil)
			_ = r.Snapshot()
		}()
	}
	wg.Wait()
}
