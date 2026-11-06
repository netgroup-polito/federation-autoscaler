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

package store

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemoryStore keeps the catalog in memory. It exists so the interface can be
// exercised, and so handler tests can run without an API server or a
// database. It is not meant for production: everything is lost on restart.
type MemoryStore struct {
	mu       sync.RWMutex
	offers   map[string]Offer
	freshFor time.Duration

	// now is the clock, swappable in tests.
	now func() time.Time
}

// NewMemoryStore returns an empty store whose offers count as fresh for
// freshFor after the last advertisement.
func NewMemoryStore(freshFor time.Duration) *MemoryStore {
	return &MemoryStore{
		offers:   map[string]Offer{},
		freshFor: freshFor,
		now:      time.Now,
	}
}

var _ Store = (*MemoryStore)(nil)

// UpsertOffer records the offer and stamps LastSeen.
func (m *MemoryStore) UpsertOffer(_ context.Context, o Offer) (Offer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored := cloneOffer(o)
	SortResources(stored.Resources)
	stored.LastSeen = m.now()
	stored.Fresh = false // never stored; computed on read

	// The whole point of this line: the advertisement owns everything about
	// the offer except how much of it is already taken. Carrying the
	// previous value over, instead of writing whatever the caller sent,
	// is what stops a provider's own heartbeat from freeing chunks that
	// are still held.
	if prev, ok := m.offers[o.ClusterID]; ok {
		stored.ReservedChunks = prev.ReservedChunks
	} else {
		stored.ReservedChunks = 0
	}

	m.offers[stored.ClusterID] = stored
	return m.view(stored), nil
}

// GetOffer returns one offer, fresh or not.
func (m *MemoryStore) GetOffer(_ context.Context, clusterID string) (Offer, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	o, ok := m.offers[clusterID]
	if !ok {
		return Offer{}, ErrNotFound
	}
	return m.view(o), nil
}

// ListOffers returns the offers matching f, in no particular order.
func (m *MemoryStore) ListOffers(_ context.Context, f OfferFilter) ([]Offer, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Offer, 0, len(m.offers))
	for _, o := range m.offers {
		v := m.view(o)
		if f.OnlyFresh && !v.Fresh {
			continue
		}
		if f.ClusterType != "" && v.ClusterType != f.ClusterType {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// TryConsumeChunks takes count chunks if that many are available.
//
// Deciding and recording happen under the same lock, which is what makes the
// decision trustworthy: no second caller can slip between the check and the
// increment and be told the same chunks are free.
func (m *MemoryStore) TryConsumeChunks(_ context.Context, clusterID string, count int32) (bool, int32, error) {
	if count <= 0 {
		return false, 0, fmt.Errorf("%w: got %d", ErrInvalidCount, count)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	o, ok := m.offers[clusterID]
	if !ok {
		return false, 0, ErrNotFound
	}

	if o.AvailableChunks() < count {
		// Nothing is written: a refused reservation must leave the
		// provider exactly as it was.
		return false, o.AvailableChunks(), nil
	}

	o.ReservedChunks += count
	m.offers[clusterID] = o
	return true, o.AvailableChunks(), nil
}

// ReleaseChunks gives count chunks back, never going below zero.
func (m *MemoryStore) ReleaseChunks(_ context.Context, clusterID string, count int32) error {
	if count <= 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidCount, count)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	o, ok := m.offers[clusterID]
	if !ok {
		return ErrNotFound
	}

	o.ReservedChunks -= count
	if o.ReservedChunks < 0 {
		o.ReservedChunks = 0
	}
	m.offers[clusterID] = o
	return nil
}

// view returns the caller's copy of a stored offer, with Fresh computed.
// Callers get a copy so they cannot reach back into the store's state by
// holding on to a slice or a pointer. A real backend serialises, which has
// the same effect; doing it here keeps this store honest.
func (m *MemoryStore) view(o Offer) Offer {
	v := cloneOffer(o)
	v.LastSeen = o.LastSeen
	v.Fresh = m.now().Sub(o.LastSeen) <= m.freshFor
	return v
}

// cloneOffer deep-copies the pointers and slices in an Offer.
func cloneOffer(o Offer) Offer {
	c := o

	if o.CarbonIntensity != nil {
		v := *o.CarbonIntensity
		c.CarbonIntensity = &v
	}
	if o.CarbonForecast != nil {
		c.CarbonForecast = append([]float64(nil), o.CarbonForecast...)
	}
	if o.Resources != nil {
		c.Resources = make([]Resource, len(o.Resources))
		for i, r := range o.Resources {
			c.Resources[i] = cloneResource(r)
		}
	}
	if o.Extra != nil {
		c.Extra = make(map[string]string, len(o.Extra))
		for k, v := range o.Extra {
			c.Extra[k] = v
		}
	}
	return c
}

func cloneResource(r Resource) Resource {
	c := r
	if r.UnitPrice != nil {
		v := *r.UnitPrice
		c.UnitPrice = &v
	}
	if r.ScalePercent != nil {
		v := *r.ScalePercent
		c.ScalePercent = &v
	}
	if r.FixedCap != nil {
		v := *r.FixedCap
		c.FixedCap = &v
	}
	return c
}
