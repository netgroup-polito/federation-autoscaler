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

// Package integration is the cross-package end-to-end suite for the
// shared agent core (step 7). It boots an envtest control plane, a
// controller-runtime manager with the four Broker reconcilers, AND the
// real Broker HTTPS server with mTLS — then drives a scenario by
// running the agent's own client + poller against the listener.
//
// The existing internal/integration suite stops short of actual HTTP:
// its scaleup_test simulates the API instruction handler by patching
// CR status directly. This suite goes the rest of the way: every byte
// the agent sends or receives travels through the real mTLS handshake
// and JSON-over-HTTP encode/decode round-trip.
package integration

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	autoscalingcontroller "github.com/netgroup-polito/federation-autoscaler/internal/controller/autoscaling"
	brokercontroller "github.com/netgroup-polito/federation-autoscaler/internal/controller/broker"
)

const (
	suiteNamespace = "default"
	suiteTimeout   = 15 * time.Second
	suiteInterval  = 200 * time.Millisecond
)

var (
	suiteCtx       context.Context
	suiteCancel    context.CancelFunc
	testEnv        *envtest.Environment
	cfg            *rest.Config
	k8sClient      client.Client
	mgrDone        chan error
	bundle         *certBundle
	bundleBuilder_ *bundleBuilder
	brokerListen   string
)

func TestAgentIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Agent Integration Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	suiteCtx, suiteCancel = context.WithCancel(context.TODO())

	By("registering Broker + autoscaling + apiextensions schemes")
	Expect(brokerv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(autoscalingv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(apiextensionsv1.AddToScheme(scheme.Scheme)).To(Succeed())

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
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

	By("installing stub Liqo CRDs so the consumer Peer/Unpeer handlers can create the CRs they need")
	Expect(installLiqoStubCRDs(suiteCtx, k8sClient)).To(Succeed())

	By("generating in-test PKI (CA + server cert + agent client cert)")
	certDir := GinkgoT().TempDir()
	bundleBuilder_, err = newBundleBuilder(certDir)
	Expect(err).NotTo(HaveOccurred())
	bundle, err = bundleBuilder_.issueServer()
	Expect(err).NotTo(HaveOccurred())

	By("picking a free port for the broker https listener")
	brokerListen = pickListener()

	By("starting controller-runtime manager with reconcilers and broker Runnable")
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
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
	Expect((&autoscalingcontroller.VirtualNodeStateReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
		// Faster requeue cadence so the suite can observe transitions
		// well within suiteTimeout (the watch fires immediately on
		// VirtualNode events, but the timer is a safety net we want
		// short for tests).
		RequeueAfter: 500 * time.Millisecond,
	}).SetupWithManager(mgr)).To(Succeed())

	runnable, err := brokerapi.NewRunnable(brokerapi.RunnableOptions{
		BindAddress: brokerListen,
		TLS: brokerapi.TLSConfig{
			CertFile:     bundle.serverCertPath,
			KeyFile:      bundle.serverKeyPath,
			ClientCAFile: bundle.caPath,
		},
		Client:    mgr.GetClient(),
		Namespace: suiteNamespace,
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(mgr.Add(runnable)).To(Succeed())

	mgrDone = make(chan error, 1)
	go func() {
		defer GinkgoRecover()
		mgrDone <- mgr.Start(suiteCtx)
	}()
	Expect(mgr.GetCache().WaitForCacheSync(suiteCtx)).To(BeTrue())

	By("waiting for the broker https listener to accept connections")
	Eventually(func() error {
		c, err := net.DialTimeout("tcp", brokerListen, time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	}, suiteTimeout, suiteInterval).Should(Succeed())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	suiteCancel()
	if mgrDone != nil {
		select {
		case <-mgrDone:
		case <-time.After(15 * time.Second):
		}
	}
	Expect(testEnv.Stop()).To(Succeed())
})

// pickListener returns a free 127.0.0.1 address. The OS may reassign
// the port between Close and the broker's bind, but the window is
// sub-millisecond on Linux and acceptable for unit tests.
func pickListener() string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	addr := l.Addr().String()
	Expect(l.Close()).To(Succeed())
	return addr
}

// envtestBinaryDir mirrors the helper used by the per-package suites: it
// locates kube-apiserver / etcd binaries staged by `make setup-envtest`
// so the tests can run from inside an IDE without manual env vars.
func envtestBinaryDir() string {
	basePath := filepath.Join("..", "..", "..", "bin", "k8s")
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

// brokerURL formats the brokerListen address as an https URL the agent
// client can dial.
func brokerURL() string { return "https://" + brokerListen }
