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

package api

import (
	"sync"
	"time"
)

// enforcedForgetAfter bounds how long a just-enforced entry is remembered. The
// informer cache catches up within milliseconds; this only prunes entries for
// instructions that are never listed again (for example, deleted ones).
const enforcedForgetAfter = 2 * time.Minute

// staleEnforced stops the instruction poll from redelivering an instruction
// whose result was just recorded.
//
// The Broker lists instructions from the manager's informer cache, and marks
// them Enforced with a direct write. An agent polls again within about a
// second of posting its result, often before the cache has seen that write,
// so the stale copy (still un-Enforced) was handed out again and the agent ran
// the whole instruction a second time (a second `liqoctl peer`, for example).
//
// After marking an instruction Enforced, the Broker records the
// resourceVersion it replaced. A listed copy with exactly that version is the
// stale one and is skipped. Any other version (the cache caught up, or the
// instruction was re-armed later) clears the entry and is handled as usual.
type staleEnforced struct {
	mu      sync.Mutex
	entries map[string]staleEntry // keyed by kind/namespace/name
}

type staleEntry struct {
	resourceVersion string
	at              time.Time
}

func newStaleEnforced() *staleEnforced {
	return &staleEnforced{entries: map[string]staleEntry{}}
}

// record remembers that the object at key, last seen with resourceVersion rv,
// has just been marked Enforced.
func (s *staleEnforced) record(key, rv string, now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = staleEntry{resourceVersion: rv, at: now}
}

// isStale reports whether a listed copy (key, rv) is the pre-Enforced one. A
// different version clears the entry. Old entries are pruned on the way. A nil
// set (a Server built without NewServer) hides nothing.
func (s *staleEnforced) isStale(key, rv string, now time.Time) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.entries {
		if now.Sub(e.at) > enforcedForgetAfter {
			delete(s.entries, k)
		}
	}
	e, ok := s.entries[key]
	if !ok {
		return false
	}
	if e.resourceVersion == rv {
		return true
	}
	delete(s.entries, key)
	return false
}
