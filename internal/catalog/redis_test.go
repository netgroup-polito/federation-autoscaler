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
	"github.com/alicebob/miniredis/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redis/go-redis/v9"
)

// The shared specs run here against miniredis, a Redis server that lives inside
// the test process: real protocol, real expiry (moved with FastForward), no
// installation. miniredis has no ACL, so the spec "does not let a provider
// write or delete another provider's key" is skipped here (asProvider is nil)
// and has to run against a real Redis.
var _ = Describe("RedisCatalog (miniredis)", func() {
	RunCatalogConformance(func() conformanceBackend {
		mr, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(mr.Close)

		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		DeferCleanup(client.Close)

		c := NewRedisCatalog(client)

		return conformanceBackend{
			w:       c,
			r:       c,
			advance: mr.FastForward,
			putRaw: func(providerID, siteID string, raw []byte) {
				key := redisKey(providerID, siteID)
				Expect(mr.Set(key, string(raw))).To(Succeed())
				mr.SetTTL(key, OfferTTL)
			},
			// asProvider stays nil: miniredis has no ACL.
		}
	})
})
