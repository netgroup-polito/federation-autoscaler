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

// Package manager centralises the controller-runtime bootstrap that is
// identical between the broker and the grpc-server binaries: flag wiring,
// HTTP/2-disable workaround for CVE-2023-44487, metrics/webhook server
// options, and health/readiness probes. The agent binary does not use this
// package because it owns no CRDs and therefore runs without a manager.
package manager

import (
	"crypto/tls"
	"flag"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// Config holds the CLI-settable portion of the common bootstrap. Call
// BindFlags to populate it from a flag.FlagSet.
type Config struct {
	MetricsAddr          string
	ProbeAddr            string
	SecureMetrics        bool
	EnableLeaderElection bool
	EnableHTTP2          bool

	WebhookCertPath string
	WebhookCertName string
	WebhookCertKey  string

	MetricsCertPath string
	MetricsCertName string
	MetricsCertKey  string
}

// Options carries the code-level (non-CLI) settings that differ between
// binaries: which scheme to use, which leader-election lease to take, and
// a human-readable component name used in setup logs.
type Options struct {
	// Scheme is the runtime.Scheme the manager will serve. Typically
	// pre-populated with clientgoscheme plus the CRD groups this binary
	// reconciles.
	Scheme *runtime.Scheme

	// LeaderElectionID is the name of the Lease the manager acquires when
	// leader election is enabled. Must be unique across federation-autoscaler
	// components so two binaries sharing a kubeconfig cannot starve each
	// other (e.g. "broker.federation-autoscaler.io").
	LeaderElectionID string
}

// BindFlags registers the common flags on fs and returns a Config whose
// fields are populated after the caller invokes fs.Parse().
//
// It is the caller's responsibility to also bind zap.Options on the same
// FlagSet if structured logging flags are desired.
func BindFlags(fs *flag.FlagSet) *Config {
	c := &Config{}
	fs.StringVar(&c.MetricsAddr, "metrics-bind-address", "0",
		"The address the metrics endpoint binds to. "+
			"Use :8443 for HTTPS, :8080 for HTTP, or 0 to disable the metrics service.")
	fs.StringVar(&c.ProbeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	fs.BoolVar(&c.EnableLeaderElection, "leader-elect", false,
		"Enable leader election; ensures only one active manager at a time.")
	fs.BoolVar(&c.SecureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS.")
	fs.BoolVar(&c.EnableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers.")
	fs.StringVar(&c.WebhookCertPath, "webhook-cert-path", "",
		"Directory that contains the webhook certificate.")
	fs.StringVar(&c.WebhookCertName, "webhook-cert-name", "tls.crt",
		"File name (under --webhook-cert-path) of the webhook certificate.")
	fs.StringVar(&c.WebhookCertKey, "webhook-cert-key", "tls.key",
		"File name (under --webhook-cert-path) of the webhook key.")
	fs.StringVar(&c.MetricsCertPath, "metrics-cert-path", "",
		"Directory that contains the metrics server certificate.")
	fs.StringVar(&c.MetricsCertName, "metrics-cert-name", "tls.crt",
		"File name (under --metrics-cert-path) of the metrics server certificate.")
	fs.StringVar(&c.MetricsCertKey, "metrics-cert-key", "tls.key",
		"File name (under --metrics-cert-path) of the metrics server key.")
	return c
}

// New builds a controller-runtime manager from cfg and opts, wires the
// standard /healthz and /readyz probes, and returns it ready for the caller
// to register reconcilers and runnables on. The caller is expected to start
// the manager with mgr.Start(ctrl.SetupSignalHandler()).
func New(cfg *Config, opts Options) (manager.Manager, error) {
	if opts.Scheme == nil {
		return nil, fmt.Errorf("manager.New: Options.Scheme must not be nil")
	}
	if opts.LeaderElectionID == "" {
		return nil, fmt.Errorf("manager.New: Options.LeaderElectionID must not be empty")
	}

	setupLog := ctrl.Log.WithName("setup").WithValues("leaderElectionID", opts.LeaderElectionID)

	var tlsOpts []func(*tls.Config)
	if !cfg.EnableHTTP2 {
		// HTTP/2 is disabled by default to avoid CVE-2023-44487 (Rapid Reset)
		// and CVE-2023-39325 (Stream Cancellation). Operators can opt back
		// in per-binary via --enable-http2.
		tlsOpts = append(tlsOpts, func(c *tls.Config) {
			setupLog.Info("disabling http/2")
			c.NextProtos = []string{"http/1.1"}
		})
	}

	webhookOpts := webhook.Options{TLSOpts: tlsOpts}
	if cfg.WebhookCertPath != "" {
		setupLog.Info("initializing webhook certificate watcher",
			"webhook-cert-path", cfg.WebhookCertPath,
			"webhook-cert-name", cfg.WebhookCertName,
			"webhook-cert-key", cfg.WebhookCertKey)
		webhookOpts.CertDir = cfg.WebhookCertPath
		webhookOpts.CertName = cfg.WebhookCertName
		webhookOpts.KeyName = cfg.WebhookCertKey
	}

	metricsOpts := metricsserver.Options{
		BindAddress:   cfg.MetricsAddr,
		SecureServing: cfg.SecureMetrics,
		TLSOpts:       tlsOpts,
	}
	if cfg.SecureMetrics {
		// Require valid service-account-backed bearer tokens for /metrics;
		// see the RBAC stanza under config/rbac.
		metricsOpts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if cfg.MetricsCertPath != "" {
		setupLog.Info("initializing metrics certificate watcher",
			"metrics-cert-path", cfg.MetricsCertPath,
			"metrics-cert-name", cfg.MetricsCertName,
			"metrics-cert-key", cfg.MetricsCertKey)
		metricsOpts.CertDir = cfg.MetricsCertPath
		metricsOpts.CertName = cfg.MetricsCertName
		metricsOpts.KeyName = cfg.MetricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 opts.Scheme,
		Metrics:                metricsOpts,
		WebhookServer:          webhook.NewServer(webhookOpts),
		HealthProbeBindAddress: cfg.ProbeAddr,
		LeaderElection:         cfg.EnableLeaderElection,
		LeaderElectionID:       opts.LeaderElectionID,
	})
	if err != nil {
		return nil, fmt.Errorf("building controller manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("adding /healthz: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("adding /readyz: %w", err)
	}
	return mgr, nil
}
