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

package main

import (
	"context"
	"hash/fnv"
	"math/rand"
	"time"
)

// staggeredTicker ticks every interval, the first time after offset, until
// ctx is done. Every agent loop starts at about the same instant; without an
// offset they would all fire together and the Broker would see bursts of N
// simultaneous requests followed by silence, which real, independently
// started agents never produce. Like time.Ticker, a tick the receiver is not
// ready for is dropped rather than queued.
func staggeredTicker(ctx context.Context, interval, offset time.Duration) <-chan time.Time {
	c := make(chan time.Time, 1)
	send := func(t time.Time) {
		select {
		case c <- t:
		default: // the receiver is busy: drop the tick, as time.Ticker does
		}
	}
	go func() {
		first := time.NewTimer(offset)
		defer first.Stop()
		select {
		case <-ctx.Done():
			return
		case t := <-first.C:
			send(t)
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				send(t)
			}
		}
	}()
	return c
}

// startOffset is where, within its first interval, one agent's loop for one
// operation starts. It is random, so agents spread over the interval, and
// derived from the run's seed, the agent and the operation, so the same
// --seed reproduces the same schedule.
func startOffset(seed int64, role string, index int, op Operation, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(role + "/" + string(op)))
	rng := rand.New(rand.NewSource(seed ^ int64(h.Sum64()) ^ int64(index)*7919))
	return time.Duration(rng.Int63n(int64(interval)))
}
