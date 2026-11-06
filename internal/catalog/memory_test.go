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
	"time"

	. "github.com/onsi/ginkgo/v2"
)

// fakeClock is a clock that only moves when a spec says so.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

var _ = Describe("MemoryCatalog", func() {
	RunCatalogConformance(func() conformanceBackend {
		clock := &fakeClock{now: time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)}
		m := NewMemoryCatalogWithClock(clock.Now)

		return conformanceBackend{
			w:       m,
			r:       m,
			advance: clock.Advance,
			putRaw: func(providerID, siteID string, raw []byte) {
				m.mu.Lock()
				defer m.mu.Unlock()
				m.entries[memoryKey{providerID, siteID}] = memoryEntry{
					data:      raw,
					expiresAt: clock.Now().Add(OfferTTL),
				}
			},
			// asProvider stays nil: MemoryCatalog has no access control.
		}
	})
})
