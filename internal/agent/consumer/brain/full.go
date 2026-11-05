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

import (
	"sync"
	"time"

	"github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
)

// FullCooldown is how long a provider stays marked full after the Broker
// refused a reservation on it for lack of capacity. The catalog refreshes
// every RefreshEvery, so after this window a fresh offer tells the truth again.
const FullCooldown = 30 * time.Second

// FullProviders remembers providers the Broker recently refused for lack of
// capacity. The brain decides on catalog offers that can be a few seconds old,
// so two consumers may pick the same last chunk; the Broker accepts one and
// answers the other with 409 InsufficientCapacity. Marking that provider full
// for FullCooldown makes the next decision expose the next-best provider
// instead of retrying the same one. Safe for concurrent use.
type FullProviders struct {
	// Now returns the current time; nil means time.Now. Tests set it to move
	// the clock.
	Now func() time.Time

	mu    sync.Mutex
	until map[string]time.Time
}

// NewFullProviders returns an empty FullProviders using the wall clock.
func NewFullProviders() *FullProviders {
	return &FullProviders{until: map[string]time.Time{}}
}

func (f *FullProviders) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Mark records provider as full for FullCooldown from now.
func (f *FullProviders) Mark(provider string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.until == nil {
		f.until = map[string]time.Time{}
	}
	f.until[provider] = f.now().Add(FullCooldown)
}

// MarkIfNoCapacity marks provider full when err is the Broker's
// InsufficientCapacity refusal, and reports whether it did. Any other error
// (including the other 409, an idempotency conflict) leaves it unmarked.
func (f *FullProviders) MarkIfNoCapacity(provider string, err error) bool {
	if !client.IsInsufficientCapacity(err) {
		return false
	}
	f.Mark(provider)
	return true
}

// Snapshot returns the providers currently marked full, dropping expired marks.
// The result is the Input.Full of the next decision.
func (f *FullProviders) Snapshot() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	out := map[string]bool{}
	for p, until := range f.until {
		if now.Before(until) {
			out[p] = true
		} else {
			delete(f.until, p)
		}
	}
	return out
}
