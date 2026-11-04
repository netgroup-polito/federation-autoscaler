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

package brain

// TEMPORARY. This file mirrors the catalog contract v1 (types, interfaces,
// constants and an in-memory catalog) so the brain can be built and tested
// before the shared internal/catalog package exists. When internal/catalog
// lands, delete this file and switch the imports; the names and JSON tags
// below are identical to the contract, so the swap is mechanical.

import (
	"context"
	"errors"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// ErrNotFound is returned by GetOffer when the offer is missing or expired.
var ErrNotFound = errors.New("catalog: offer not found")

const (
	// OfferTTL is how long an offer stays in the catalog without a refresh.
	OfferTTL = 90 * time.Second
	// RefreshEvery is how often a writer re-publishes its offers.
	RefreshEvery = 30 * time.Second
)

// Offer is what one provider site publishes in the catalog. The catalog
// stores it as written and computes nothing: no chunks, no totals, no ranking.
type Offer struct {
	SchemaVersion int    `json:"schemaVersion"` // always 1 for now
	ProviderID    string `json:"providerId"`    // = cluster ID (certificate CN)
	LiqoClusterID string `json:"liqoClusterId"`
	SiteID        string `json:"siteId"` // "asti", or "default"

	Location Location `json:"location"`

	// Available is the free capacity right now: cpu, memory, nvidia.com/gpu.
	Available corev1.ResourceList `json:"available"`

	UnitPrices        corev1.ResourceList `json:"unitPrices,omitempty"`        // EUR per unit per hour
	SLA               string              `json:"sla,omitempty"`               // free label, e.g. "gold"
	ExpectedLatencyMs *float64            `json:"expectedLatencyMs,omitempty"` // declared by the provider
	CarbonIntensity   *float64            `json:"carbonIntensity,omitempty"`   // gCO2/kWh
	CarbonForecast    []float64           `json:"carbonForecast,omitempty"`
	Renewable         bool                `json:"renewable,omitempty"`
	ProbeEndpoint     string              `json:"probeEndpoint,omitempty"` // host:port for latency probing

	NodeLabels map[string]string `json:"nodeLabels,omitempty"`
	NodeTaints []corev1.Taint    `json:"nodeTaints,omitempty"`

	Extra map[string]any `json:"extra,omitempty"` // anything else, unstructured

	UpdatedAt time.Time `json:"updatedAt"`
}

// Location is where an offer's site is.
type Location struct {
	Region    string   `json:"region,omitempty"`
	City      string   `json:"city,omitempty"`
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
}

// Writer is used by providers (and, at first, by the broker).
type Writer interface {
	UpsertOffer(ctx context.Context, o Offer) error // stores it with OfferTTL
	DeleteOffer(ctx context.Context, providerID, siteID string) error
}

// Reader is used by the consumer brain and the broker.
type Reader interface {
	GetOffer(ctx context.Context, providerID, siteID string) (Offer, error) // ErrNotFound if missing or expired
	ListOffers(ctx context.Context) ([]Offer, error)                        // all live offers, in no particular order
}

// MemoryCatalog is an in-memory Reader and Writer that honours OfferTTL, for
// tests and for running the brain without Redis.
type MemoryCatalog struct {
	// Now returns the current time; nil means time.Now. Tests set it to move
	// the clock and exercise expiry.
	Now func() time.Time

	mu     sync.RWMutex
	offers map[offerKey]memEntry
}

type offerKey struct{ provider, site string }

type memEntry struct {
	offer     Offer
	expiresAt time.Time
}

var (
	_ Reader = (*MemoryCatalog)(nil)
	_ Writer = (*MemoryCatalog)(nil)
)

// NewMemoryCatalog returns an empty MemoryCatalog using the wall clock.
func NewMemoryCatalog() *MemoryCatalog {
	return &MemoryCatalog{offers: map[offerKey]memEntry{}}
}

func (m *MemoryCatalog) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// UpsertOffer stores o, replacing any offer for the same (provider, site), and
// (re)starts its OfferTTL.
func (m *MemoryCatalog) UpsertOffer(_ context.Context, o Offer) error {
	if o.ProviderID == "" || o.SiteID == "" {
		return errors.New("catalog: providerId and siteId are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.offers == nil {
		m.offers = map[offerKey]memEntry{}
	}
	m.offers[offerKey{o.ProviderID, o.SiteID}] = memEntry{offer: o, expiresAt: m.now().Add(OfferTTL)}
	return nil
}

// DeleteOffer removes the offer for (providerID, siteID). Deleting a missing
// offer is not an error.
func (m *MemoryCatalog) DeleteOffer(_ context.Context, providerID, siteID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.offers, offerKey{providerID, siteID})
	return nil
}

// GetOffer returns the live offer for (providerID, siteID), or ErrNotFound.
func (m *MemoryCatalog) GetOffer(_ context.Context, providerID, siteID string) (Offer, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.offers[offerKey{providerID, siteID}]
	if !ok || !m.now().Before(e.expiresAt) {
		return Offer{}, ErrNotFound
	}
	return e.offer, nil
}

// ListOffers returns every live offer, in no particular order. The catalog
// never filters or ranks.
func (m *MemoryCatalog) ListOffers(_ context.Context) ([]Offer, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := m.now()
	out := make([]Offer, 0, len(m.offers))
	for _, e := range m.offers {
		if now.Before(e.expiresAt) {
			out = append(out, e.offer)
		}
	}
	return out, nil
}
