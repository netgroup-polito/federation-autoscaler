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

package integration

import (
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/health"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/poller"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/provider"
)

// fakeLiqoctlScript is a tiny POSIX shell script that mimics the
// subset of `liqoctl` the provider handlers shell out to. It writes
// a recognisable kubeconfig YAML on stdout for `generate peering-user`
// and silently succeeds for `delete peering-user`. Anything else
// returns non-zero so a typo in the handler args fails loudly.
const fakeLiqoctlScript = `#!/bin/sh
case "$1 $2" in
  "generate peering-user")
    cat <<EOF
apiVersion: v1
kind: Config
clusters:
- cluster: {server: https://provider.local}
  name: provider
EOF
    ;;
  "delete peering-user")
    exit 0
    ;;
  *)
    echo "fake-liqoctl: unknown subcommand: $*" 1>&2
    exit 2
    ;;
esac
`

// writeFakeLiqoctl writes the fake script to a unique temp file under
// the suite's certificate directory and chmods +x so os/exec can spawn
// it directly.
func writeFakeLiqoctl() string {
	dir, err := os.MkdirTemp("", "fake-liqoctl-")
	Expect(err).NotTo(HaveOccurred())
	path := filepath.Join(dir, "liqoctl")
	Expect(os.WriteFile(path, []byte(fakeLiqoctlScript), 0o755)).To(Succeed())
	DeferCleanup(func() { _ = os.RemoveAll(dir) })
	return path
}

var _ = Describe("Step 8 end-to-end: real provider.Run against the broker over mTLS, with a fake liqoctl", func() {
	const (
		providerCluster = "provider-8f"
		consumerCluster = "consumer-8f"
		resName         = "provider-8f-resv"
	)

	resvKey := types.NamespacedName{Name: resName, Namespace: suiteNamespace}
	gkKey := types.NamespacedName{Name: "gk-" + resName, Namespace: suiteNamespace}
	cadvKey := types.NamespacedName{Name: providerCluster, Namespace: suiteNamespace}

	AfterEach(func() {
		_ = k8sClient.Delete(suiteCtx, &autoscalingv1alpha1.ProviderInstruction{
			ObjectMeta: metav1.ObjectMeta{Name: gkKey.Name, Namespace: suiteNamespace},
		})
		_ = k8sClient.Delete(suiteCtx, &brokerv1alpha1.Reservation{
			ObjectMeta: metav1.ObjectMeta{Name: resName, Namespace: suiteNamespace},
		})
		_ = k8sClient.Delete(suiteCtx, &brokerv1alpha1.ClusterAdvertisement{
			ObjectMeta: metav1.ObjectMeta{Name: providerCluster, Namespace: suiteNamespace},
		})
	})

	It("provider.Run picks up GenerateKubeconfig, executes the fake liqoctl, and the broker advances the Reservation", func() {
		By("issuing a provider-8f mTLS cert and writing the fake liqoctl on disk")
		agentCert, err := bundleBuilder_.issueAgentCert(providerCluster, 200)
		Expect(err).NotTo(HaveOccurred())
		liqoctlPath := writeFakeLiqoctl()

		By("creating an available ClusterAdvertisement so the reservation guard passes")
		cadv := &brokerv1alpha1.ClusterAdvertisement{
			ObjectMeta: metav1.ObjectMeta{Name: providerCluster, Namespace: suiteNamespace},
			Spec: brokerv1alpha1.ClusterAdvertisementSpec{
				ClusterID:     providerCluster,
				LiqoClusterID: "liqo-" + providerCluster,
				ClusterType:   brokerv1alpha1.ChunkTypeStandard,
				Resources: brokerv1alpha1.AdvertisedResources{
					Allocatable: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("8"),
						corev1.ResourceMemory: resource.MustParse("16Gi"),
					},
				},
			},
		}
		Expect(k8sClient.Create(suiteCtx, cadv)).To(Succeed())
		Eventually(func() error {
			c := &brokerv1alpha1.ClusterAdvertisement{}
			if err := k8sClient.Get(suiteCtx, cadvKey, c); err != nil {
				return err
			}
			now := metav1.Now()
			c.Status.Available = true
			c.Status.LastSeen = &now
			return k8sClient.Status().Update(suiteCtx, c)
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("creating a Reservation in Pending so the reconciler emits GenerateKubeconfig")
		resv := &brokerv1alpha1.Reservation{
			ObjectMeta: metav1.ObjectMeta{Name: resName, Namespace: suiteNamespace},
			Spec: brokerv1alpha1.ReservationSpec{
				ConsumerClusterID:     consumerCluster,
				ConsumerLiqoClusterID: "liqo-" + consumerCluster,
				ProviderClusterID:     providerCluster,
				ProviderLiqoClusterID: "liqo-" + providerCluster,
				ChunkCount:            1,
				ChunkType:             brokerv1alpha1.ChunkTypeStandard,
				Resources: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
			},
		}
		Expect(k8sClient.Create(suiteCtx, resv)).To(Succeed())
		Eventually(func() error {
			r := &brokerv1alpha1.Reservation{}
			if err := k8sClient.Get(suiteCtx, resvKey, r); err != nil {
				return err
			}
			r.Status.Phase = brokerv1alpha1.ReservationPhasePending
			return k8sClient.Status().Update(suiteCtx, r)
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("waiting for the reconciler to emit the GenerateKubeconfig instruction")
		Eventually(func() error {
			pi := &autoscalingv1alpha1.ProviderInstruction{}
			return k8sClient.Get(suiteCtx, gkKey, pi)
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("building the agent's broker client over real mTLS")
		brokerClient, err := agentclient.New(agentclient.Options{
			BrokerURL: brokerURL(),
			TLS: agentclient.TLSConfig{
				CertFile:     agentCert.certPath,
				KeyFile:      agentCert.keyPath,
				BrokerCAFile: bundle.caPath,
				ServerName:   serverNameFromAddr(brokerListen),
			},
			RequestTimeout: 5 * time.Second,
			MaxRetries:     2,
			InitialBackoff: 50 * time.Millisecond,
			MaxBackoff:     200 * time.Millisecond,
		})
		Expect(err).NotTo(HaveOccurred())

		By("running provider.Run — registers all three provider handlers + starts publisher")
		probe := health.New(health.Options{PollStaleAfter: 5 * time.Second})
		registry := poller.NewRegistry()
		runCtx, runCancel := context.WithCancel(suiteCtx)
		defer runCancel()
		Expect(provider.Run(runCtx, provider.Options{
			Client:        brokerClient,
			Registry:      registry,
			LocalClient:   k8sClient,
			ClusterID:     providerCluster,
			LiqoClusterID: "liqo-" + providerCluster,
			LiqoctlPath:   liqoctlPath,
			Probe:         probe,
		})).To(Succeed())

		// All three provider handlers must be registered.
		Expect(registry.Len()).To(Equal(3))

		// Wire the poller (cmd/agent/main.go does this in production).
		p, err := poller.New(poller.Options{
			Client:       brokerClient,
			Registry:     registry,
			Interval:     200 * time.Millisecond,
			OnPollResult: probe.RecordPoll,
		})
		Expect(err).NotTo(HaveOccurred())

		pollerDone := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			p.Run(runCtx)
			close(pollerDone)
		}()
		defer func() {
			runCancel()
			Eventually(pollerDone, time.Second, 50*time.Millisecond).Should(BeClosed())
		}()

		By("the broker recorded the result — instruction Status.Enforced flips to true")
		Eventually(func(g Gomega) {
			pi := &autoscalingv1alpha1.ProviderInstruction{}
			g.Expect(k8sClient.Get(suiteCtx, gkKey, pi)).To(Succeed())
			g.Expect(pi.Status.Enforced).To(BeTrue())
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("the broker advanced the Reservation past GeneratingKubeconfig")
		Eventually(func(g Gomega) {
			r := &brokerv1alpha1.Reservation{}
			g.Expect(k8sClient.Get(suiteCtx, resvKey, r)).To(Succeed())
			g.Expect(r.Status.Phase).To(BeElementOf(
				brokerv1alpha1.ReservationPhaseKubeconfigReady,
				brokerv1alpha1.ReservationPhasePeering,
				brokerv1alpha1.ReservationPhasePeered,
			))
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("the broker stored the fake kubeconfig from the fake liqoctl in the staging Secret")
		Eventually(func(g Gomega) {
			sec := &corev1.Secret{}
			g.Expect(k8sClient.Get(suiteCtx, types.NamespacedName{
				Name: "kubeconfig-" + resName, Namespace: suiteNamespace,
			}, sec)).To(Succeed())
			g.Expect(string(sec.Data["kubeconfig"])).To(ContainSubstring("https://provider.local"))
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("the readiness probe is green — the poll loop is healthy")
		Expect(probe.Ready()).To(Succeed())
	})
})
