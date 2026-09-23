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
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pullServer is an Ollama /api/pull endpoint that streams the given lines.
func pullServer(t *testing.T, lines ...string) *ollamaRuntime {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" {
			http.NotFound(w, r)
			return
		}
		for _, l := range lines {
			_, _ = w.Write([]byte(l + "\n"))
		}
	}))
	t.Cleanup(srv.Close)
	return &ollamaRuntime{cfg: OllamaConfig{Model: "llama3.2"}, baseURL: srv.URL, http: srv.Client()}
}

// Only a registry that cannot be reached at all is worth retrying through the
// host network; a registry that answered (for example: no such model) would
// answer the same way from there.
func TestPull_NetworkFailureIsRecognised(t *testing.T) {
	for name, tc := range map[string]struct {
		line    string
		network bool
	}{
		"DNS timeout inside the container": {
			`{"error":"pull model manifest: Get \"https://registry.ollama.ai/v2/library/llama3.2/manifests/latest\": ` +
				`dial tcp: lookup registry.ollama.ai on 1.1.1.1:53: read udp 172.17.0.2:40000->1.1.1.1:53: i/o timeout"}`,
			true,
		},
		"no route": {
			`{"error":"pull model manifest: Get \"https://registry.ollama.ai/v2/\": dial tcp 104.21.75.227:443: ` +
				`connect: network is unreachable"}`,
			true,
		},
		"unknown model": {`{"error":"pull model manifest: file does not exist"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			rt := pullServer(t, `{"status":"pulling manifest"}`, tc.line)
			err := rt.pull(context.Background())
			if err == nil {
				t.Fatal("the pull must fail")
			}
			if got := isNetworkError(err); got != tc.network {
				t.Errorf("isNetworkError(%q) = %v, want %v", err, got, tc.network)
			}
		})
	}
}

func TestPull_Success(t *testing.T) {
	rt := pullServer(t, `{"status":"pulling manifest"}`,
		`{"status":"downloading","total":2000,"completed":1000}`, `{"status":"success"}`)
	if err := rt.pull(context.Background()); err != nil {
		t.Errorf("a stream ending in success must pass, got %v", err)
	}
}

// The host-network pull container must resolve names as the host does: the
// daemon's own DNS setting is what broke the first attempt, and host
// networking alone does not replace it.
func TestHostPullArgs(t *testing.T) {
	args := strings.Join(hostPullArgs("run-ollama-pull", "127.0.0.1:40000", "cache", "ollama/ollama:0.34.0", true), " ")
	for _, want := range []string{
		"--network host",
		"-e OLLAMA_HOST=127.0.0.1:40000",
		"-v cache:/root/.ollama",
		"-v /etc/resolv.conf:/etc/resolv.conf:ro",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q must contain %q", args, want)
		}
	}
	if !strings.HasSuffix(args, " ollama/ollama:0.34.0") {
		t.Errorf("the image must come last: %q", args)
	}
	if without := strings.Join(hostPullArgs("n", "a", "v", "img", false), " "); strings.Contains(without, "resolv.conf") {
		t.Errorf("no resolv.conf mount when the host has none: %q", without)
	}
}

func TestFreeLoopbackPort(t *testing.T) {
	port, err := freeLoopbackPort()
	if err != nil || port <= 0 {
		t.Fatalf("port %d, err %v", port, err)
	}
	// The port must really be free: the host-network pull container binds it.
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("port %d is not usable: %v", port, err)
	}
	_ = l.Close()
}

// After joining the Kind network the harness must reach Ollama again: through
// the container's address when the host routes to it, otherwise through the
// published port as Docker reports it now.
func TestFirstAnswering(t *testing.T) {
	answering := func(ok ...string) func(context.Context, string) error {
		return func(_ context.Context, url string) error {
			for _, u := range ok {
				if u == url {
					return nil
				}
			}
			return errors.New("connection refused")
		}
	}
	candidates := []string{"http://172.18.0.23:11434", "http://127.0.0.1:32772"}

	got, err := firstAnswering(context.Background(), candidates, answering(candidates...), time.Second, time.Millisecond)
	if err != nil || got != candidates[0] {
		t.Errorf("both answer: got %q, %v; want the container address", got, err)
	}
	got, err = firstAnswering(context.Background(), candidates, answering(candidates[1]), time.Second, time.Millisecond)
	if err != nil || got != candidates[1] {
		t.Errorf("only the published port answers: got %q, %v", got, err)
	}
	if _, err := firstAnswering(context.Background(), candidates, answering(), 20*time.Millisecond,
		5*time.Millisecond); err == nil {
		t.Error("nothing answers: must fail after the timeout")
	}
}

func TestProbeVersion(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			_, _ = w.Write([]byte(`{"version":"0.34.0"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer up.Close()
	rt := &ollamaRuntime{}
	if err := rt.probeVersion(context.Background(), up.URL); err != nil {
		t.Errorf("a server answering /api/version must pass: %v", err)
	}
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close() // nothing listens any more: like the moved published port
	if err := rt.probeVersion(context.Background(), down.URL); err == nil {
		t.Error("a closed port must fail")
	}
}
