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
	"time"
)

// ConsumerEntry is one row of ConsumerRegistry, captured the last time the
// Consumer Agent called POST /api/v1/heartbeat.
type ConsumerEntry struct {
	ClusterID     string
	LiqoClusterID string
	LastSeen      time.Time
}

// ConsumerRegistry holds the in-memory mapping ClusterID → LiqoClusterID
// for every consumer cluster that has heartbeated since this Broker pod
// started. Reservation creation reads this map to populate
// Reservation.Spec.ConsumerLiqoClusterID — the only path through which a
// consumer's Liqo identity reaches the Broker.
//
// Persistence is intentionally omitted: a Broker restart loses the map but
// every consumer re-fills its own entry within the 15 s heartbeat cadence.
// If a reservation arrives in that window, it gets a 412 Precondition-
// Failed and the agent retries on the next CA tick.
type ConsumerRegistry struct {
	mu      sync.RWMutex
	entries map[string]ConsumerEntry
}

// NewConsumerRegistry returns an empty registry.
func NewConsumerRegistry() *ConsumerRegistry {
	return &ConsumerRegistry{entries: make(map[string]ConsumerEntry)}
}

// Touch records (or refreshes) one consumer's identity. Safe for concurrent
// use; later calls overwrite earlier ones, which is fine because the most
// recent heartbeat is by definition the most accurate.
func (r *ConsumerRegistry) Touch(clusterID, liqoClusterID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[clusterID] = ConsumerEntry{
		ClusterID:     clusterID,
		LiqoClusterID: liqoClusterID,
		LastSeen:      time.Now(),
	}
}

// Lookup returns the entry for clusterID and true, or the zero value and
// false if no heartbeat has been recorded.
func (r *ConsumerRegistry) Lookup(clusterID string) (ConsumerEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[clusterID]
	return e, ok
}
