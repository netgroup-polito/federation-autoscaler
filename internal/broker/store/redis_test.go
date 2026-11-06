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
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("RedisStore", func() {
	// The same contract the other two implementations satisfy, with nothing
	// written for this backend in particular.
	//
	// That is the whole point: the catalog can move from Kubernetes to Redis
	// — two systems with nothing in common — and the rules that say what the
	// Broker may rely on do not change by a single line.
	//
	// miniredis is a Redis server that runs inside the test process, so
	// these specs need no installed Redis and no container. It speaks the
	// real protocol and runs real Lua, which is what matters here: the
	// capacity check is a script, and a fake that only pretended to run it
	// would be testing nothing.
	RunOfferStoreConformance(func(freshFor time.Duration) OfferStore {
		mr, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(mr.Close)

		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		DeferCleanup(func() { _ = client.Close() })

		return NewRedisStore(client, "", freshFor)
	})
})
