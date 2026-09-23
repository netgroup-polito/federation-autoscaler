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

// Command broker-scalability-test is a Broker-only load generator (see
// federation-tests/scalability/README.md). It reuses the real agent-side mTLS HTTP
// client (internal/agent/client) and the real wire types
// (internal/broker/api) so its request shapes can never drift from what a
// genuine Provider/Consumer Agent sends — no endpoint, schema, or auth
// mechanism in this program is invented; every one is imported from the
// package it is defined in.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config is the full set of parameters for one experiment run. Every field
// is either a CLI flag (see bindFlags) or derived at Validate() time.
type Config struct {
	Consumers int
	Providers int
	Duration  time.Duration

	BrokerURL        string
	BrokerServerName string // TLS ServerName override; empty = derive from BrokerURL host

	CertsDir string // output of generate-test-certs.sh
	TLSCert  string // shared single client cert (alternative to CertsDir; providers+consumers must be <=1 each)
	TLSKey   string
	TLSCA    string // required when CertsDir is empty

	OutputDir string

	ConsumerEvalInterval    time.Duration
	AdvertisementInterval   time.Duration
	HeartbeatInterval       time.Duration
	InstructionPollInterval time.Duration
	InstructionPoll         bool

	MonitorInterval time.Duration
	MonitorMode     string // none|k8s|docker|process
	BrokerNamespace string
	BrokerPodLabel  string
	BrokerPod       string
	BrokerContainer string
	BrokerPID       int
	Kubeconfig      string

	RequestTimeout   time.Duration
	ClientMaxRetries int
	WarmupTimeout    time.Duration

	ProviderCPU    string
	ProviderMemory string

	Seed int64

	// RunID identifies this experiment for logging; derived from OutputDir's
	// timestamp unless the directory already existed (resume not supported).
	RunID string
}

const clusterIDPrefix = "scaltest"

func bindFlags(fs *flag.FlagSet) *Config {
	cfg := &Config{}

	fs.IntVar(&cfg.Consumers, "consumers", 1, "Number of logical Consumers to simulate.")
	fs.IntVar(&cfg.Providers, "providers", 1, "Number of logical Providers to simulate.")
	fs.DurationVar(&cfg.Duration, "duration", 60*time.Second, "Measurement-phase duration (after warm-up).")

	fs.StringVar(&cfg.BrokerURL, "broker-url", "", "Broker REST API base URL, e.g. https://localhost:9443 (required, https only).")
	fs.StringVar(&cfg.BrokerServerName, "broker-server-name", "", "TLS ServerName to verify the Broker's server certificate against "+
		"(overrides the host parsed from --broker-url; needed when dialing via kubectl port-forward to 127.0.0.1/localhost while the "+
		"server cert only covers the in-cluster Service DNS names, e.g. broker.federation-autoscaler-system.svc).")

	fs.StringVar(&cfg.CertsDir, "certs-dir", "", "Directory produced by generate-test-certs.sh: ca.crt plus "+
		"scaltest-provider-NNN.{crt,key} / scaltest-consumer-NNN.{crt,key} per logical agent.")
	fs.StringVar(&cfg.TLSCert, "tls-cert", "", "Single shared client certificate (alternative to --certs-dir; only valid with <=1 provider and <=1 consumer).")
	fs.StringVar(&cfg.TLSKey, "tls-key", "", "Private key matching --tls-cert.")
	fs.StringVar(&cfg.TLSCA, "tls-ca", "", "CA bundle to verify the Broker's server certificate (required with --tls-cert; ignored with --certs-dir, which uses <dir>/ca.crt).")

	fs.StringVar(&cfg.OutputDir, "output-dir", "", "Output directory (default results/<UTC timestamp>).")

	fs.DurationVar(&cfg.ConsumerEvalInterval, "consumer-eval-interval", 5*time.Second, "Interval between GET /api/v1/nodegroups calls per consumer.")
	fs.DurationVar(&cfg.AdvertisementInterval, "advertisement-interval", 30*time.Second, "Interval between POST /api/v1/advertisements calls per provider (matches the real Provider Agent's cadence).")
	fs.DurationVar(&cfg.HeartbeatInterval, "heartbeat-interval", 15*time.Second, "Interval between POST /api/v1/heartbeat calls per consumer (matches the real Consumer Agent's cadence).")
	fs.DurationVar(&cfg.InstructionPollInterval, "instruction-poll-interval", 5*time.Second, "Interval between GET /api/v1/instructions calls per agent (matches the real agents' poller cadence).")
	fs.BoolVar(&cfg.InstructionPoll, "instruction-poll", true, "Exercise GET /api/v1/instructions from every logical agent. "+
		"With no reservations/controllers running, the Broker always answers an empty list — see README Limitations.")

	fs.DurationVar(&cfg.MonitorInterval, "monitor-interval", 5*time.Second, "Broker CPU/RAM sampling interval.")
	fs.StringVar(&cfg.MonitorMode, "monitor-mode", "none", "How to sample Broker resource usage: none|k8s|docker|process.")
	fs.StringVar(&cfg.BrokerNamespace, "broker-namespace", "federation-autoscaler-system", "Namespace the Broker Deployment runs in (k8s monitor mode; matches config/broker/deployment.yaml).")
	fs.StringVar(&cfg.BrokerPodLabel, "broker-pod-label", "app.kubernetes.io/component=broker", "Label selector for `kubectl top pod` (k8s monitor mode; matches config/broker/deployment.yaml).")
	fs.StringVar(&cfg.BrokerPod, "broker-pod", "", "Exact Broker pod name, overriding --broker-pod-label (k8s monitor mode).")
	fs.StringVar(&cfg.BrokerContainer, "broker-container", "", "Docker container name or ID running the Broker (docker monitor mode).")
	fs.IntVar(&cfg.BrokerPID, "broker-pid", 0, "PID of the running Broker binary (process monitor mode; "+
		"run-scalability-test.sh passes it). Not the PID of `go run`, which is the go tool's, not the Broker's.")
	fs.StringVar(&cfg.Kubeconfig, "kubeconfig", "", "Kubeconfig path passed to kubectl (k8s monitor mode and cleanup subcommand); empty uses kubectl's default resolution.")

	fs.DurationVar(&cfg.RequestTimeout, "request-timeout", 10*time.Second, "Per-HTTP-attempt timeout (matches internal/agent/client.DefaultRequestTimeout).")
	fs.IntVar(&cfg.ClientMaxRetries, "client-max-retries", 0, "Additional attempts the agent HTTP client makes on "+
		"transient failures for idempotent calls. 0 (the default) means the client's own default, 3 retries, exactly as "+
		"the real agents run (internal/agent/client treats any value <= 0 as DefaultMaxRetries): a request that fails and "+
		"then succeeds on a retry is recorded as one success whose latency includes the backoff. A single attempt is "+
		"not possible with the production client.")
	fs.DurationVar(&cfg.WarmupTimeout, "warmup-timeout", 60*time.Second, "Max time to wait for every provider (then every consumer) to succeed once before starting measurement traffic.")

	fs.StringVar(&cfg.ProviderCPU, "provider-cpu", "16", "Synthetic cpu quantity each logical provider advertises (resource.Quantity syntax, e.g. \"16\").")
	fs.StringVar(&cfg.ProviderMemory, "provider-memory", "32Gi", "Synthetic memory quantity each logical provider advertises (resource.Quantity syntax, e.g. \"32Gi\").")

	fs.Int64Var(&cfg.Seed, "seed", time.Now().UnixNano(), "Seed for the deterministic-per-agent synthetic data generator (topology, prices, carbon intensity). "+
		"Fixed by default only in the sense that it is printed to configuration.json for reproducibility; pass an explicit value to repeat a prior run's synthetic payloads exactly.")

	return cfg
}

// Validate checks parameter sanity and fills in derived defaults
// (OutputDir, RunID). It does not touch the filesystem beyond MkdirAll on
// OutputDir, and does not perform any network I/O.
func (c *Config) Validate() error {
	if c.Consumers < 0 || c.Providers < 0 {
		return fmt.Errorf("--consumers and --providers must be >= 0")
	}
	if c.Consumers+c.Providers < 1 {
		return fmt.Errorf("at least one of --consumers / --providers must be > 0")
	}
	if c.BrokerURL == "" {
		return fmt.Errorf("--broker-url is required, e.g. https://localhost:9443 " +
			"(the Broker's config/broker/deployment.yaml default; --api-bind-address defaults to :8443 when run outside that manifest)")
	}
	if c.CertsDir == "" && (c.TLSCert == "" || c.TLSKey == "" || c.TLSCA == "") {
		return fmt.Errorf("either --certs-dir, or all of --tls-cert/--tls-key/--tls-ca, is required " +
			"(the Broker's ClusterIDMiddleware rejects any connection without a verified client certificate)")
	}
	if c.CertsDir != "" && (c.TLSCert != "" || c.TLSKey != "") {
		return fmt.Errorf("--certs-dir and --tls-cert/--tls-key are mutually exclusive")
	}
	if c.TLSCert != "" && (c.Providers > 1 || c.Consumers > 1) {
		return fmt.Errorf("--tls-cert is a single shared identity: body.clusterId must equal the cert's CN on every request, " +
			"so it only supports <=1 provider and <=1 consumer; use --certs-dir for more")
	}
	if c.Duration <= 0 {
		return fmt.Errorf("--duration must be > 0")
	}
	switch c.MonitorMode {
	case "none":
	case "k8s":
		// broker-pod-label has a usable default; nothing further required.
	case "docker":
		if c.BrokerContainer == "" {
			return fmt.Errorf("--monitor-mode=docker requires --broker-container")
		}
	case "process":
		if c.BrokerPID <= 0 {
			return fmt.Errorf("--monitor-mode=process requires --broker-pid")
		}
	default:
		return fmt.Errorf("--monitor-mode must be one of none|k8s|docker|process (got %q)", c.MonitorMode)
	}

	if c.OutputDir == "" {
		c.OutputDir = filepath.Join("results", time.Now().UTC().Format("20060102T150405Z"))
	}
	if c.RunID == "" {
		c.RunID = filepath.Base(c.OutputDir)
	}
	if err := os.MkdirAll(c.OutputDir, 0o755); err != nil {
		return fmt.Errorf("create output dir %q: %w", c.OutputDir, err)
	}
	return nil
}
