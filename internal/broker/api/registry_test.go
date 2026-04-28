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
	r.Touch(consumerCluster, "liqo-a-1")

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

	// Overwrite semantics: same clusterID with new liqo id wins.
	r.Touch(consumerCluster, "liqo-a-2")
	got, _ = r.Lookup(consumerCluster)
	if got.LiqoClusterID != "liqo-a-2" {
		t.Errorf("Touch must overwrite; got %q", got.LiqoClusterID)
	}

	// Concurrent stress: many writers + readers, no panic / race / corruption.
	var wg sync.WaitGroup
	const writers = 16
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Touch("consumer-b", "liqo-b")
			_, _ = r.Lookup(consumerCluster)
		}()
	}
	wg.Wait()
	if _, ok := r.Lookup("consumer-b"); !ok {
		t.Errorf("expected consumer-b to be present after concurrent Touch")
	}
}
