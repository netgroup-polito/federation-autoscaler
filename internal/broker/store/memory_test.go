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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("MemoryStore", func() {
	// The shared contract. The CR-backed store and, later, the Redis store
	// run these same specs.
	RunOfferStoreConformance(func(freshFor time.Duration) OfferStore {
		return NewMemoryStore(freshFor)
	})

	// Specs below are specific to this implementation, so they may use its
	// swappable clock instead of waiting.
	Describe("with a fake clock", func() {
		var (
			ctx context.Context
			m   *MemoryStore
			now time.Time
		)

		BeforeEach(func() {
			ctx = context.Background()
			now = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
			m = NewMemoryStore(90 * time.Second)
			m.now = func() time.Time { return now }
		})

		It("keeps an offer fresh for exactly the window", func() {
			_, err := m.UpsertOffer(ctx, sampleOffer("cluster-a"))
			Expect(err).NotTo(HaveOccurred())

			now = now.Add(89 * time.Second)
			got, err := m.GetOffer(ctx, "cluster-a")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Fresh).To(BeTrue())

			now = now.Add(2 * time.Second)
			got, err = m.GetOffer(ctx, "cluster-a")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Fresh).To(BeFalse())
		})

		It("stamps LastSeen itself and ignores what the caller sent", func() {
			o := sampleOffer("cluster-a")
			o.LastSeen = time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)

			stored, err := m.UpsertOffer(ctx, o)
			Expect(err).NotTo(HaveOccurred())
			Expect(stored.LastSeen).To(Equal(now))
		})
	})
})
