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

package latency

import (
	"context"
	"math"
	"net"
	"testing"
	"time"
)

// startEcho starts a loopback UDP echo (like ghcr.io/liqotech/udpecho) and returns
// its address. It stops when the test ends or stop() is called.
func startEcho(t *testing.T) (addr string, stop func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()
	stop = func() {
		select {
		case <-done:
		default:
			close(done)
			_ = pc.Close()
		}
	}
	t.Cleanup(stop)
	return pc.LocalAddr().String(), stop
}

// closedAddr returns a loopback UDP address with nothing listening.
func closedAddr(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()
	return addr
}

func testProber() *Prober {
	return New(Options{ProbeCount: 2, ProbeTimeout: 100 * time.Millisecond, CacheTTL: time.Hour})
}

func TestReachableBeatsUnreachable(t *testing.T) {
	echo, _ := startEcho(t)
	p := testProber()

	res := p.MeasureAndPick(context.Background(), []Candidate{
		{ProviderClusterID: "p-up", Endpoint: echo},
		{ProviderClusterID: "p-down", Endpoint: closedAddr(t)},
	})
	if res.Chosen != "p-up" {
		t.Fatalf("chosen = %q, want p-up", res.Chosen)
	}
	if math.IsInf(res.RTTs["p-up"], 1) {
		t.Errorf("p-up RTT should be finite, got +Inf")
	}
	if !math.IsInf(res.RTTs["p-down"], 1) {
		t.Errorf("p-down RTT should be +Inf, got %v", res.RTTs["p-down"])
	}
}

func TestAllUnreachable(t *testing.T) {
	p := testProber()
	res := p.MeasureAndPick(context.Background(), []Candidate{
		{ProviderClusterID: "a", Endpoint: closedAddr(t)},
		{ProviderClusterID: "b", Endpoint: ""}, // no endpoint advertised
	})
	if res.Chosen != "" {
		t.Errorf("chosen = %q, want empty (nobody answered)", res.Chosen)
	}
	if !math.IsInf(res.RTTs["a"], 1) || !math.IsInf(res.RTTs["b"], 1) {
		t.Errorf("both should be +Inf: %+v", res.RTTs)
	}
}

func TestCacheReuse(t *testing.T) {
	echo, stop := startEcho(t)
	p := testProber() // TTL 1h

	first := p.MeasureAndPick(context.Background(), []Candidate{{ProviderClusterID: "p", Endpoint: echo}})
	if math.IsInf(first.RTTs["p"], 1) {
		t.Fatalf("first probe should be finite")
	}
	// Kill the echo; a fresh probe would now be +Inf, but the cached sample stands.
	stop()
	second := p.MeasureAndPick(context.Background(), []Candidate{{ProviderClusterID: "p", Endpoint: echo}})
	if second.RTTs["p"] != first.RTTs["p"] {
		t.Errorf("expected cached RTT %v, got %v", first.RTTs["p"], second.RTTs["p"])
	}
}

func TestLastMeasurements(t *testing.T) {
	echo, _ := startEcho(t)
	p := testProber()
	res := p.MeasureAndPick(context.Background(), []Candidate{{ProviderClusterID: "p", Endpoint: echo}})

	last := p.LastMeasurements()
	if last.Chosen != res.Chosen || last.RTTs["p"] != res.RTTs["p"] {
		t.Errorf("LastMeasurements = %+v, want %+v", last, res)
	}
	// Mutating the returned map must not corrupt the prober's copy.
	last.RTTs["p"] = -1
	if p.LastMeasurements().RTTs["p"] == -1 {
		t.Error("LastMeasurements returned a shared map")
	}
}

func TestNoCandidates(t *testing.T) {
	p := testProber()
	res := p.MeasureAndPick(context.Background(), nil)
	if res.Chosen != "" || len(res.RTTs) != 0 {
		t.Errorf("empty input should yield empty result, got %+v", res)
	}
}
