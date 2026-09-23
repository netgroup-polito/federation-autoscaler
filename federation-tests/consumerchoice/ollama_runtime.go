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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/federation-tests/testlib"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// ollamaRuntime is the model server used by one run: a container this run
// started (managed) or an existing endpoint (unmanaged).
type ollamaRuntime struct {
	cfg       OllamaConfig
	container string // empty when not managed
	baseURL   string
	http      *http.Client

	// done is closed when the background preparation ends; prepErr is its
	// result, written before the close and so safe to read after it.
	done    chan struct{}
	prepErr error

	mu       sync.Mutex // guards the fields below, written by the preparation goroutine
	version  string
	warnings []string
}

// startOllama brings up the runtime. In managed mode it starts the container
// immediately and returns without waiting for the model: preparation runs in
// the background (beginPreparation) so a first-run model download overlaps the
// cluster creation instead of adding to it.
func startOllama(ctx context.Context, cfg OllamaConfig, runID string) (*ollamaRuntime, error) {
	rt := &ollamaRuntime{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}
	if !cfg.IsManaged() {
		rt.baseURL = strings.TrimRight(cfg.BaseURL, "/")
		log.Printf("[ollama] using existing server at %s", redactURL(rt.baseURL))
		return rt, nil
	}

	rt.container = runID + "-ollama"
	args := []string{
		"run", "-d", "--name", rt.container,
		"--label", "federation-autoscaler.test=" + testType,
		"--label", "federation-autoscaler.run=" + runID,
		// Loopback only, on a port Docker picks: the harness is the only client,
		// and a fixed 11434 would collide with an Ollama already on the host.
		"-p", "127.0.0.1::11434",
		"-v", cfg.ModelCacheVolume + ":/root/.ollama",
	}
	if cfg.GPU {
		args = append(args, "--gpus", "all")
	}
	args = append(args, cfg.Image)
	log.Printf("[ollama] starting container %s (%s, model cache volume %s)",
		rt.container, cfg.Image, cfg.ModelCacheVolume)
	if out, err := docker(ctx, args...); err != nil {
		return nil, fmt.Errorf("start Ollama container: %w\n%s", err, out)
	}

	out, err := docker(ctx, "port", rt.container, "11434/tcp")
	if err != nil {
		rt.stop(context.Background(), false)
		return nil, fmt.Errorf("read Ollama host port: %w\n%s", err, out)
	}
	hostPort := firstLine(out)
	if hostPort == "" {
		rt.stop(context.Background(), false)
		return nil, fmt.Errorf("docker reported no host port for %s", rt.container)
	}
	rt.baseURL = "http://" + hostPort
	log.Printf("[ollama] listening on %s", rt.baseURL)
	return rt, nil
}

// beginPreparation waits for the server, makes sure the model is present and
// checks it can answer, all in the background. onFailure runs when that fails,
// so the caller can abandon work that would be wasted -- the cluster setup,
// which otherwise takes many minutes to reach an error known much earlier.
// awaitPrepared collects the result.
func (rt *ollamaRuntime) beginPreparation(ctx context.Context, onFailure func()) {
	rt.done = make(chan struct{})
	go func() {
		rt.prepErr = rt.prepare(ctx)
		// Publish the result before reacting to it: onFailure makes the setup
		// return, and the caller then asks preparationFailure why -- which must
		// already see the error, not a still-running preparation.
		close(rt.done)
		if rt.prepErr != nil {
			onFailure()
		}
	}()
}

func (rt *ollamaRuntime) awaitPrepared(ctx context.Context) error {
	select {
	case <-rt.done:
		return rt.prepErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// preparationFailure returns the preparation error if preparation has already
// failed, and nil if it succeeded or is still running.
func (rt *ollamaRuntime) preparationFailure() error {
	select {
	case <-rt.done:
		return rt.prepErr
	default:
		return nil
	}
}

func (rt *ollamaRuntime) prepare(ctx context.Context) error {
	if err := rt.waitReady(ctx, 3*time.Minute); err != nil {
		return err
	}
	present, err := rt.hasModel(ctx)
	if err != nil {
		return err
	}
	if present {
		log.Printf("[ollama] model %s already available", rt.cfg.Model)
	} else {
		if !*rt.cfg.PullIfMissing {
			return fmt.Errorf("model %s is not available on %s and ollama.pullIfMissing is false; run: ollama pull %s",
				rt.cfg.Model, redactURL(rt.baseURL), rt.cfg.Model)
		}
		pullCtx, cancel := context.WithTimeout(ctx, rt.cfg.PullTimeout)
		err := rt.pull(pullCtx)
		if err != nil && rt.container != "" && isNetworkError(err) {
			// The Ollama server downloads the model with the container's own
			// network. On a host whose containers cannot resolve DNS that fails,
			// although the host itself -- which pulled this image -- can.
			log.Printf("[ollama] the container cannot reach the model registry (%v); "+
				"pulling %s through the host network instead", err, rt.cfg.Model)
			err = rt.pullViaHostNetwork(pullCtx)
		}
		cancel()
		if err != nil {
			return err
		}
		if present, err := rt.hasModel(ctx); err != nil || !present {
			return fmt.Errorf("model %s was pulled but Ollama at %s does not list it (err: %v)",
				rt.cfg.Model, redactURL(rt.baseURL), err)
		}
	}
	// One real selection now, while the clusters are still being created: a
	// runtime that rejects the request shape fails the run in minutes, not
	// after the whole federation is up. A timeout is not held against it here:
	// the setup is compiling images and starting clusters on the same CPUs, and
	// the warm-up before the first scenario checks again on a quiet machine.
	return rt.warmUp(ctx, false)
}

func (rt *ollamaRuntime) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		var v struct {
			Version string `json:"version"`
		}
		if lastErr = rt.getJSON(ctx, "/api/version", &v); lastErr == nil {
			rt.mu.Lock()
			rt.version = v.Version
			rt.mu.Unlock()
			log.Printf("[ollama] server ready, version %s", v.Version)
			return nil
		}
		if err := testlib.SleepCtx(ctx, 2*time.Second); err != nil {
			return err
		}
	}
	return fmt.Errorf("timed out waiting for Ollama at %s after %s: %w", redactURL(rt.baseURL), timeout, lastErr)
}

// serverVersion is the version the server reported, or "" before it answered.
func (rt *ollamaRuntime) serverVersion() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.version
}

// takeWarnings returns the warnings recorded so far and clears them.
func (rt *ollamaRuntime) takeWarnings() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	w := rt.warnings
	rt.warnings = nil
	return w
}

func (rt *ollamaRuntime) hasModel(ctx context.Context) (bool, error) {
	var tags struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := rt.getJSON(ctx, "/api/tags", &tags); err != nil {
		return false, fmt.Errorf("list local models: %w", err)
	}
	want := canonicalModel(rt.cfg.Model)
	for _, m := range tags.Models {
		if canonicalModel(m.Name) == want || canonicalModel(m.Model) == want {
			return true, nil
		}
	}
	return false, nil
}

// pull downloads the model, logging progress roughly every 10%.
func (rt *ollamaRuntime) pull(ctx context.Context) error {
	log.Printf("[ollama] pulling %s into volume %s (first run only)", rt.cfg.Model, rt.cfg.ModelCacheVolume)
	body, _ := json.Marshal(map[string]any{"model": rt.cfg.Model, "stream": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rt.baseURL+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req) // no client timeout: pullCtx bounds it
	if err != nil {
		return fmt.Errorf("pull %s: %w", rt.cfg.Model, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pull %s: HTTP %d", rt.cfg.Model, resp.StatusCode)
	}

	lastDecile := -1
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var ev struct {
			Status    string `json:"status"`
			Error     string `json:"error"`
			Total     int64  `json:"total"`
			Completed int64  `json:"completed"`
		}
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}
		if ev.Error != "" {
			return fmt.Errorf("pull %s: %s", rt.cfg.Model, ev.Error)
		}
		if ev.Total > 0 {
			if decile := int(ev.Completed * 10 / ev.Total); decile != lastDecile {
				lastDecile = decile
				log.Printf("[ollama] pull %s: %d%% of %.1f GB", rt.cfg.Model, decile*10, float64(ev.Total)/1e9)
			}
		}
		if ev.Status == "success" {
			log.Printf("[ollama] model %s pulled", rt.cfg.Model)
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("pull %s: %w", rt.cfg.Model, err)
	}
	return fmt.Errorf("pull %s ended without a success status", rt.cfg.Model)
}

// pullViaHostNetwork downloads the model with a short-lived Ollama container on
// the host's network that shares this run's model cache volume. The run's own
// container then finds the model in the volume; nothing else about it changes.
func (rt *ollamaRuntime) pullViaHostNetwork(ctx context.Context) error {
	port, err := freeLoopbackPort()
	if err != nil {
		return fmt.Errorf("pick a port for the host-network pull: %w", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	name := rt.container + "-pull"
	_, statErr := os.Stat(hostResolvConf)
	args := hostPullArgs(name, addr, rt.cfg.ModelCacheVolume, rt.cfg.Image, statErr == nil)
	if out, err := docker(ctx, args...); err != nil {
		return fmt.Errorf("start the host-network pull container: %w\n%s", err, out)
	}
	defer func() {
		if out, err := docker(context.Background(), "rm", "-f", name); err != nil {
			log.Printf("[ollama] remove %s: %v\n%s", name, err, out)
		}
	}()

	helper := &ollamaRuntime{cfg: rt.cfg, baseURL: "http://" + addr, http: rt.http}
	if err := helper.waitReady(ctx, 2*time.Minute); err != nil {
		return fmt.Errorf("host-network pull container: %w", err)
	}
	if err := helper.pull(ctx); err != nil {
		return fmt.Errorf("pull through the host network: %w", err)
	}
	return nil
}

// hostResolvConf is the host's resolver configuration.
const hostResolvConf = "/etc/resolv.conf"

// hostPullArgs are the `docker run` arguments of the host-network pull
// container. Host networking alone is not enough: Docker still writes the
// container's /etc/resolv.conf itself, and a daemon configured with a DNS
// server the network blocks (seen as "lookup … on 1.1.1.1:53: i/o timeout")
// breaks every container, host-network ones included. Mounting the host's own
// resolv.conf makes the container resolve names exactly as the host does --
// a local stub such as systemd-resolved's 127.0.0.53 is reachable, since the
// container shares the host's network.
func hostPullArgs(name, addr, volume, image string, mountResolvConf bool) []string {
	args := []string{
		"run", "-d", "--name", name, "--network", "host",
		"--label", "federation-autoscaler.test=" + testType,
		"-e", "OLLAMA_HOST=" + addr,
		"-v", volume + ":/root/.ollama",
	}
	if mountResolvConf {
		args = append(args, "-v", hostResolvConf+":/etc/resolv.conf:ro")
	}
	return append(args, image)
}

// isNetworkError reports whether a pull failed because the registry could not
// be reached at all (name resolution, routing, connection), as opposed to a
// failure the registry itself reported, such as an unknown model.
func isNetworkError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"dial tcp", "lookup ", "no such host", "i/o timeout", "connection refused", "connection reset",
		"network is unreachable", "tls handshake timeout", "server misbehaving",
		"temporary failure in name resolution",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// freeLoopbackPort asks the kernel for a free port on 127.0.0.1.
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// warmUp loads the model into memory with one selection on synthetic providers,
// so model load time is not booked as the first scenario's decision latency,
// and checks that the runtime accepts the request the decisions will send.
//
// Only a failure to get an answer at all stops the run (a timeout only when
// timeoutIsFatal). An answer that fails validation is recorded as a warning: it
// says something about the model, which is what the scenarios measure, not that
// the test cannot run.
func (rt *ollamaRuntime) warmUp(ctx context.Context, timeoutIsFatal bool) error {
	opts := rt.clientOptions(nil)
	opts.Timeout = rt.cfg.WarmupTimeout
	client := ollama.NewWithOptions(rt.baseURL, rt.cfg.Model, opts)
	start := time.Now()
	trace, err := client.SelectDetailed(ctx, "Choose the provider with the lowest carbon intensity.", warmUpCandidates())
	if err == nil {
		log.Printf("[ollama] warm-up selection in %s: %v", time.Since(start).Round(time.Millisecond), trace.Ranked)
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if trace.ErrorKind == ollama.ErrKindTimeout && !timeoutIsFatal {
		log.Printf("[ollama] early warm-up timed out after %s while the setup was busy; checking again before the scenarios",
			rt.cfg.WarmupTimeout)
		return nil
	}
	if isTransportFailure(trace.ErrorKind) {
		return fmt.Errorf("model %s did not answer the warm-up selection (%s): %w", rt.cfg.Model, trace.ErrorKind, err)
	}
	warning := fmt.Sprintf("warm-up answer from %s failed validation (%s): %v; raw response %q",
		rt.cfg.Model, trace.ErrorKind, err, trace.RawResponse)
	log.Printf("[ollama] WARNING: %s", warning)
	rt.mu.Lock()
	rt.warnings = append(rt.warnings, warning)
	rt.mu.Unlock()
	return nil
}

// isTransportFailure reports whether a selection failed before any model answer
// could be judged: the server was not reached, timed out, refused the request
// or returned something that is not an Ollama response.
func isTransportFailure(kind ollama.ErrorKind) bool {
	switch kind {
	case ollama.ErrKindUnreachable, ollama.ErrKindTimeout, ollama.ErrKindHTTPStatus, ollama.ErrKindEnvelopeDecode:
		return true
	}
	return false
}

// redactURL drops credentials, query and fragment from a URL before it is
// logged or written to an artifact; an unmanaged Ollama endpoint may carry them.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable URL omitted)"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

// clientOptions builds the selector options every recorded decision uses. They
// are the consumer agent's own: its --ollama-timeout (set to the same value,
// see configureAgent) and the consumer's location. Nothing else is tunable, so
// the harness asks the model exactly what the agent asks it.
func (rt *ollamaRuntime) clientOptions(consumer *ollama.Location) ollama.Options {
	return ollama.Options{Timeout: rt.cfg.Timeout, ConsumerLocation: consumer}
}

// agentURL returns the base URL at which a consumer agent running in the Kind
// node container nodeContainer reaches this Ollama. The loopback port the
// harness uses is not reachable from pods, so a managed container is attached
// to that node's Docker network (it may already be) and addressed by its IP
// there. An unmanaged server is reached at ollama.agentBaseUrl.
func (rt *ollamaRuntime) agentURL(ctx context.Context, nodeContainer string) (string, error) {
	if rt.container == "" {
		return strings.TrimRight(rt.cfg.AgentBaseURL, "/"), nil
	}
	out, err := docker(ctx, "inspect", "-f", "{{range $name, $_ := .NetworkSettings.Networks}}{{$name}}\n{{end}}",
		nodeContainer)
	if err != nil {
		return "", fmt.Errorf("find the Docker network of %s: %w\n%s", nodeContainer, err, out)
	}
	network := firstLine(out)
	if network == "" {
		return "", fmt.Errorf("%s is on no Docker network", nodeContainer)
	}
	if out, err := docker(ctx, "network", "connect", network, rt.container); err != nil &&
		!strings.Contains(string(out), "already exists") {
		return "", fmt.Errorf("attach %s to network %s: %w\n%s", rt.container, network, err, out)
	}
	out, err = docker(ctx, "inspect", "-f",
		fmt.Sprintf("{{(index .NetworkSettings.Networks %q).IPAddress}}", network), rt.container)
	if err != nil {
		return "", fmt.Errorf("read the IP of %s on %s: %w\n%s", rt.container, network, err, out)
	}
	ip := firstLine(out)
	if ip == "" {
		return "", fmt.Errorf("%s has no IP on network %s", rt.container, network)
	}
	containerURL := "http://" + net.JoinHostPort(ip, "11434")
	if err := rt.repointHarness(ctx, containerURL); err != nil {
		return "", err
	}
	return containerURL, nil
}

// repointHarness makes the harness reach Ollama again after the container
// joined the Kind network. The loopback port published at start may no longer
// answer: recent Docker versions move a container's published ports to the
// network that provides its default gateway -- the Kind network, once joined --
// and a random host port can change in the move. The harness therefore tries
// the container's address on that network first (the host routes to it on
// Linux, and it is the same address the agent uses), then the published port
// as Docker reports it now.
func (rt *ollamaRuntime) repointHarness(ctx context.Context, containerURL string) error {
	candidates := []string{containerURL}
	if out, err := docker(ctx, "port", rt.container, "11434/tcp"); err == nil {
		if hostPort := firstLine(out); hostPort != "" {
			candidates = append(candidates, "http://"+hostPort)
		}
	}
	chosen, err := firstAnswering(ctx, candidates, rt.probeVersion, 45*time.Second, 3*time.Second)
	if err != nil {
		return fmt.Errorf("cannot reach Ollama from the harness after joining the Kind network (tried %v): %w",
			candidates, err)
	}
	if chosen != rt.baseURL {
		log.Printf("[ollama] harness now reaches Ollama at %s (was %s)", chosen, rt.baseURL)
	}
	rt.baseURL = chosen
	return nil
}

// probeVersion checks that an Ollama server answers at baseURL, with a short
// timeout so an unroutable candidate fails fast.
func (rt *ollamaRuntime) probeVersion(ctx context.Context, baseURL string) error {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, baseURL+"/api/version", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /api/version: HTTP %d", resp.StatusCode)
	}
	return nil
}

// firstAnswering returns the first candidate probe accepts, retrying all of
// them every interval until timeout. Candidates are tried in order, so the
// earlier ones are preferred whenever more than one answers.
func firstAnswering(ctx context.Context, candidates []string, probe func(context.Context, string) error,
	timeout, interval time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		for _, c := range candidates {
			if lastErr = probe(ctx, c); lastErr == nil {
				return c, nil
			}
		}
		if time.Now().After(deadline) {
			return "", lastErr
		}
		if err := testlib.SleepCtx(ctx, interval); err != nil {
			return "", err
		}
	}
}

// stop removes the container (and optionally the model cache) unless the run
// is being kept for inspection, in which case it prints how to clean up.
func (rt *ollamaRuntime) stop(ctx context.Context, keep bool) {
	if rt == nil || rt.container == "" {
		return
	}
	if keep {
		log.Printf("[ollama] keeping container; remove it with: docker rm -f %s", rt.container)
		return
	}
	if out, err := docker(ctx, "rm", "-f", rt.container); err != nil {
		log.Printf("[ollama] remove container %s: %v\n%s", rt.container, err, out)
	}
	if rt.cfg.RemoveModelCache {
		if out, err := docker(ctx, "volume", "rm", rt.cfg.ModelCacheVolume); err != nil {
			log.Printf("[ollama] remove volume %s: %v\n%s", rt.cfg.ModelCacheVolume, err, out)
		}
	}
}

// logs returns the container's output, or nil when not managed.
func (rt *ollamaRuntime) logs(ctx context.Context) []byte {
	if rt == nil || rt.container == "" {
		return nil
	}
	out, err := docker(ctx, "logs", "--timestamps", rt.container)
	if err != nil {
		return []byte(fmt.Sprintf("docker logs failed: %v\n%s", err, out))
	}
	return out
}

func (rt *ollamaRuntime) getJSON(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rt.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := rt.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// warmUpCandidates are two synthetic providers that differ only in carbon, so
// the warm-up prompt has an unambiguous answer and exercises the real path.
func warmUpCandidates() []brokerapi.NodeGroupView {
	candidate := func(id string, carbon float64) brokerapi.NodeGroupView {
		c := carbon
		return brokerapi.NodeGroupView{
			ID:                "ng-" + id + "-standard",
			ProviderClusterID: id,
			Type:              brokerv1alpha1.ChunkTypeStandard,
			MaxSize:           2,
			ChunkResources: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
			CarbonIntensity: &c,
		}
	}
	return []brokerapi.NodeGroupView{candidate("warmup-a", 600), candidate("warmup-b", 40)}
}

// canonicalModel makes "llama3.2" and "llama3.2:latest" compare equal.
func canonicalModel(name string) string {
	if name == "" || strings.Contains(name, ":") {
		return name
	}
	return name + ":latest"
}

func docker(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	return bytes.TrimSpace(out), err
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}
