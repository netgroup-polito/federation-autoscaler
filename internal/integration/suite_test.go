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

// Package integration owns the cross-package envtest suites that wire the
// Broker reconcilers up to a real controller-runtime manager and let them
// react to one another via watches. Each *_test.go file in this package
// drives a single end-to-end scenario (e.g. scale-up happy path) by
// creating CRs and then asserting the chain of reconciler-emitted side
// effects with Eventually.
//
// API HTTP handler behaviour is *simulated* here: the test code patches
// instruction status and Reservation phase the same way the real handler
// would. That keeps the surface narrow — the HTTP layer already has its
// own integration suite under internal/broker/api.
package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	autoscalingcontroller "github.com/netgroup-polito/federation-autoscaler/internal/controller/autoscaling"
	brokercontroller "github.com/netgroup-polito/federation-autoscaler/internal/controller/broker"
)

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	mgrDone   chan error
)

func TestIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Integration Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	By("registering Broker + autoscaling schemes")
	Expect(brokerv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(autoscalingv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := envtestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	By("starting controller-runtime manager with all four reconcilers")
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		// Leader election is irrelevant in-process; disable it to keep
		// startup deterministic and avoid Lease-create chatter.
		LeaderElection: false,
	})
	Expect(err).NotTo(HaveOccurred())

	Expect((&brokercontroller.ClusterAdvertisementReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)).To(Succeed())
	Expect((&brokercontroller.ReservationReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)).To(Succeed())
	Expect((&autoscalingcontroller.ProviderInstructionReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)).To(Succeed())
	Expect((&autoscalingcontroller.ReservationInstructionReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)).To(Succeed())

	mgrDone = make(chan error, 1)
	go func() {
		defer GinkgoRecover()
		mgrDone <- mgr.Start(ctx)
	}()

	// Wait for the manager's cache to populate before any spec runs.
	Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	if mgrDone != nil {
		select {
		case <-mgrDone:
		case <-time.After(15 * time.Second):
			// Unblock if Start somehow hangs; envtest.Stop will still run.
		}
	}
	Expect(testEnv.Stop()).To(Succeed())
})

// envtestBinaryDir mirrors the helper used by the per-package suites: it
// locates kube-apiserver / etcd binaries staged by `make setup-envtest`
// so the tests can run from inside an IDE without manual env vars.
func envtestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
