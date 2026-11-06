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

// Package catalog is the shared offer catalog used by the provider agent,
// the consumer side and the broker.
//
// The catalog is deliberately "as stupid as possible": it stores offers and
// returns them. It does not rank, choose, count reservations or compute
// anything. There is no controller and no polling loop next to it.
package catalog

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when an offer is missing or has expired.
var ErrNotFound = errors.New("catalog: offer not found")

const (
	// OfferTTL is how long an offer lives without being refreshed.
	OfferTTL = 90 * time.Second
	// RefreshEvery is how often a writer must refresh its offers.
	RefreshEvery = 30 * time.Second
)

// Writer is used by providers (and, at first, by the broker).
type Writer interface {
	// UpsertOffer stores the offer with OfferTTL.
	UpsertOffer(ctx context.Context, o Offer) error
	// DeleteOffer removes the offer of one (provider, site).
	DeleteOffer(ctx context.Context, providerID, siteID string) error
}

// Reader is used by the consumer side and the broker.
type Reader interface {
	// GetOffer returns ErrNotFound if the offer is missing or expired.
	GetOffer(ctx context.Context, providerID, siteID string) (Offer, error)
	// ListOffers returns all live offers, in no particular order.
	ListOffers(ctx context.Context) ([]Offer, error)
}
