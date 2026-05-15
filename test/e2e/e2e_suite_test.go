//go:build e2e
// +build e2e

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

// Package e2e is the multi-cluster end-to-end suite (step 13). It boots
// the four-Kind topology defined under ../kind, runs the per-cluster
// bootstrap helpers from ../bootstrap (cert-manager + Liqo + CRDs +
// federation-autoscaler overlays + Cluster Autoscaler), and drives the
// happy-path scenarios from ../scenario.
//
// The suite is gated by the `e2e` build tag so regular `go test ./...`
// never tries to provision four Kind clusters; `make test-e2e` is the
// only entry point.
//
// Environment knobs:
//
//	KEEP_CLUSTERS=true   — skip teardown so the developer can poke at
//	                       the clusters after a failure
//	SKIP_IMAGE_BUILD=true — skip `make docker-build`; useful when
//	                       iterating against pre-built images
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/netgroup-polito/federation-autoscaler/test/e2e/bootstrap"
	"github.com/netgroup-polito/federation-autoscaler/test/e2e/kind"
)

const (
	// suiteSetupTimeout caps the entire BeforeSuite. 20 m absorbs Kind
	// bootstrap (4 clusters × ~30 s), cert-manager + Liqo installs (~1 m
	// each), image loads, and overlay applies on a CI host.
	suiteSetupTimeout = 20 * time.Minute

	// suiteTeardownTimeout caps AfterSuite — mostly four `kind delete
	// cluster` calls.
	suiteTeardownTimeout = 5 * time.Minute
)

var (
	suiteCtx    context.Context
	suiteCancel context.CancelFunc
	topology    *kind.Topology
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintln(GinkgoWriter, "starting federation-autoscaler multi-cluster e2e suite")
	RunSpecs(t, "federation-autoscaler e2e (4-cluster Kind)")
}

var _ = BeforeSuite(func() {
	suiteCtx, suiteCancel = context.WithTimeout(context.Background(), suiteSetupTimeout)

	if os.Getenv("SKIP_IMAGE_BUILD") != "true" {
		By("building component images via `make docker-build`")
		cmd := exec.CommandContext(suiteCtx, "make", "docker-build")
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(),
			"make docker-build failed: %s", string(out))
	}

	By("provisioning the four-Kind topology")
	var err error
	topology, err = kind.Bootstrap(suiteCtx, kind.BootstrapOptions{})
	Expect(err).NotTo(HaveOccurred())

	for _, role := range topology.Roles() {
		By(fmt.Sprintf("[%s] installing cert-manager + federation-autoscaler CRDs", role))
		Expect(bootstrap.InstallCertManager(suiteCtx, bootstrap.CertManagerOptions{
			Kubeconfig: topology.Kubeconfig(role),
		})).To(Succeed())
		Expect(bootstrap.InstallCRDs(suiteCtx, bootstrap.CRDOptions{
			Kubeconfig: topology.Kubeconfig(role),
		})).To(Succeed())
	}

	for _, role := range topology.Roles() {
		By(fmt.Sprintf("[%s] installing Liqo", role))
		Expect(bootstrap.InstallLiqoForRole(suiteCtx, role, topology.Kubeconfig(role))).To(Succeed())
	}

	for _, role := range topology.Roles() {
		images := bootstrap.ImagesForRole(role)
		for _, img := range images {
			By(fmt.Sprintf("[%s] loading image %s into Kind", role, img))
			Expect(bootstrap.LoadImage(suiteCtx, bootstrap.LoadImageOptions{
				ClusterName: kind.ClusterName(role),
				Image:       img,
			})).To(Succeed())
		}
	}

	for _, role := range topology.Roles() {
		for _, overlay := range bootstrap.OverlayPathForRole(role) {
			By(fmt.Sprintf("[%s] applying overlay %s", role, overlay))
			Expect(bootstrap.ApplyOverlay(suiteCtx, bootstrap.ApplyOverlayOptions{
				Kubeconfig:  topology.Kubeconfig(role),
				OverlayPath: overlay,
			})).To(Succeed())
		}
	}

	By("[central] pinning the broker Service to a NodePort so agents on other clusters can reach it")
	Expect(bootstrap.PinBrokerServiceNodePort(suiteCtx,
		topology.Kubeconfig(kind.RoleCentral))).To(Succeed())

	for _, role := range []kind.Role{kind.RoleConsumer1, kind.RoleProvider1, kind.RoleProvider2} {
		By(fmt.Sprintf("[%s] stamping agent-config with cluster identity", role))
		Expect(bootstrap.PatchAgentConfig(suiteCtx, bootstrap.AgentConfigOptions{
			Kubeconfig:      topology.Kubeconfig(role),
			Identity:        bootstrap.IdentityForRole(role),
			CertificateName: "agent-client",
		})).To(Succeed())
	}

	By("[consumer-1] deploying Cluster Autoscaler with --cloud-provider=externalgrpc")
	Expect(bootstrap.DeployClusterAutoscaler(suiteCtx, bootstrap.ClusterAutoscalerOptions{
		Kubeconfig: topology.Kubeconfig(kind.RoleConsumer1),
	})).To(Succeed())
}, suiteSetupTimeout.Seconds())

var _ = AfterSuite(func() {
	suiteCancel()
	if os.Getenv("KEEP_CLUSTERS") == "true" {
		_, _ = fmt.Fprintln(GinkgoWriter, "KEEP_CLUSTERS=true — leaving Kind clusters in place")
		return
	}
	tdCtx, cancel := context.WithTimeout(context.Background(), suiteTeardownTimeout)
	defer cancel()
	if topology != nil {
		Expect(topology.Teardown(tdCtx)).To(Succeed())
	}
})
