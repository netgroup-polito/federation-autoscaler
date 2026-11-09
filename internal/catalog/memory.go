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

package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Compile-time checks: MemoryCatalog must satisfy both interfaces.
var (
	_ Writer = (*MemoryCatalog)(nil)
	_ Reader = (*MemoryCatalog)(nil)
)

type memoryKey struct {
	providerID string
	siteID     string
}

type memoryEntry struct {
	data      []byte    // the offer as JSON, exactly what Redis would hold
	expiresAt time.Time // after this moment the offer is gone
}

// MemoryCatalog is an in-process catalog, used in tests.
//
// It behaves like the Redis catalog: one offer per (provider, site), a fixed
// OfferTTL, and offers stored as JSON. Storing JSON (instead of the Go struct)
// means a caller can never change a stored offer by editing its own copy, and
// the offer goes through the same encode/decode path as with Redis.
type MemoryCatalog struct {
	mu      sync.RWMutex
	now     func() time.Time
	entries map[memoryKey]memoryEntry
}

// NewMemoryCatalog returns an empty catalog that uses the real clock.
func NewMemoryCatalog() *MemoryCatalog {
	return NewMemoryCatalogWithClock(time.Now)
}

// NewMemoryCatalogWithClock lets a test control time, so expiry can be
// checked without sleeping for 90 seconds.
func NewMemoryCatalogWithClock(now func() time.Time) *MemoryCatalog {
	return &MemoryCatalog{
		now:     now,
		entries: make(map[memoryKey]memoryEntry),
	}
}

// UpsertOffer stores the offer with OfferTTL. A second call for the same
// (provider, site) replaces the offer and restarts the TTL.
func (m *MemoryCatalog) UpsertOffer(ctx context.Context, o Offer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateKey(o.ProviderID, o.SiteID); err != nil {
		return err
	}
	data, err := json.Marshal(o)
	if err != nil {
		return fmt.Errorf("catalog: encode offer: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.purgeExpiredLocked(now)
	m.entries[memoryKey{o.ProviderID, o.SiteID}] = memoryEntry{
		data:      data,
		expiresAt: now.Add(OfferTTL),
	}
	return nil
}

// DeleteOffer removes one offer. Deleting an offer that does not exist is not
// an error (same as DEL in Redis).
func (m *MemoryCatalog) DeleteOffer(ctx context.Context, providerID, siteID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateKey(providerID, siteID); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, memoryKey{providerID, siteID})
	return nil
}

// GetOffer returns ErrNotFound if the offer is missing or expired.
func (m *MemoryCatalog) GetOffer(ctx context.Context, providerID, siteID string) (Offer, error) {
	if err := ctx.Err(); err != nil {
		return Offer{}, err
	}
	m.mu.RLock()
	e, ok := m.entries[memoryKey{providerID, siteID}]
	now := m.now()
	m.mu.RUnlock()

	if !ok || !now.Before(e.expiresAt) {
		return Offer{}, ErrNotFound
	}
	o, err := decodeOffer(e.data)
	if err != nil {
		return Offer{}, err
	}
	if o.ProviderID != providerID || o.SiteID != siteID {
		return Offer{}, ErrNotFound
	}
	return o, nil
}

// ListOffers returns all live offers, in no particular order.
func (m *MemoryCatalog) ListOffers(ctx context.Context) ([]Offer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	now := m.now()
	type stored struct {
		key  memoryKey
		data []byte
	}
	raw := make([]stored, 0, len(m.entries))
	for k, e := range m.entries {
		if now.Before(e.expiresAt) {
			raw = append(raw, stored{k, e.data})
		}
	}
	m.mu.RUnlock()

	offers := make([]Offer, 0, len(raw))
	for _, s := range raw {
		o, err := decodeOffer(s.data)
		if err != nil {
			continue // not valid JSON: skip it, so one bad writer cannot break every reader
		}
		if (memoryKey{o.ProviderID, o.SiteID}) != s.key {
			continue
		}
		offers = append(offers, o)
	}
	return offers, nil
}

// purgeExpiredLocked drops expired entries so the map does not grow forever.
// It runs only inside UpsertOffer: the catalog has no background loop.
// The caller must hold m.mu for writing.
func (m *MemoryCatalog) purgeExpiredLocked(now time.Time) {
	for k, e := range m.entries {
		if !now.Before(e.expiresAt) {
			delete(m.entries, k)
		}
	}
}

// decodeOffer turns stored JSON back into an Offer. Unknown fields are
// ignored on purpose (json.Unmarshal does that by default): a reader must
// not fail because a newer writer added a field.
func decodeOffer(data []byte) (Offer, error) {
	var o Offer
	if err := json.Unmarshal(data, &o); err != nil {
		return Offer{}, fmt.Errorf("catalog: decode offer: %w", err)
	}
	return o, nil
}
