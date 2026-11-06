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

// Package store holds the Broker's storage behind an interface, so the same
// code can run on top of Kubernetes custom resources, a SQL database or a
// key-value store such as Redis.
//
// The interface is written at the level of business operations rather than
// generic read and write. That is what makes several backends possible: a
// generic "read, write, transaction" interface could not be implemented
// honestly by all of them, because Kubernetes has no multi-object
// transactions and Redis has no queries.
package store

import (
	"context"
	"errors"
	"sort"
	"time"
)

// Offer is one entry in the catalog: a provider's standing offer of capacity.
//
// It is a plain Go type on purpose. Nothing here depends on Kubernetes, so
// the callers of this package do not depend on Kubernetes either.
type Offer struct {
	ClusterID     string
	LiqoClusterID string
	ClusterType   string // "standard" or "gpu"

	// Topology carries its own HasLocation flag, so it is a value: an empty
	// Topology means nothing is known about where this provider is.
	Topology Topology

	// Resources is always returned sorted by Name. The order is part of
	// the contract rather than a detail each backend decides, because one
	// of them cannot keep an insertion order at all: the custom resource
	// holds resources in a map, and Go iterates a map in a random order.
	// Left unspecified, the same offer would come back in a different
	// order on different backends, and on the same backend between two
	// reads — which is the worst kind of difference, because code written
	// against one of them passes its tests and fails in production.
	Resources []Resource

	ProbeEndpoint string

	// CarbonIntensity is nil when the provider reported no value.
	CarbonIntensity *float64
	CarbonForecast  []float64
	Renewable       bool

	// TotalChunks is how many chunks this offer is worth, as computed by the
	// chunk sizer before the offer reaches the store.
	TotalChunks int32

	// ReservedChunks is how many of those chunks are currently held by
	// reservations. UpsertOffer never writes it; see the method comment.
	ReservedChunks int32

	// LastSeen is when the provider last advertised. It is an output only:
	// UpsertOffer stamps it itself and ignores whatever the caller passed,
	// so a stale timestamp can never be written in.
	LastSeen time.Time

	// Fresh is set by the store when it returns an Offer: true while the
	// provider is still within the store's freshness window. It is never an
	// input, and it is not stored as a field; each backend decides how to
	// answer it.
	Fresh bool

	// Extra carries whatever a provider sent that the catalog does not
	// define. The store keeps these fields and hands them back unchanged:
	// it never looks inside them, never filters on them, and never fails
	// because of what they contain. Nil and empty mean the same thing.
	//
	// It is a map of strings rather than free-form JSON so that every
	// backend can hold it natively. A provider with genuinely shapeless
	// data can still put it under one key as its own encoded text.
	Extra map[string]string
}

// AvailableChunks is how many chunks can still be reserved. It is computed,
// never stored, and never negative.
func (o Offer) AvailableChunks() int32 {
	if n := o.TotalChunks - o.ReservedChunks; n > 0 {
		return n
	}
	return 0
}

// Topology is where a provider is.
type Topology struct {
	Zone   string
	Region string
	City   string

	Latitude  float64
	Longitude float64

	// HasLocation distinguishes "at 0,0" from "unknown", which the
	// coordinates alone cannot express.
	HasLocation bool
}

// Resource is one resource a provider offers, with the limits and the price
// it set for it.
type Resource struct {
	Name string // "cpu", "memory", "nvidia.com/gpu"

	// Allocatable is the quantity as the provider reported it, for example
	// "8" or "16Gi". It is kept as text so every backend can store it
	// unchanged; parse it in Go where arithmetic is needed.
	Allocatable string

	// UnitPrice is text for the same reason as Allocatable: the advertisement
	// carries it as a Kubernetes quantity, so keeping the text round-trips it
	// exactly instead of rounding through a float.
	UnitPrice    *string
	ScalePercent *int32
	FixedCap     *string
}

// OfferFilter narrows what ListOffers returns.
//
// It is deliberately a small closed set of options and not a query language.
// Every field here has to be answerable by an index that a key-value backend
// can maintain by itself; anything that would need a free-form search does
// not belong in this struct.
type OfferFilter struct {
	// OnlyFresh keeps only providers still within the freshness window.
	//
	// Note it is a flag and not a timestamp. A timestamp ("seen since T")
	// would work in SQL but is impossible in Redis, which cannot search by
	// time at all. So the window itself belongs inside the implementation,
	// chosen when the store is built, and each backend answers the flag its
	// own way.
	//
	// Not, however, by letting a key expire, which is the trick Redis
	// invites. Expiry deletes, and going stale is not being deleted: a
	// quiet provider must still be found by GetOffer and still appear in an
	// unfiltered list. Every backend therefore keeps the entry and computes
	// the answer from LastSeen when it is read.
	OnlyFresh bool

	// ClusterType keeps only offers of this type. Empty means any.
	ClusterType string
}

// OfferStore is the catalog side of the store: the list of registered offers
// that consumers, and later FiberCop, read.
type OfferStore interface {
	// UpsertOffer records a provider's current offer and renews its
	// freshness. It is called on every advertisement, so roughly every
	// thirty seconds per provider.
	//
	// It must never write ReservedChunks. That counter belongs to the
	// reservation path, and overwriting it on every advertisement would
	// reset it to zero and let the same chunks be sold twice.
	//
	// It returns the stored offer, with Fresh and ReservedChunks filled in.
	UpsertOffer(ctx context.Context, o Offer) (Offer, error)

	// GetOffer returns one offer, fresh or not. It returns ErrNotFound when
	// no provider with that cluster ID has ever advertised.
	GetOffer(ctx context.Context, clusterID string) (Offer, error)

	// ListOffers returns the offers matching f, in no particular order.
	ListOffers(ctx context.Context, f OfferFilter) ([]Offer, error)
}

// ChunkStore is the reservation side of the store: how many of a provider's
// chunks are already taken.
//
// It is a second interface rather than more methods on OfferStore because the
// two are read by different code. Everything that only answers "what is in
// the catalog" needs OfferStore alone, and cannot accidentally change a
// counter it has no business touching.
//
// Both methods are business operations, not generic reads and writes. That is
// deliberate: the thing that must not break is that two consumers are never
// sold the same chunk, and that is only guaranteed if deciding and recording
// happen as one indivisible step inside the implementation. A generic
// interface would have to expose a transaction, which Kubernetes cannot give
// across objects and Redis cannot give as a query — so each backend keeps its
// own way of being atomic:
//
//	SQL    UPDATE ... WHERE total - reserved >= n, one statement
//	Redis  one Lua script, run atomically
//	CR     read, check, write, retried when the object changed underneath
type ChunkStore interface {
	// TryConsumeChunks takes count chunks from a provider if that many are
	// still available.
	//
	// Not enough capacity is an answer, not a failure: it returns ok false
	// with the number actually available and no error, and writes nothing.
	// A caller turns that into a refused reservation. err is for a store
	// that could not be reached at all.
	//
	// On success, available is what is left after this call.
	TryConsumeChunks(ctx context.Context, clusterID string, count int32) (ok bool, available int32, err error)

	// ReleaseChunks gives count chunks back when a reservation ends.
	//
	// It never lets the counter go below zero, and releasing more than is
	// held is not an error. A release that is retried after a timeout, or
	// runs twice because two paths both think they own the cleanup, must
	// not be able to wedge a provider — and a counter that went negative
	// would make the provider look like it had more capacity than it owns,
	// which is the one failure this whole interface exists to prevent.
	ReleaseChunks(ctx context.Context, clusterID string, count int32) error
}

// Store is both sides at once. Every implementation provides both; the two
// interfaces exist so callers can ask for only the half they need.
type Store interface {
	OfferStore
	ChunkStore
}

// SortResources puts resources in the order every store returns them:
// by name. Implementations call it before handing an Offer back.
func SortResources(rs []Resource) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
}

// ErrNotFound is returned when the requested entry does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrInvalidCount is returned when a caller asks to consume or release a
// number of chunks that is not positive. It is rejected rather than quietly
// treated as nothing to do, because a zero or negative count reaching this
// layer means the caller computed it wrong, and silently succeeding would
// hide that.
var ErrInvalidCount = errors.New("store: chunk count must be positive")

// ErrPartialWrite means the offer itself was stored, but its freshness stamp
// was not. The offer is in the catalog; only "last seen" is older than it
// should be, and the provider's next advertisement renews it.
//
// It names a state, not a backend: any store that writes the offer and its
// timestamp in two steps can report it, and one that writes both at once
// never will. A caller that treats a missed stamp as harmless can log it and
// carry on; one that does not can fail.
var ErrPartialWrite = errors.New("store: offer stored, freshness not updated")

// DefaultFreshFor is how long an offer counts as fresh when the caller does
// not say otherwise. It mirrors the ninety seconds the advertisement
// controller has always used to decide a provider is gone.
const DefaultFreshFor = 90 * time.Second
