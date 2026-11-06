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
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore keeps the catalog in Redis.
//
// Redis has no queries, so anything the interface can be asked must already
// exist as a key or an index. The layout is:
//
//	<prefix>offers              a SET of every cluster ID
//	<prefix>offer:<clusterID>   a HASH with four fields:
//	                              data      the offer's attributes, as JSON
//	                              total     how many chunks the offer is worth
//	                              reserved  how many are currently held
//	                              lastSeen  when the provider last advertised
//
// The counters are their own hash fields rather than numbers inside the JSON.
// That is what lets the capacity check be one small script doing integer
// arithmetic, instead of a script that would have to decode, edit and
// re-encode a document.
type RedisStore struct {
	rdb      redis.Cmdable
	prefix   string
	freshFor time.Duration

	now func() time.Time
}

// DefaultRedisPrefix namespaces this Broker's keys, so several deployments
// can share one Redis without colliding.
const DefaultRedisPrefix = "fa:"

// NewRedisStore returns a store backed by rdb. An empty prefix becomes
// DefaultRedisPrefix.
func NewRedisStore(rdb redis.Cmdable, prefix string, freshFor time.Duration) *RedisStore {
	if prefix == "" {
		prefix = DefaultRedisPrefix
	}
	return &RedisStore{rdb: rdb, prefix: prefix, freshFor: freshFor, now: time.Now}
}

var _ Store = (*RedisStore)(nil)

func (s *RedisStore) indexKey() string          { return s.prefix + "offers" }
func (s *RedisStore) offerKey(id string) string { return s.prefix + "offer:" + id }

// -----------------------------------------------------------------------------
// catalog
// -----------------------------------------------------------------------------

// upsertScript writes the offer and adds it to the index as one step.
//
// The reserved counter is created only when it does not already exist, which
// is this backend's version of the rule every implementation obeys: an
// advertisement may not touch reservation accounting. A provider's heartbeat
// arrives every thirty seconds, and if it reset this field those chunks would
// be sold twice.
var upsertScript = redis.NewScript(`
local key   = KEYS[1]
local index = KEYS[2]

redis.call('HSET', key, 'data', ARGV[1], 'total', ARGV[2], 'lastSeen', ARGV[3])
if redis.call('HEXISTS', key, 'reserved') == 0 then
  redis.call('HSET', key, 'reserved', '0')
end
redis.call('SADD', index, ARGV[4])

return redis.call('HGET', key, 'reserved')
`)

// UpsertOffer records the offer and stamps LastSeen.
func (s *RedisStore) UpsertOffer(ctx context.Context, o Offer) (Offer, error) {
	if o.ClusterID == "" {
		return Offer{}, fmt.Errorf("store: ClusterID is required")
	}

	blob, err := encodeOfferAttributes(o)
	if err != nil {
		return Offer{}, err
	}

	seen := s.now()
	res, err := upsertScript.Run(ctx, s.rdb,
		[]string{s.offerKey(o.ClusterID), s.indexKey()},
		blob, o.TotalChunks, seen.UnixNano(), o.ClusterID,
	).Result()
	if err != nil {
		return Offer{}, fmt.Errorf("store: upsert %s: %w", o.ClusterID, err)
	}

	stored := cloneOffer(o)
	SortResources(stored.Resources)
	stored.LastSeen = seen
	stored.Fresh = true
	stored.ReservedChunks = parseCount(res)
	return stored, nil
}

// GetOffer returns one offer, fresh or not.
func (s *RedisStore) GetOffer(ctx context.Context, clusterID string) (Offer, error) {
	h, err := s.rdb.HGetAll(ctx, s.offerKey(clusterID)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return Offer{}, fmt.Errorf("store: get %s: %w", clusterID, err)
	}
	if len(h) == 0 {
		return Offer{}, ErrNotFound
	}
	return s.offerFromHash(h)
}

// ListOffers returns the offers matching f, in no particular order.
//
// The set is what makes this possible at all: without an index maintained on
// write, listing would mean scanning the whole keyspace.
func (s *RedisStore) ListOffers(ctx context.Context, f OfferFilter) ([]Offer, error) {
	ids, err := s.rdb.SMembers(ctx, s.indexKey()).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("store: list: %w", err)
	}
	if len(ids) == 0 {
		return []Offer{}, nil
	}

	// One round trip for every offer instead of one per offer.
	pipe := s.rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, s.offerKey(id))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("store: list: %w", err)
	}

	out := make([]Offer, 0, len(ids))
	for _, cmd := range cmds {
		h, err := cmd.Result()
		if err != nil || len(h) == 0 {
			// The index named an offer whose hash is gone. Skip it
			// rather than failing the whole listing.
			continue
		}
		o, err := s.offerFromHash(h)
		if err != nil {
			return nil, err
		}
		if f.OnlyFresh && !o.Fresh {
			continue
		}
		if f.ClusterType != "" && o.ClusterType != f.ClusterType {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// reservation counters
// -----------------------------------------------------------------------------

// consumeScript is where this backend keeps its promise that the same chunk
// is never sold twice.
//
// Redis runs a script to completion with nothing else interleaved, so the
// read, the comparison and the write are one indivisible step. No locking,
// no retry loop, no version check: the atomicity is a property of the engine,
// the same way it is a property of a single SQL statement.
//
// Returns {status, available}, where status is -1 when the provider is
// unknown, 0 when there was not enough capacity, and 1 when the chunks were
// taken.
var consumeScript = redis.NewScript(`
local key = KEYS[1]
if redis.call('EXISTS', key) == 0 then
  return {-1, 0}
end

local total    = tonumber(redis.call('HGET', key, 'total')) or 0
local reserved = tonumber(redis.call('HGET', key, 'reserved')) or 0
local want     = tonumber(ARGV[1])

local available = total - reserved
if available < 0 then available = 0 end

if available < want then
  return {0, available}
end

reserved = reserved + want
redis.call('HSET', key, 'reserved', reserved)

available = total - reserved
if available < 0 then available = 0 end
return {1, available}
`)

// releaseScript gives chunks back, clamped at zero.
//
// Returns -1 when the provider is unknown, 1 otherwise.
var releaseScript = redis.NewScript(`
local key = KEYS[1]
if redis.call('EXISTS', key) == 0 then
  return -1
end

local reserved = tonumber(redis.call('HGET', key, 'reserved')) or 0
reserved = reserved - tonumber(ARGV[1])
if reserved < 0 then reserved = 0 end

redis.call('HSET', key, 'reserved', reserved)
return 1
`)

// TryConsumeChunks takes count chunks if that many are available.
func (s *RedisStore) TryConsumeChunks(ctx context.Context, clusterID string, count int32) (bool, int32, error) {
	if count <= 0 {
		return false, 0, fmt.Errorf("%w: got %d", ErrInvalidCount, count)
	}

	raw, err := consumeScript.Run(ctx, s.rdb, []string{s.offerKey(clusterID)}, count).Slice()
	if err != nil {
		return false, 0, fmt.Errorf("store: consume %d chunks from %s: %w", count, clusterID, err)
	}
	if len(raw) != 2 {
		return false, 0, fmt.Errorf("store: consume %d chunks from %s: unexpected reply %v",
			count, clusterID, raw)
	}

	status := parseCount(raw[0])
	available := parseCount(raw[1])

	switch status {
	case -1:
		return false, 0, ErrNotFound
	case 0:
		return false, available, nil
	default:
		return true, available, nil
	}
}

// ReleaseChunks gives count chunks back, never going below zero.
func (s *RedisStore) ReleaseChunks(ctx context.Context, clusterID string, count int32) error {
	if count <= 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidCount, count)
	}

	raw, err := releaseScript.Run(ctx, s.rdb, []string{s.offerKey(clusterID)}, count).Result()
	if err != nil {
		return fmt.Errorf("store: release %d chunks to %s: %w", count, clusterID, err)
	}
	if parseCount(raw) == -1 {
		return ErrNotFound
	}
	return nil
}

// -----------------------------------------------------------------------------
// encoding
// -----------------------------------------------------------------------------

// encodeOfferAttributes serialises everything except the three values the
// hash keeps in fields of their own. They are zeroed rather than omitted so
// a stale copy inside the document can never be read back by mistake.
func encodeOfferAttributes(o Offer) ([]byte, error) {
	o.TotalChunks = 0
	o.ReservedChunks = 0
	o.LastSeen = time.Time{}
	o.Fresh = false

	blob, err := json.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("store: encode offer %s: %w", o.ClusterID, err)
	}
	return blob, nil
}

// offerFromHash rebuilds an Offer, computing Fresh from lastSeen.
//
// Freshness is computed here rather than left to key expiry, even though
// expiry would be the more Redis-like trick. An expired key disappears, and
// a provider going stale must not disappear: it still has to be found by
// GetOffer and still appear in an unfiltered list. Going quiet is not the
// same as being deleted.
func (s *RedisStore) offerFromHash(h map[string]string) (Offer, error) {
	var o Offer
	if err := json.Unmarshal([]byte(h["data"]), &o); err != nil {
		return Offer{}, fmt.Errorf("store: decode offer: %w", err)
	}

	SortResources(o.Resources)
	o.TotalChunks = parseCount(h["total"])
	o.ReservedChunks = parseCount(h["reserved"])

	if ns, err := strconv.ParseInt(h["lastSeen"], 10, 64); err == nil {
		o.LastSeen = time.Unix(0, ns)
		o.Fresh = s.now().Sub(o.LastSeen) <= s.freshFor
	}
	return o, nil
}

// parseCount reads a counter out of whatever Redis handed back. Redis speaks
// in text and in 64-bit integers depending on the command, and a missing
// field reads as zero.
func parseCount(v any) int32 {
	switch n := v.(type) {
	case int64:
		return int32(n)
	case int32:
		return n
	case int:
		return int32(n)
	case string:
		parsed, err := strconv.ParseInt(n, 10, 32)
		if err != nil {
			return 0
		}
		return int32(parsed)
	default:
		return 0
	}
}
