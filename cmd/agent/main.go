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

// Command agent is the federation-autoscaler Agent running on every consumer
// and provider cluster. Unlike the Broker and the gRPC server it does not own
// any CRDs on its local cluster: it is a pure HTTP client that polls the
// Broker every --poll-interval (default 5 s) and, when --role=consumer, also
// exposes a loopback-only REST server that the co-located gRPC server calls
// to request scale-ups / scale-downs.
//
// See docs/design.md §4.3 (Consumer Agent) and §4.4 (Provider Agent).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	roleConsumer = "consumer"
	roleProvider = "provider"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

// nolint:gocyclo
func main() {
	var (
		role            string
		clusterID       string
		liqoClusterID   string
		brokerURL       string
		clientCertPath  string
		clientKeyPath   string
		brokerCAPath    string
		pollInterval    time.Duration
		localAPIAddr    string
		healthProbeAddr string
	)

	flag.StringVar(&role, "role", "",
		"Agent role; must be one of: consumer, provider.")
	flag.StringVar(&clusterID, "cluster-id", "",
		"Broker-facing identifier of this cluster (must match the CN of --client-cert).")
	flag.StringVar(&liqoClusterID, "liqo-cluster-id", "",
		"Liqo cluster identifier of this cluster (from `liqoctl status`).")
	flag.StringVar(&brokerURL, "broker-url", "",
		"HTTPS URL of the Broker (e.g. https://broker.example.com:8443).")
	flag.StringVar(&clientCertPath, "client-cert", "",
		"Path to the agent's mTLS client certificate (PEM).")
	flag.StringVar(&clientKeyPath, "client-key", "",
		"Path to the agent's mTLS client key (PEM).")
	flag.StringVar(&brokerCAPath, "broker-ca", "",
		"Path to the CA bundle that signs the Broker's server certificate (PEM).")
	flag.DurationVar(&pollInterval, "poll-interval", 5*time.Second,
		"Interval between GET /api/v1/instructions polls against the Broker.")
	flag.StringVar(&localAPIAddr, "local-api-bind-address", "127.0.0.1:9090",
		"(consumer role only) Address the loopback REST API binds to; consumed by the local gRPC server.")
	flag.StringVar(&healthProbeAddr, "health-probe-bind-address", ":8081",
		"Address the health/readiness probe endpoint binds to.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if err := validateFlags(role, clusterID, liqoClusterID, brokerURL,
		clientCertPath, clientKeyPath, brokerCAPath); err != nil {
		setupLog.Error(err, "invalid configuration")
		os.Exit(1)
	}

	setupLog.Info("starting agent",
		"role", role,
		"clusterID", clusterID,
		"liqoClusterID", liqoClusterID,
		"brokerURL", brokerURL,
		"pollInterval", pollInterval)

	// Local-cluster client is used by the poll loop and the local REST API to
	// interact with the Kubernetes API of this cluster (create ResourceSlices
	// / NamespaceOffloading on consumers, run liqoctl / read node info on
	// providers). It is deliberately NOT a controller-runtime Manager: the
	// agent reconciles no CRDs locally.
	cfg := ctrl.GetConfigOrDie()
	localClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to build local-cluster client")
		os.Exit(1)
	}
	_ = localClient // wired up in a later step (polling + local API).

	ctx := ctrl.SetupSignalHandler()

	go startHealthProbe(ctx, healthProbeAddr)

	switch role {
	case roleConsumer:
		setupLog.Info("consumer role selected",
			"localAPIAddr", localAPIAddr,
			"TODO", "start local REST API + poll loop for ReservationInstructions")
	case roleProvider:
		setupLog.Info("provider role selected",
			"TODO", "start advertisement publisher + poll loop for ProviderInstructions")
	}

	<-ctx.Done()
	setupLog.Info("shutdown signal received, exiting")
}

// validateFlags rejects invalid or incomplete CLI configuration up front so
// the agent fails fast at start-up instead of mid-poll.
func validateFlags(role, clusterID, liqoClusterID, brokerURL,
	clientCertPath, clientKeyPath, brokerCAPath string) error {
	if role != roleConsumer && role != roleProvider {
		return fmt.Errorf("--role must be %q or %q, got %q",
			roleConsumer, roleProvider, role)
	}
	if clusterID == "" {
		return fmt.Errorf("--cluster-id is required")
	}
	if liqoClusterID == "" {
		return fmt.Errorf("--liqo-cluster-id is required")
	}
	if brokerURL == "" {
		return fmt.Errorf("--broker-url is required")
	}
	if clientCertPath == "" || clientKeyPath == "" || brokerCAPath == "" {
		return fmt.Errorf("--client-cert, --client-key and --broker-ca are all required (mTLS is mandatory)")
	}
	return nil
}

// startHealthProbe serves /healthz and /readyz on probeAddr. Both endpoints
// return 200 as long as the process is alive; readyz will grow real checks
// (broker reachability, local-API readiness) once polling is wired in.
func startHealthProbe(ctx context.Context, probeAddr string) {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/readyz", ok)

	srv := &http.Server{
		Addr:              probeAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	setupLog.Info("health probe listening", "address", probeAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		setupLog.Error(err, "health probe server failed")
	}
}
