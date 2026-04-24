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

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	autoscalingcontroller "github.com/netgroup-polito/federation-autoscaler/internal/controller/autoscaling"
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

	if err := (&autoscalingcontroller.VirtualNodeStateReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VirtualNodeState")
		os.Exit(1)
	}

	setupLog.Info("gRPC externalgrpc server configured",
		"grpcBindAddress", grpcBindAddress,
		"agentLocalAPIURL", agentLocalAPIURL,
		"TODO", "start externalgrpc server + wire Consumer Agent client")

	setupLog.Info("starting grpc-server manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
