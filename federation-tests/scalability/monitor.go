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

// Resource-usage sampling. The Broker is a controller-runtime process with
// no built-in per-process CPU/RAM self-report over its own REST API — the
// only real, already-implemented signals this repository exposes are:
//
//   - the Kubernetes Deployment it always runs as in every deploy path this
//     repo ships (config/broker/deployment.yaml; deploy/standalone/central-up.sh
//     runs `kubectl rollout status deploy/broker` — there is no bare-process
//     or Docker Compose deployment path in this repository at all), sampled
//     via `kubectl top pod` (needs metrics-server, the standard k8s add-on);
//   - `docker stats`, useful only when the target cluster is a local
//     kind/minikube-on-Docker node and the caller supplies the container
//     name directly (this repository does not run the Broker as a bare
//     Docker container in any script);
//   - direct process sampling from Linux /proc, for a Broker running as a
//     local process -- which is how run-scalability-test.sh runs it. CPU is
//     the difference in the process's CPU time between two samples over the
//     wall time between them: the load of that interval, in cores. (`ps
//     %cpu`, used before, is the average over the whole life of the process
//     and would hide the load the test puts on it.)
//
// A fourth, unimplemented option is worth recording for future work: the
// manager's controller-runtime metrics server (--metrics-bind-address,
// wired in internal/manager/manager.go and set to :8443 in
// config/broker/deployment.yaml) exposes the standard Prometheus process
// collector (process_cpu_seconds_total, process_resident_memory_bytes) —
// but it defaults to --metrics-secure=true, requiring a bearer token with
// RBAC access (see config/rbac and config/network-policy/allow-metrics-traffic.yaml),
// and this repository ships no Prometheus server to scrape it from
// (config/prometheus/monitor.yaml only defines a ServiceMonitor, which is
// inert without a Prometheus Operator watching it). Wiring a token-authenticated
// scrape here was left out rather than guessed at; --monitor-mode=k8s is the
// documented default for a real cluster.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ResourceSample is one CPU/RAM observation of the Broker process/pod.
type ResourceSample struct {
	Timestamp time.Time
	CPUValue  float64
	CPUUnit   string // "cores" (k8s/process) or "percent" (docker)
	MemMiB    float64
	Source    string // human-readable description of where the sample came from
}

// MonitorRunner samples Broker resource usage on a fixed interval until ctx
// is cancelled. sampleFn is swapped per --monitor-mode by newMonitor.
type MonitorRunner struct {
	interval time.Duration
	sampleFn func(ctx context.Context) (ResourceSample, error)
	samples  []ResourceSample
	errs     []string
}

func newMonitor(cfg *Config) (*MonitorRunner, error) {
	var fn func(ctx context.Context) (ResourceSample, error)
	switch cfg.MonitorMode {
	case "none":
		return nil, nil
	case "k8s":
		fn = k8sSampler(cfg)
	case "docker":
		fn = dockerSampler(cfg)
	case "process":
		var err error
		if fn, err = processSampler(cfg.BrokerPID); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("monitor: unknown mode %q", cfg.MonitorMode)
	}
	return &MonitorRunner{interval: cfg.MonitorInterval, sampleFn: fn}, nil
}

// Run blocks, sampling every interval, until ctx is cancelled. Sampling
// errors are recorded (not fatal) so a transient `kubectl top` hiccup
// doesn't abort the whole experiment.
func (m *MonitorRunner) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s, err := m.sampleFn(ctx)
			if err != nil {
				m.errs = append(m.errs, err.Error())
				continue
			}
			m.samples = append(m.samples, s)
		}
	}
}

// --- k8s: kubectl top pod ---------------------------------------------------

func k8sSampler(cfg *Config) func(ctx context.Context) (ResourceSample, error) {
	return func(ctx context.Context) (ResourceSample, error) {
		args := []string{"top", "pod", "-n", cfg.BrokerNamespace, "--no-headers", "--containers=false"}
		if cfg.Kubeconfig != "" {
			args = append([]string{"--kubeconfig", cfg.Kubeconfig}, args...)
		}
		if cfg.BrokerPod != "" {
			args = append(args, cfg.BrokerPod)
		} else {
			args = append(args, "-l", cfg.BrokerPodLabel)
		}
		out, err := exec.CommandContext(ctx, "kubectl", args...).Output()
		if err != nil {
			return ResourceSample{}, fmt.Errorf("kubectl top pod: %w", err)
		}

		var cpuCores, memMiB float64
		lines := 0
		sc := bufio.NewScanner(strings.NewReader(string(out)))
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 3 {
				continue
			}
			cpuCores += parseK8sCPU(fields[1])
			memMiB += parseK8sMemMiB(fields[2])
			lines++
		}
		if lines == 0 {
			return ResourceSample{}, fmt.Errorf("kubectl top pod: no matching pod (namespace=%s label=%s pod=%s)",
				cfg.BrokerNamespace, cfg.BrokerPodLabel, cfg.BrokerPod)
		}
		return ResourceSample{
			Timestamp: time.Now(),
			CPUValue:  cpuCores,
			CPUUnit:   "cores",
			MemMiB:    memMiB,
			Source:    "kubectl top pod",
		}, nil
	}
}

// parseK8sCPU parses kubectl top's CPU column ("123m" millicores, or a bare
// core count like "1") into fractional cores.
func parseK8sCPU(s string) float64 {
	if strings.HasSuffix(s, "m") {
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "m"), 64)
		return v / 1000
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// parseK8sMemMiB parses kubectl top's MEMORY column (Ki/Mi/Gi suffix, or
// bare bytes) into MiB.
func parseK8sMemMiB(s string) float64 {
	units := map[string]float64{
		"Ki": 1.0 / 1024, "Mi": 1, "Gi": 1024, "Ti": 1024 * 1024,
		"K": 1000.0 / (1024 * 1024), "M": 1e6 / (1024 * 1024), "G": 1e9 / (1024 * 1024),
	}
	for suffix, mult := range units {
		if strings.HasSuffix(s, suffix) {
			v, _ := strconv.ParseFloat(strings.TrimSuffix(s, suffix), 64)
			return v * mult
		}
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v / (1024 * 1024)
}

// --- docker: docker stats ---------------------------------------------------

func dockerSampler(cfg *Config) func(ctx context.Context) (ResourceSample, error) {
	return func(ctx context.Context) (ResourceSample, error) {
		out, err := exec.CommandContext(ctx, "docker", "stats", "--no-stream",
			"--format", "{{.CPUPerc}}\t{{.MemUsage}}", cfg.BrokerContainer).Output()
		if err != nil {
			return ResourceSample{}, fmt.Errorf("docker stats: %w", err)
		}
		fields := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
		if len(fields) != 2 {
			return ResourceSample{}, fmt.Errorf("docker stats: unexpected output %q", string(out))
		}
		cpuPct, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(fields[0]), "%"), 64)
		memMiB := parseDockerMem(fields[1])
		return ResourceSample{
			Timestamp: time.Now(),
			CPUValue:  cpuPct,
			CPUUnit:   "percent",
			MemMiB:    memMiB,
			Source:    "docker stats container=" + cfg.BrokerContainer,
		}, nil
	}
}

// parseDockerMem parses docker stats' "12.34MiB / 256MiB" MemUsage column,
// keeping only the usage side, converted to MiB.
func parseDockerMem(s string) float64 {
	usage := strings.TrimSpace(strings.SplitN(s, "/", 2)[0])
	switch {
	case strings.HasSuffix(usage, "GiB"):
		v, _ := strconv.ParseFloat(strings.TrimSuffix(usage, "GiB"), 64)
		return v * 1024
	case strings.HasSuffix(usage, "MiB"):
		v, _ := strconv.ParseFloat(strings.TrimSuffix(usage, "MiB"), 64)
		return v
	case strings.HasSuffix(usage, "KiB"):
		v, _ := strconv.ParseFloat(strings.TrimSuffix(usage, "KiB"), 64)
		return v / 1024
	default:
		v, _ := strconv.ParseFloat(usage, 64)
		return v
	}
}

// --- process: Linux /proc/<pid> ---------------------------------------------

// userHZ is the unit of utime/stime in /proc/<pid>/stat. Linux reports them
// in USER_HZ clock ticks, fixed at 100 for every userspace interface
// whatever the kernel's internal tick rate.
const userHZ = 100

// procReadFile reads a /proc file; tests swap it for canned content.
var procReadFile = os.ReadFile

// processSampler samples the Broker process from /proc. It takes the first
// CPU reading right away, as the baseline the first sample is measured from,
// so a wrong PID or a system without /proc fails here, before the run,
// rather than as a column of sampling errors afterwards.
func processSampler(pid int) (func(ctx context.Context) (ResourceSample, error), error) {
	prevTicks, err := procCPUTicks(pid)
	if err != nil {
		return nil, fmt.Errorf("monitor-mode=process reads the Broker's /proc/<pid> and needs Linux "+
			"and the Broker's own PID: %w", err)
	}
	prevAt := time.Now()
	return func(context.Context) (ResourceSample, error) {
		ticks, err := procCPUTicks(pid)
		if err != nil {
			return ResourceSample{}, err
		}
		rss, err := procRSSMiB(pid)
		if err != nil {
			return ResourceSample{}, err
		}
		now := time.Now()
		cores := cpuCores(prevTicks, ticks, now.Sub(prevAt))
		prevTicks, prevAt = ticks, now
		return ResourceSample{
			Timestamp: now,
			CPUValue:  cores,
			CPUUnit:   "cores",
			MemMiB:    rss,
			Source:    fmt.Sprintf("/proc pid=%d", pid),
		}, nil
	}, nil
}

// cpuCores is the CPU the process used between two readings, in cores: 1.0
// is one core busy for the whole interval.
func cpuCores(prevTicks, ticks uint64, elapsed time.Duration) float64 {
	if elapsed <= 0 || ticks < prevTicks {
		return 0
	}
	return float64(ticks-prevTicks) / userHZ / elapsed.Seconds()
}

// procCPUTicks returns utime+stime of pid, in USER_HZ ticks. The fields are
// counted after the last ')' because the second field, the command name in
// parentheses, may itself contain spaces and parentheses.
func procCPUTicks(pid int) (uint64, error) {
	path := fmt.Sprintf("/proc/%d/stat", pid)
	raw, err := procReadFile(path)
	if err != nil {
		return 0, err
	}
	s := string(raw)
	end := strings.LastIndex(s, ")")
	if end < 0 {
		return 0, fmt.Errorf("%s: no command name in %q", path, s)
	}
	// After ')' come field 3 (state) onwards: utime is field 14, stime 15.
	fields := strings.Fields(s[end+1:])
	const utime, stime = 14 - 3, 15 - 3
	if len(fields) <= stime {
		return 0, fmt.Errorf("%s: only %d fields after the command name", path, len(fields))
	}
	u, err := strconv.ParseUint(fields[utime], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: utime: %w", path, err)
	}
	st, err := strconv.ParseUint(fields[stime], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: stime: %w", path, err)
	}
	return u + st, nil
}

// procRSSMiB returns the resident set size of pid (VmRSS in
// /proc/<pid>/status, reported in kB) in MiB.
func procRSSMiB(pid int) (float64, error) {
	path := fmt.Sprintf("/proc/%d/status", pid)
	raw, err := procReadFile(path)
	if err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "VmRSS:" {
			kb, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				return 0, fmt.Errorf("%s: VmRSS: %w", path, err)
			}
			return kb / 1024, nil
		}
	}
	return 0, fmt.Errorf("%s: no VmRSS line", path)
}
