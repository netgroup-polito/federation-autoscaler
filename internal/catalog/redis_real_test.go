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
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redis/go-redis/v9"
)

// These specs run against a real Redis server, because miniredis has no ACL.
// They only exist when CATALOG_TEST_REDIS_ADDR is set (for example
// localhost:6379); without it the normal `go test` does not see them.
//
// WARNING: use a throwaway Redis. Every spec deletes all catalog:offer:* keys
// before and after it runs, and creates and deletes provider-* and consumer-*
// accounts. CATALOG_TEST_REDIS_PASSWORD is the password of the default user,
// if the server has one.
const aclTestPassword = "test-password"

// realRedis holds the connection details and helpers shared by the specs.
type realRedis struct {
	addr     string
	password string

	ctx   context.Context
	admin *redis.Client // the default user: full rights, used to inspect state
}

// newRealRedis returns nil when no real Redis was configured.
func newRealRedis() *realRedis {
	addr := os.Getenv("CATALOG_TEST_REDIS_ADDR")
	if addr == "" {
		return nil
	}
	return &realRedis{addr: addr, password: os.Getenv("CATALOG_TEST_REDIS_PASSWORD")}
}

// setup connects the admin client and leaves the catalog empty. It must be
// called from inside a spec (a BeforeEach or the backend constructor).
func (rr *realRedis) setup() {
	rr.ctx = context.Background()
	rr.admin = redis.NewClient(&redis.Options{Addr: rr.addr, Password: rr.password})
	Expect(rr.admin.Ping(rr.ctx).Err()).To(Succeed(), "cannot reach Redis at "+rr.addr)

	rr.wipe()
	DeferCleanup(func() {
		rr.wipe()
		_ = rr.admin.Close()
	})
}

// wipe deletes every catalog offer. It never uses FLUSHDB, so keys that do not
// belong to the catalog are left alone.
func (rr *realRedis) wipe() {
	iter := rr.admin.Scan(rr.ctx, 0, redisScanPattern, redisScanCount).Iterator()
	for iter.Next(rr.ctx) {
		Expect(rr.admin.Del(rr.ctx, iter.Val()).Err()).To(Succeed())
	}
	Expect(iter.Err()).To(Succeed())
}

// login creates a Redis account with the given ACL rules and returns a client
// connected as that account. The account is deleted when the spec ends.
func (rr *realRedis) login(user string, rules ...string) *redis.Client {
	args := make([]any, 0, 6+len(rules))
	args = append(args, "ACL", "SETUSER", user, "reset", "on", ">"+aclTestPassword)
	for _, r := range rules {
		args = append(args, r)
	}
	Expect(rr.admin.Do(rr.ctx, args...).Err()).To(Succeed())
	DeferCleanup(func() {
		_ = rr.admin.Do(context.Background(), "ACL", "DELUSER", user).Err()
	})

	c := redis.NewClient(&redis.Options{Addr: rr.addr, Username: user, Password: aclTestPassword})
	DeferCleanup(c.Close)
	return c
}

// providerRules are the permissions of a provider account (contract, section 5):
// write and delete only its own keys. +@connection lets the client log in.
func providerRules(providerID string) []string {
	return []string{"~" + redisKeyPrefix + providerID + ":*", "+set", "+del", "+@connection"}
}

// consumerRules are the permissions of a consumer account: read only, any
// offer.
func consumerRules() []string {
	return []string{"~" + redisKeyPrefix + "*", "+get", "+scan", "+mget", "+@connection"}
}

// The whole shared suite, on a real server. The specs that need to move time
// are skipped (a real 90 s TTL cannot be fast-forwarded); everything else runs,
// including the one miniredis has to skip: a provider cannot write another
// provider's key.
var _ = Describe("RedisCatalog (real Redis)", func() {
	rr := newRealRedis()
	if rr == nil {
		return
	}

	RunCatalogConformance(func() conformanceBackend {
		rr.setup()
		c := NewRedisCatalog(rr.admin)

		return conformanceBackend{
			w: c,
			r: c,
			advance: func(time.Duration) {
				Skip("a real Redis cannot be fast-forwarded; expiry runs on miniredis")
			},
			putRaw: func(providerID, siteID string, raw []byte) {
				Expect(rr.admin.Set(rr.ctx, redisKey(providerID, siteID), raw, OfferTTL).Err()).To(Succeed())
			},
			asProvider: func(providerID string) Writer {
				return NewRedisCatalog(rr.login("provider-"+providerID, providerRules(providerID)...))
			},
		}
	})
})

// Things only a real server can show.
var _ = Describe("RedisCatalog (real Redis): server behaviour", func() {
	rr := newRealRedis()
	if rr == nil {
		return
	}

	BeforeEach(rr.setup)

	It("sets a real expiry on every offer", func() {
		c := NewRedisCatalog(rr.admin)
		Expect(c.UpsertOffer(rr.ctx, sampleOffer("fibercop", "asti"))).To(Succeed())

		ttl, err := rr.admin.TTL(rr.ctx, redisKey("fibercop", "asti")).Result()
		Expect(err).NotTo(HaveOccurred())
		Expect(ttl).To(BeNumerically(">", 0), "the offer has no expiry")
		Expect(ttl).To(BeNumerically("<=", OfferTTL))
	})

	It("lets a consumer read offers but not write or delete them", func() {
		Expect(NewRedisCatalog(rr.admin).UpsertOffer(rr.ctx, sampleOffer("fibercop", "asti"))).To(Succeed())

		consumer := NewRedisCatalog(rr.login("consumer-x", consumerRules()...))

		all, err := consumer.ListOffers(rr.ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(1))

		_, err = consumer.GetOffer(rr.ctx, "fibercop", "asti")
		Expect(err).NotTo(HaveOccurred())

		Expect(consumer.UpsertOffer(rr.ctx, sampleOffer("fibercop", "cuneo"))).NotTo(Succeed(),
			"a consumer must not be able to write")
		Expect(consumer.DeleteOffer(rr.ctx, "fibercop", "asti")).NotTo(Succeed(),
			"a consumer must not be able to delete")

		// The forbidden calls changed nothing.
		admin := NewRedisCatalog(rr.admin)
		_, err = admin.GetOffer(rr.ctx, "fibercop", "asti")
		Expect(err).NotTo(HaveOccurred())
		_, err = admin.GetOffer(rr.ctx, "fibercop", "cuneo")
		Expect(err).To(MatchError(ErrNotFound))
	})
})
