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
	"sort"
	"sync"
	"time"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
)

// ConsumerEntry is one row of ConsumerRegistry, captured the last time the
// Consumer Agent called POST /api/v1/heartbeat.
type ConsumerEntry struct {
	ClusterID     string
	LiqoClusterID string
	LastSeen      time.Time

	// Placement is the consumer's most recently heartbeated placement policy.
	// The zero value (empty Type) means the Broker default — no price
	// preference. Stored by value: PlacementPolicy has no reference fields, so
	// Snapshot copies stay safe to hand to the read-only dashboard.
	Placement autoscalingv1alpha1.PlacementPolicy

	// Region is the consumer's most recently heartbeated region (may be empty);
	// informational, surfaced on the dashboard.
	Region string

	// City is the consumer's most recently heartbeated city (may be empty);
	// informational, surfaced on the dashboard alongside Region.
	City string

	// MeasuredLatencies is the consumer's most recent measured RTT (ms) per
	// provider from UDP-probing the latency shortlist; ChosenProvider is the
	// lowest-RTT provider it grew. Both informational (the consumer already
	// re-masked locally), surfaced on the dashboard. Empty when the consumer isn't
	// using the latency strategy or hasn't probed yet.
	MeasuredLatencies map[string]float64
	ChosenProvider    string

	// Latitude/Longitude are the consumer's coordinates (decimal degrees), valid
	// only when HasLocation is true. Used by the latency placement strategy to
	// rank providers by great-circle distance to this consumer. Stored as values
	// (not pointers) so ConsumerEntry stays reference-free and Snapshot copies
	// remain safe for the read-only dashboard.
	Latitude    float64
	Longitude   float64
	HasLocation bool
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

// Touch records (or refreshes) one consumer's identity, placement policy, and
// (optionally) its geographic location. Safe for concurrent use; later calls
// overwrite earlier ones, which is fine because the most recent heartbeat is by
// definition the most accurate. lat/lon are accepted as pointers (the heartbeat
// wire shape) and stored by value with HasLocation set only when both arrive.
func (r *ConsumerRegistry) Touch(clusterID, liqoClusterID string, placement autoscalingv1alpha1.PlacementPolicy, region, city string, lat, lon *float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := ConsumerEntry{
		ClusterID:     clusterID,
		LiqoClusterID: liqoClusterID,
		LastSeen:      time.Now(),
		Placement:     placement,
		Region:        region,
		City:          city,
	}
	if lat != nil && lon != nil {
		entry.Latitude = *lat
		entry.Longitude = *lon
		entry.HasLocation = true
	}
	r.entries[clusterID] = entry
}

// SetMeasuredLatency records the consumer's most recent measured-latency result
// on its existing entry (informational, for the dashboard). Called right after
// Touch in the heartbeat handler, so the entry exists; a no-op if it doesn't. The
// map is stored by reference but never mutated in place (each heartbeat brings a
// fresh map), so Snapshot copies stay safe.
func (r *ConsumerRegistry) SetMeasuredLatency(clusterID string, rtts map[string]float64, chosen string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[clusterID]
	if !ok {
		return
	}
	entry.MeasuredLatencies = rtts
	entry.ChosenProvider = chosen
	r.entries[clusterID] = entry
}

// Lookup returns the entry for clusterID and true, or the zero value and
// false if no heartbeat has been recorded.
func (r *ConsumerRegistry) Lookup(clusterID string) (ConsumerEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[clusterID]
	return e, ok
}

// Snapshot returns a copy of every ConsumerEntry recorded since this Broker
// pod started, sorted by ClusterID for stable rendering (the read-only
// dashboard polls this every couple of seconds). Safe for concurrent use:
// it takes only the read lock and returns value copies, so callers cannot
// mutate registry state — ConsumerEntry has no reference fields.
func (r *ConsumerRegistry) Snapshot() []ConsumerEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ConsumerEntry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClusterID < out[j].ClusterID })
	return out
}
