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

// envtest-backed Ginkgo suite for the Broker REST API package. Plain
// `func TestX(t *testing.T)` tests in the same package (chunk_test.go,
// registry_test.go, tls_test.go, middleware_test.go) run independently of
// this suite — only the spec runs that exercise real CRDs need envtest.

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// Suite-wide handles. Set in BeforeSuite, cleared in AfterSuite.
var (
	suiteCtx    context.Context
	suiteCancel context.CancelFunc
	testEnv     *envtest.Environment
	cfg         *rest.Config
	k8sClient   client.Client

	// testNamespace is created fresh in BeforeSuite so each suite run gets
	// a clean slate without colliding with other test packages.
	testNamespace = "broker-api-it"

	// httpServer is the in-process httptest server fronting the Broker
	// API. Tests construct requests against httpServer.URL.
	httpServer *httptest.Server

	// authClusterID is the cluster ID injected on every request via the
	// test middleware. Tests reset it per case to simulate different
	// callers without spinning up multiple servers.
	authClusterID = ""
)

// TestAPISuite is the Ginkgo entry point. Plain Go test functions
// elsewhere in this package run separately.
func TestAPISuite(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Broker API Integration Suite")
}

// BeforeSuite boots envtest, registers schemes, creates the test namespace,
// and starts an httptest server wrapping Server.Handler() with a fake-auth
// middleware that injects authClusterID into the request context.
var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	suiteCtx, suiteCancel = context.WithCancel(context.TODO())

	Expect(brokerv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(autoscalingv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := firstFoundEnvTestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	Expect(k8sClient.Create(suiteCtx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: testNamespace},
	})).To(Succeed())

	srv := NewServer(ServerOptions{
		Client:    k8sClient,
		Namespace: testNamespace,
	})
	httpServer = httptest.NewServer(testAuthMiddleware(srv.Handler()))
})

var _ = AfterSuite(func() {
	if httpServer != nil {
		httpServer.Close()
	}
	By("tearing down the test environment")
	suiteCancel()
	Expect(testEnv.Stop()).To(Succeed())
})

// testAuthMiddleware short-circuits the real ClusterIDMiddleware: it copies
// the suite-level authClusterID variable into the request context. We do
// not exercise mTLS here because middleware_test.go covers that path with
// httptest already; this lets every spec drive the handler with a single
// line.
func testAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authClusterID != "" {
			r = r.WithContext(NewContextWithClusterID(r.Context(), authClusterID))
		}
		next.ServeHTTP(w, r)
	})
}

// firstFoundEnvTestBinaryDir mirrors the helper in the controller suites
// so envtest assets are picked up automatically when run from the IDE
// (i.e. without going through the make target).
func firstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(basePath, e.Name())
		}
	}
	return ""
}
