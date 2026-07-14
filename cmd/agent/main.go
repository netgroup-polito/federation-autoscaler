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
	"flag"
	"fmt"
	"os"
	"time"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/health"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/poller"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/provider"
)

const (
	roleConsumer = "consumer"
	roleProvider = "provider"

	// defaultNamespace is where the consumer agent creates its
	// VirtualNodeState CRs, Liqo ResourceSlices, and kubeconfig Secrets
	// when neither --namespace nor POD_NAMESPACE is set. It matches the
	// namespace the agent itself is deployed into so all
	// federation-autoscaler resources live together; in-cluster the
	// downward-API POD_NAMESPACE env overrides it (see config/agent).
	defaultNamespace = "federation-autoscaler-system"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	// VirtualNodeState is read by the consumer Reconcile handler
	// (step 9e) and produced by the gRPC server's reconciler (step 11).
	// Registering it on the agent's scheme lets the local-cluster
	// client list / get the CRs without resorting to unstructured.
	utilruntime.Must(autoscalingv1alpha1.AddToScheme(scheme))
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
		consoleAddr     string
		healthProbeAddr string
		namespace       string
		priceFile       string
		capacityFile    string
		renewableFile   string
		advertisedIP    string
		mockEcoURL      string
		mockGeoURL      string
		probeUDPPort    int
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
	flag.StringVar(&consoleAddr, "console-bind-address", "",
		"Address the role-specific config console (a plain-HTTP, UNAUTHENTICATED web UI "+
			"for setting policy/region/workload on a consumer, or prices/region/capacity on a "+
			"provider) binds to, e.g. :9095. Empty ⇒ console disabled. Exposing it on a NodePort "+
			"lets anyone with node network reach mutate cluster state — demo use only.")
	flag.StringVar(&healthProbeAddr, "health-probe-bind-address", ":8081",
		"Address the health/readiness probe endpoint binds to.")
	flag.StringVar(&namespace, "namespace", envOrDefault("POD_NAMESPACE", defaultNamespace),
		"(consumer role only) Namespace where the agent creates VirtualNodeState, "+
			"Liqo ResourceSlice, and kubeconfig Secret resources. Defaults to "+
			"$POD_NAMESPACE (downward API) or "+defaultNamespace+".")
	flag.StringVar(&priceFile, "price-file", "",
		"(provider role only) Path to a YAML/JSON file of per-resource unit prices "+
			"(e.g. {\"cpu\":\"0.03\",\"memory\":\"4Mi\"}). Re-read every advertisement "+
			"cycle so prices can change without a restart. Empty ⇒ advertise no price.")
	flag.StringVar(&capacityFile, "capacity-file", "",
		"(provider role only) Path to a YAML/JSON file of per-resource advertised-"+
			"capacity percentages (e.g. {\"cpu\":100,\"memory\":50}). A value in (0,100) "+
			"advertises that fraction of the resource's allocatable; 100, >100, ≤0, or "+
			"unset advertise the full allocatable. Re-read every advertisement cycle so "+
			"the cap can change without a restart. Empty ⇒ advertise full allocatable.")
	flag.StringVar(&renewableFile, "renewable-file", "",
		"(provider role only) Path to a YAML/JSON file with this cluster's self-"+
			"declared renewable-energy flag (e.g. {\"renewable\":true}). Re-read every "+
			"cycle so it can toggle without a restart. The standard composite default "+
			"policy gives renewable providers a placement bonus. Empty/false ⇒ no bonus.")
	flag.StringVar(&advertisedIP, "advertised-ip", "",
		"Optional IP override for automatic location discovery (the demo/steering "+
			"lever). When set, this IP is geolocated instead of the agent's own node "+
			"IP. Empty ⇒ discover the node IP from v1.Node (ExternalIP, then "+
			"InternalIP) using the NODE_NAME env.")
	flag.StringVar(&mockEcoURL, "mock-eco-url", "",
		"(provider role only) Base URL of the carbon-intensity service, e.g. "+
			"http://mock-eco:8081. Keyed by the discovered region code. Empty ⇒ "+
			"advertise no carbon intensity.")
	flag.StringVar(&mockGeoURL, "mock-geo-url", "",
		"Base URL of the geo-IP service, e.g. http://mock-geo:8080. Used by both "+
			"roles to resolve this cluster's node IP to a region + coordinates. "+
			"Empty ⇒ advertise no location (the eco/latency strategies then have no "+
			"effect for this cluster).")
	flag.IntVar(&probeUDPPort, "probe-udp-port", 0,
		"(provider role only) The always-on UDP NodePort the udpecho responder is "+
			"exposed on; the provider advertises <nodeIP>:<port> as its measured-"+
			"latency probe endpoint. Must match the agent-probe Service's nodePort. "+
			"0 ⇒ advertise no probe endpoint (latency falls back to distance).")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if err := validateFlags(role, clusterID, liqoClusterID, brokerURL,
		clientCertPath, clientKeyPath, brokerCAPath); err != nil {
		setupLog.Error(err, "invalid configuration")
		os.Exit(1)
	}

	// The node this pod runs on, injected via the NODE_NAME downward-API env
	// (spec.nodeName). Its IP is auto-discovered and geolocated for the eco/
	// latency placement strategies; --advertised-ip overrides it.
	nodeName := os.Getenv("NODE_NAME")

	setupLog.Info("starting agent",
		"role", role,
		"clusterID", clusterID,
		"liqoClusterID", liqoClusterID,
		"brokerURL", brokerURL,
		"pollInterval", pollInterval,
		"nodeName", nodeName,
		"advertisedIP", advertisedIP)

	// Local-cluster client is used by the poll loop and the local REST API to
	// interact with the Kubernetes API of this cluster (create ResourceSlices
	// / NamespaceOffloading on consumers, run liqoctl / read node info on
	// providers). It is deliberately NOT a controller-runtime Manager: the
	// agent reconciles no CRDs locally. Steps 8 and 9 wire role-specific
	// usage on top.
	cfg := ctrl.GetConfigOrDie()
	localClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to build local-cluster client")
		os.Exit(1)
	}

	// Broker HTTP client (mTLS).
	brokerClient, err := agentclient.New(agentclient.Options{
		BrokerURL: brokerURL,
		TLS: agentclient.TLSConfig{
			CertFile:     clientCertPath,
			KeyFile:      clientKeyPath,
			BrokerCAFile: brokerCAPath,
		},
		Logger: ctrl.Log.WithName("broker-client"),
	})
	if err != nil {
		setupLog.Error(err, "unable to build broker client")
		os.Exit(1)
	}

	// Liveness/readiness probe. The poller's OnPollResult callback flips
	// /readyz green on the first successful tick and red after staleness.
	probe := health.New(health.Options{})

	// Handler registry. Step 8 (provider role) and step 9 (consumer role)
	// populate it with the role-specific ProviderInstruction /
	// ReservationInstruction handlers respectively. For now the registry
	// is empty: the agent runs the full poll loop end-to-end (so the
	// broker sees a live caller and /readyz flips green), and any
	// instruction the broker queues is reported back as Failed with
	// "no handler for kind X" — operators see the misconfiguration
	// immediately.
	registry := poller.NewRegistry()

	pollerInstance, err := poller.New(poller.Options{
		Client:       brokerClient,
		Registry:     registry,
		Interval:     pollInterval,
		Logger:       ctrl.Log.WithName("poller"),
		OnPollResult: probe.RecordPoll,
	})
	if err != nil {
		setupLog.Error(err, "unable to build poller")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	go func() {
		if err := probe.Serve(ctx, healthProbeAddr, ctrl.Log.WithName("health")); err != nil {
			setupLog.Error(err, "health probe server failed")
		}
	}()

	switch role {
	case roleConsumer:
		if err := consumer.Run(ctx, consumer.Options{
			Client:        brokerClient,
			Registry:      registry,
			LocalClient:   localClient,
			ClusterID:     clusterID,
			LiqoClusterID: liqoClusterID,
			LocalAPIAddr:  localAPIAddr,
			ConsoleAddr:   consoleAddr,
			Namespace:     namespace,
			NodeName:      nodeName,
			AdvertisedIP:  advertisedIP,
			MockGeoURL:    mockGeoURL,
			Logger:        ctrl.Log.WithName("consumer"),
			Probe:         probe,
		}); err != nil {
			setupLog.Error(err, "failed to bootstrap consumer role")
			os.Exit(1)
		}
	case roleProvider:
		if err := provider.Run(ctx, provider.Options{
			Client:        brokerClient,
			Registry:      registry,
			LocalClient:   localClient,
			ClusterID:     clusterID,
			LiqoClusterID: liqoClusterID,
			PriceFile:     priceFile,
			CapacityFile:  capacityFile,
			RenewableFile: renewableFile,
			NodeName:      nodeName,
			AdvertisedIP:  advertisedIP,
			MockEcoURL:    mockEcoURL,
			MockGeoURL:    mockGeoURL,
			ProbeUDPPort:  probeUDPPort,
			ConsoleAddr:   consoleAddr,
			Logger:        ctrl.Log.WithName("provider"),
			Probe:         probe,
		}); err != nil {
			setupLog.Error(err, "failed to bootstrap provider role")
			os.Exit(1)
		}
	}

	pollerInstance.Run(ctx)
	setupLog.Info("shutdown signal received, exiting")
}

// envOrDefault returns the value of environment variable key, or def
// when key is unset or empty. Used so --namespace can default to the
// downward-API POD_NAMESPACE in-cluster while still working for a bare
// `go run` outside a pod.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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
