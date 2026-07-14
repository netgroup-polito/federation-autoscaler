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

// Package latency implements the consumer side of the measured-latency placement
// strategy (docs/design.md §4.7 / feature 6). The Broker exposes a shortlist of
// the nearest providers-with-capacity (by great-circle distance); this package
// UDP-probes each candidate's always-on echo endpoint, measures real round-trip
// time, and picks the lowest. The consumer's loopback API then re-masks the
// node-group list to that winner so the Cluster Autoscaler grows exactly it — the
// Broker never probes and stays dial-out-free.
package latency

import (
	"context"
	"maps"
	"math"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Defaults for the probe cadence. N datagrams per candidate (median smooths a
// single unlucky packet); a short per-probe deadline so an unreachable provider
// doesn't stall the Cluster Autoscaler's node-group read; a cache TTL so CA's
// frequent reads reuse a recent measurement instead of re-probing every time.
const (
	DefaultProbeCount   = 5
	DefaultProbeTimeout = 300 * time.Millisecond
	DefaultCacheTTL     = 15 * time.Second
)

// Candidate is one provider to probe: its cluster ID and its UDP echo endpoint
// (<nodeIP>:port, from the advertisement's ProbeEndpoint).
type Candidate struct {
	ProviderClusterID string
	Endpoint          string
}

// Result is a measurement round: the chosen (lowest-RTT) provider and the median
// RTT in milliseconds per provider (+Inf when unreachable). Chosen is "" when no
// candidate answered.
type Result struct {
	Chosen string
	RTTs   map[string]float64
}

type sample struct {
	rttMs float64
	at    time.Time
}

// Prober measures RTT to provider echo endpoints and caches recent results.
// Safe for concurrent use.
type Prober struct {
	mu    sync.Mutex
	cache map[string]sample // keyed by endpoint
	last  Result            // last MeasureAndPick result, for the heartbeat report

	nProbes int
	timeout time.Duration
	ttl     time.Duration
	log     logr.Logger
}

// Options configures a Prober; zero values fall back to the package defaults.
type Options struct {
	ProbeCount   int
	ProbeTimeout time.Duration
	CacheTTL     time.Duration
	Logger       logr.Logger
}

// New returns a Prober ready to use.
func New(opts Options) *Prober {
	logger := opts.Logger
	if logger.GetSink() == nil {
		logger = log.Log.WithName("latency-prober")
	}
	n := opts.ProbeCount
	if n <= 0 {
		n = DefaultProbeCount
	}
	timeout := opts.ProbeTimeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	ttl := opts.CacheTTL
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &Prober{
		cache:   make(map[string]sample),
		nProbes: n,
		timeout: timeout,
		ttl:     ttl,
		log:     logger,
	}
}

// MeasureAndPick returns the lowest-RTT candidate, probing any whose cached
// sample is missing or stale. Candidates with no endpoint (or that never answer)
// score +Inf and lose. The result is stashed for LastMeasurements. On ties the
// earlier candidate wins (callers pass them in the Broker's distance order).
func (p *Prober) MeasureAndPick(ctx context.Context, cands []Candidate) Result {
	rtts := make(map[string]float64, len(cands))

	// Under the lock: serve fresh cache hits, collect the rest to probe.
	type job struct{ providerID, endpoint string }
	toProbe := make([]job, 0, len(cands))
	p.mu.Lock()
	for _, c := range cands {
		if c.Endpoint == "" {
			rtts[c.ProviderClusterID] = math.Inf(1)
			continue
		}
		if s, ok := p.cache[c.Endpoint]; ok && time.Since(s.at) < p.ttl {
			rtts[c.ProviderClusterID] = s.rttMs
			continue
		}
		toProbe = append(toProbe, job{c.ProviderClusterID, c.Endpoint})
	}
	p.mu.Unlock()

	// Probe the stale/missing endpoints concurrently (a shortlist is tiny).
	if len(toProbe) > 0 {
		var wg sync.WaitGroup
		var mu sync.Mutex
		measured := make(map[string]float64, len(toProbe))
		for _, j := range toProbe {
			wg.Add(1)
			go func(endpoint string) {
				defer wg.Done()
				rtt := p.probeMedian(ctx, endpoint)
				mu.Lock()
				measured[endpoint] = rtt
				mu.Unlock()
			}(j.endpoint)
		}
		wg.Wait()

		now := time.Now()
		p.mu.Lock()
		for _, j := range toProbe {
			p.cache[j.endpoint] = sample{rttMs: measured[j.endpoint], at: now}
			rtts[j.providerID] = measured[j.endpoint]
		}
		p.mu.Unlock()
	}

	// Pick the lowest finite RTT (stable on ties: first candidate wins).
	chosen := ""
	best := math.Inf(1)
	for _, c := range cands {
		if r := rtts[c.ProviderClusterID]; r < best {
			best = r
			chosen = c.ProviderClusterID
		}
	}
	if math.IsInf(best, 1) {
		chosen = "" // nobody answered
	}

	res := Result{Chosen: chosen, RTTs: rtts}
	p.mu.Lock()
	p.last = res
	p.mu.Unlock()
	return res
}

// LastMeasurements returns a copy of the most recent MeasureAndPick result (for
// the heartbeat's dashboard report). Zero-value Result before the first probe.
func (p *Prober) LastMeasurements() Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := Result{Chosen: p.last.Chosen}
	if len(p.last.RTTs) > 0 {
		out.RTTs = make(map[string]float64, len(p.last.RTTs))
		maps.Copy(out.RTTs, p.last.RTTs)
	}
	return out
}

// probeMedian sends up to nProbes UDP datagrams to endpoint and returns the median
// successful RTT in milliseconds, or +Inf when none answer.
func (p *Prober) probeMedian(ctx context.Context, endpoint string) float64 {
	samples := make([]float64, 0, p.nProbes)
	for range p.nProbes {
		if ctx.Err() != nil {
			break
		}
		if d, err := probeOnce(endpoint, p.timeout); err == nil {
			samples = append(samples, float64(d.Microseconds())/1000.0)
		}
	}
	if len(samples) == 0 {
		p.log.V(1).Info("probe endpoint unreachable", "endpoint", endpoint)
		return math.Inf(1)
	}
	sort.Float64s(samples)
	return samples[len(samples)/2]
}

// probeOnce sends one datagram to the udpecho endpoint and measures the round trip
// until the echo returns. A UDP "dial" does no handshake, so a lost datagram or a
// dead responder surfaces as a read deadline, not a dial error.
func probeOnce(endpoint string, timeout time.Duration) (time.Duration, error) {
	conn, err := net.DialTimeout("udp", endpoint, timeout)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}
	start := time.Now()
	if _, err := conn.Write([]byte("fa-probe")); err != nil {
		return 0, err
	}
	buf := make([]byte, 64)
	if _, err := conn.Read(buf); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}
