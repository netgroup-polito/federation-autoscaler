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

// Command grpc-server is the federation-autoscaler gRPC server running on the
// consumer cluster next to Cluster Autoscaler. It implements the
// externalgrpc.CloudProvider interface (see
// cluster-autoscaler/cloudprovider/externalgrpc/protos/externalgrpc.proto) so
// CA can scale virtual nodes that Liqo has peered in from remote providers.
//
// Responsibilities (see docs/design.md §4.2):
//   - Own the autoscaling.federation-autoscaler.io/VirtualNodeState CRD on
//     the consumer cluster; one object per virtual-node chunk.
//   - Translate CA gRPC calls into local REST calls to the Consumer Agent
//     (it never talks to the Broker directly).
//   - Present node groups and node templates back to CA, built from the
//     advertised-resources snapshot it fetches through the Consumer Agent.
package main

import (
	"flag"
	"os"
	"time"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	autoscalingcontroller "github.com/netgroup-polito/federation-autoscaler/internal/controller/autoscaling"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/agentclient"
	fedmanager "github.com/netgroup-polito/federation-autoscaler/internal/manager"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(autoscalingv1alpha1.AddToScheme(scheme))
}

func main() {
	cfg := fedmanager.BindFlags(flag.CommandLine)
	zapOpts := zap.Options{Development: true}
	zapOpts.BindFlags(flag.CommandLine)

	var (
		grpcBindAddress                                     string
		grpcCertPath, grpcCertName, grpcKeyName, grpcCAName string
		agentLocalAPIURL                                    string
		reEvalInterval                                      time.Duration
	)
	// externalgrpc listener used by Cluster Autoscaler (see CA's
	// --cloud-provider=externalgrpc flag and --cloud-config pointing to it).
	flag.StringVar(&grpcBindAddress, "grpc-bind-address", ":8443",
		"Address the externalgrpc server binds to; must match CA's --cloud-config URL.")
	flag.StringVar(&grpcCertPath, "grpc-cert-path", "",
		"Directory that holds the externalgrpc server certificate and CA bundle.")
	flag.StringVar(&grpcCertName, "grpc-cert-name", "tls.crt",
		"File name (under --grpc-cert-path) of the externalgrpc server certificate.")
	flag.StringVar(&grpcKeyName, "grpc-key-name", "tls.key",
		"File name (under --grpc-cert-path) of the externalgrpc server key.")
	flag.StringVar(&grpcCAName, "grpc-ca-name", "ca.crt",
		"File name (under --grpc-cert-path) of the CA bundle that signs CA's client cert.")
	flag.StringVar(&agentLocalAPIURL, "agent-local-api-url", "http://127.0.0.1:9090",
		"Base URL of the co-located Consumer Agent's loopback REST API.")
	flag.DurationVar(&reEvalInterval, "re-eval-interval", time.Hour,
		"How often an active MANUAL reservation is re-evaluated for a better provider "+
			"(and the minimum gap between two migrations of the same reservation). Only "+
			"stable-metric policies (Price/Eco/Latency) migrate; Standard never does. "+
			"0 disables re-evaluation. Lower it for a live demo.")

	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	mgr, err := fedmanager.New(cfg, fedmanager.Options{
		Scheme:           scheme,
		LeaderElectionID: "grpc-server.federation-autoscaler.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Build the typed agent client (step 10b) so the gRPC RPCs — and the
	// ResourceRequest controller below — can proxy to the co-located Consumer
	// Agent's loopback REST.
	agent, err := agentclient.New(agentclient.Options{
		BaseURL: agentLocalAPIURL,
		Logger:  ctrl.Log.WithName("grpc-agentclient"),
	})
	if err != nil {
		setupLog.Error(err, "unable to build agent client")
		os.Exit(1)
	}

	if err := (&autoscalingcontroller.VirtualNodeStateReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VirtualNodeState")
		os.Exit(1)
	}

	// The ResourceRequest controller turns user-created manual reservations into
	// the same broker reservation + Liqo peering the autoscaler path drives, via
	// the agent loopback (feature: manual reservations).
	if err := (&autoscalingcontroller.ResourceRequestReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		Agent:          agent,
		ReEvalInterval: reEvalInterval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ResourceRequest")
		os.Exit(1)
	}

	// Build the externalgrpc server. Read-mostly RPCs land in 10c;
	// mutating + lifecycle RPCs in 10d/10e; everything else remains
	// Unimplemented for now.
	gs, err := grpcserver.New(grpcserver.Options{
		BindAddress: grpcBindAddress,
		TLS: grpcserver.TLSConfig{
			CertDir:  grpcCertPath,
			CertName: grpcCertName,
			KeyName:  grpcKeyName,
			CAName:   grpcCAName,
		},
		AgentClient: agent,
		Logger:      ctrl.Log.WithName("grpcserver"),
	})
	if err != nil {
		setupLog.Error(err, "unable to build externalgrpc server")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	setupLog.Info("gRPC externalgrpc server configured",
		"grpcBindAddress", grpcBindAddress,
		"agentLocalAPIURL", agentLocalAPIURL)

	go func() {
		if err := gs.Run(ctx); err != nil {
			setupLog.Error(err, "externalgrpc server stopped with error")
			os.Exit(1)
		}
	}()

	setupLog.Info("starting grpc-server manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
