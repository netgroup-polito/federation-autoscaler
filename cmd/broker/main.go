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

// Command broker is the federation-autoscaler Broker running on the central
// cluster. It reconciles the four CRDs the Broker owns:
//
//   - broker.federation-autoscaler.io/ClusterAdvertisement
//   - broker.federation-autoscaler.io/Reservation
//   - autoscaling.federation-autoscaler.io/ProviderInstruction
//   - autoscaling.federation-autoscaler.io/ReservationInstruction
//
// It does NOT watch VirtualNodeState — that CRD is consumed only by the
// gRPC server running on the consumer cluster. See docs/design.md §4.1.
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
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	autoscalingcontroller "github.com/netgroup-polito/federation-autoscaler/internal/controller/autoscaling"
	brokercontroller "github.com/netgroup-polito/federation-autoscaler/internal/controller/broker"
	fedmanager "github.com/netgroup-polito/federation-autoscaler/internal/manager"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(brokerv1alpha1.AddToScheme(scheme))
	utilruntime.Must(autoscalingv1alpha1.AddToScheme(scheme))
}

func main() {
	cfg := fedmanager.BindFlags(flag.CommandLine)
	zapOpts := zap.Options{Development: true}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	mgr, err := fedmanager.New(cfg, fedmanager.Options{
		Scheme:           scheme,
		LeaderElectionID: "broker.federation-autoscaler.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&brokercontroller.ClusterAdvertisementReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClusterAdvertisement")
		os.Exit(1)
	}
	if err := (&brokercontroller.ReservationReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Reservation")
		os.Exit(1)
	}
	if err := (&autoscalingcontroller.ProviderInstructionReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ProviderInstruction")
		os.Exit(1)
	}
	if err := (&autoscalingcontroller.ReservationInstructionReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ReservationInstruction")
		os.Exit(1)
	}

	setupLog.Info("starting broker manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
