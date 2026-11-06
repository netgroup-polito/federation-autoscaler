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
	"errors"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

const (
	// redisKeyPrefix starts every catalog key: catalog:offer:<providerId>:<siteId>.
	redisKeyPrefix = "catalog:offer:"
	// redisScanPattern matches every offer key.
	redisScanPattern = redisKeyPrefix + "*"
	// redisScanCount is a hint to SCAN about how many keys to look at per call.
	redisScanCount = 100
)

// Compile-time checks: RedisCatalog must satisfy both interfaces.
var (
	_ Writer = (*RedisCatalog)(nil)
	_ Reader = (*RedisCatalog)(nil)
)

// RedisCatalog is the catalog stored in Redis (catalog contract v1, section 4).
//
//	key    catalog:offer:<providerId>:<siteId>
//	value  the offer as one JSON string
//	expiry OfferTTL, set by the same command that writes the offer
//
// Freshness is Redis expiry: an offer that its provider stops refreshing
// disappears on its own, so readers need no extra logic. Nothing runs next to
// the catalog: no controller and no polling loop.
//
// The client decides who is connected. A provider builds its client with the
// provider-<id> account, a consumer with a consumer-<id> account, and Redis
// itself refuses what that account may not do.
type RedisCatalog struct {
	client redis.Cmdable
}

// NewRedisCatalog returns a catalog that talks to Redis through client.
// The caller owns the client and closes it.
func NewRedisCatalog(client redis.Cmdable) *RedisCatalog {
	return &RedisCatalog{client: client}
}

// redisKey builds the Redis key of one (provider, site) pair.
func redisKey(providerID, siteID string) string {
	return redisKeyPrefix + providerID + ":" + siteID
}

// parseRedisKey splits a catalog key into its two IDs. ok is false when the
// key does not have the catalog format or its IDs fail validateKey.
func parseRedisKey(key string) (providerID, siteID string, ok bool) {
	rest, found := strings.CutPrefix(key, redisKeyPrefix)
	if !found {
		return "", "", false
	}
	providerID, siteID, found = strings.Cut(rest, ":")
	if !found || validateKey(providerID, siteID) != nil {
		return "", "", false
	}
	return providerID, siteID, true
}

// UpsertOffer stores the offer and its expiry in a single SET ... EX command,
// so there is no moment where the offer exists without an expiry.
func (c *RedisCatalog) UpsertOffer(ctx context.Context, o Offer) error {
	if err := validateKey(o.ProviderID, o.SiteID); err != nil {
		return err
	}
	data, err := json.Marshal(o)
	if err != nil {
		return fmt.Errorf("catalog: encode offer: %w", err)
	}
	if err := c.client.Set(ctx, redisKey(o.ProviderID, o.SiteID), data, OfferTTL).Err(); err != nil {
		return fmt.Errorf("catalog: store offer: %w", err)
	}
	return nil
}

// DeleteOffer removes one offer. Deleting an offer that does not exist is not
// an error (DEL simply returns 0).
func (c *RedisCatalog) DeleteOffer(ctx context.Context, providerID, siteID string) error {
	if err := validateKey(providerID, siteID); err != nil {
		return err
	}
	if err := c.client.Del(ctx, redisKey(providerID, siteID)).Err(); err != nil {
		return fmt.Errorf("catalog: delete offer: %w", err)
	}
	return nil
}

// GetOffer returns ErrNotFound if the offer is missing or has expired.
func (c *RedisCatalog) GetOffer(ctx context.Context, providerID, siteID string) (Offer, error) {
	data, err := c.client.Get(ctx, redisKey(providerID, siteID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Offer{}, ErrNotFound
	}
	if err != nil {
		return Offer{}, fmt.Errorf("catalog: read offer: %w", err)
	}
	o, err := decodeOffer(data)
	if err != nil {
		return Offer{}, err
	}
	if o.ProviderID != providerID || o.SiteID != siteID {
		return Offer{}, ErrNotFound
	}
	return o, nil
}

// ListOffers returns all live offers, in no particular order.
//
// It walks the keyspace with SCAN (never KEYS, which blocks the server), then
// reads the values in one MGET.
func (c *RedisCatalog) ListOffers(ctx context.Context) ([]Offer, error) {
	// SCAN may return the same key more than once, so collect into a set.
	seen := make(map[string]struct{})
	iter := c.client.Scan(ctx, 0, redisScanPattern, redisScanCount).Iterator()
	for iter.Next(ctx) {
		seen[iter.Val()] = struct{}{}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("catalog: scan offers: %w", err)
	}

	offers := []Offer{}
	if len(seen) == 0 {
		return offers, nil // MGET with no keys is an error in Redis
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}

	values, err := c.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("catalog: read offers: %w", err)
	}
	for i, v := range values {
		s, ok := v.(string)
		if !ok {
			continue // nil: the offer expired between SCAN and MGET
		}
		providerID, siteID, ok := parseRedisKey(keys[i])
		if !ok {
			continue
		}
		o, err := decodeOffer([]byte(s))
		if err != nil {
			return nil, err
		}
		if o.ProviderID != providerID || o.SiteID != siteID {
			continue
		}
		offers = append(offers, o)
	}
	return offers, nil
}
