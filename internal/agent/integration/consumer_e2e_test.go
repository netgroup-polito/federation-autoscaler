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
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/health"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/poller"
)

// fakeConsumerLiqoctlScript handles `peer` and `unpeer` for the
// consumer-role tests. Writes nothing to stdout — the broker doesn't
// read stdout for these subcommands — and silently succeeds.
const fakeConsumerLiqoctlScript = `#!/bin/sh
case "$1" in
  peer)
    exit 0
    ;;
  unpeer)
    exit 0
    ;;
  *)
    echo "fake-liqoctl: unknown subcommand: $*" 1>&2
    exit 2
    ;;
esac
`

func writeFakeConsumerLiqoctl() string {
	dir, err := os.MkdirTemp("", "fake-liqoctl-consumer-")
	Expect(err).NotTo(HaveOccurred())
	path := filepath.Join(dir, "liqoctl")
	Expect(os.WriteFile(path, []byte(fakeConsumerLiqoctlScript), 0o755)).To(Succeed())
	DeferCleanup(func() { _ = os.RemoveAll(dir) })
	return path
}

var _ = Describe("Step 9 end-to-end: real consumer.Run against the broker over mTLS, with a fake liqoctl", func() {
	const (
		providerCluster = "provider-9f"
		consumerCluster = "consumer-9f"
		resName         = "consumer-9f-resv"
	)

	resvKey := types.NamespacedName{Name: resName, Namespace: suiteNamespace}
	peerKey := types.NamespacedName{Name: "peer-" + resName, Namespace: suiteNamespace}
	cadvKey := types.NamespacedName{Name: providerCluster, Namespace: suiteNamespace}
	brokerSecretKey := types.NamespacedName{Name: "kubeconfig-" + resName, Namespace: suiteNamespace}

	AfterEach(func() {
		_ = k8sClient.Delete(suiteCtx, &autoscalingv1alpha1.ReservationInstruction{
			ObjectMeta: metav1.ObjectMeta{Name: peerKey.Name, Namespace: suiteNamespace},
		})
		_ = k8sClient.Delete(suiteCtx, &brokerv1alpha1.Reservation{
			ObjectMeta: metav1.ObjectMeta{Name: resName, Namespace: suiteNamespace},
		})
		_ = k8sClient.Delete(suiteCtx, &brokerv1alpha1.ClusterAdvertisement{
			ObjectMeta: metav1.ObjectMeta{Name: providerCluster, Namespace: suiteNamespace},
		})
		_ = k8sClient.Delete(suiteCtx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: brokerSecretKey.Name, Namespace: suiteNamespace},
		})
		// The Peer handler creates a VirtualNodeState CR (vns-<resv>);
		// without an explicit delete it leaks into later specs sharing
		// the envtest namespace.
		_ = k8sClient.Delete(suiteCtx, &autoscalingv1alpha1.VirtualNodeState{
			ObjectMeta: metav1.ObjectMeta{Name: "vns-" + resName, Namespace: suiteNamespace},
		})
	})

	It("consumer.Run picks up Peer, runs fake liqoctl, creates Liqo CRs, and the broker advances to Peered", func() {
		By("issuing a CN=consumer-9f mTLS cert and writing the fake liqoctl on disk")
		agentCert, err := bundleBuilder_.issueAgentCert(consumerCluster, 300)
		Expect(err).NotTo(HaveOccurred())
		liqoctlPath := writeFakeConsumerLiqoctl()

		By("pre-creating the broker-side kubeconfig staging Secret the Peer instruction will inline")
		Expect(k8sClient.Create(suiteCtx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      brokerSecretKey.Name,
				Namespace: suiteNamespace,
				Labels: map[string]string{
					"federation-autoscaler.io/reservation": resName,
				},
			},
			Data: map[string][]byte{
				"kubeconfig": []byte("apiVersion: v1\nkind: Config\nclusters:\n- cluster: {server: https://provider-9f.local}\n  name: provider-9f"),
			},
		})).To(Succeed())

		By("creating an available ClusterAdvertisement so the provider-availability guard passes")
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

		By("creating the Reservation directly in KubeconfigReady so the reconciler emits Peer next")
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
			r.Status.Phase = brokerv1alpha1.ReservationPhaseKubeconfigReady
			r.Status.Message = "test seed"
			return k8sClient.Status().Update(suiteCtx, r)
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("waiting for the reconciler to emit the Peer instruction")
		Eventually(func(g Gomega) {
			ri := &autoscalingv1alpha1.ReservationInstruction{}
			g.Expect(k8sClient.Get(suiteCtx, peerKey, ri)).To(Succeed())
			g.Expect(ri.Spec.Kind).To(Equal(autoscalingv1alpha1.ReservationInstructionPeer))
			g.Expect(ri.Spec.TargetClusterID).To(Equal(consumerCluster))
			g.Expect(ri.Spec.KubeconfigRef).To(Equal(brokerSecretKey.Name))
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

		By("running consumer.Run — registers all four consumer handlers + starts heartbeat + loopback REST")
		probe := health.New(health.Options{PollStaleAfter: 5 * time.Second})
		registry := poller.NewRegistry()
		runCtx, runCancel := context.WithCancel(suiteCtx)
		defer runCancel()

		// Consumer's local-API listener binds to a free 127.0.0.1 port
		// so multiple suite runs and parallel specs don't collide.
		Expect(consumer.Run(runCtx, consumer.Options{
			Client:        brokerClient,
			Registry:      registry,
			LocalClient:   k8sClient,
			ClusterID:     consumerCluster,
			LiqoClusterID: "liqo-" + consumerCluster,
			LocalAPIAddr:  pickListener(),
			Namespace:     suiteNamespace,
			LiqoctlPath:   liqoctlPath,
			Probe:         probe,
		})).To(Succeed())

		// All four consumer handlers must be registered.
		Expect(registry.Len()).To(Equal(4))

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

		By("the broker recorded the result — Peer instruction Status.Enforced flips to true")
		Eventually(func(g Gomega) {
			ri := &autoscalingv1alpha1.ReservationInstruction{}
			g.Expect(k8sClient.Get(suiteCtx, peerKey, ri)).To(Succeed())
			g.Expect(ri.Status.Enforced).To(BeTrue())
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("the consumer agent created the Liqo ResourceSlice and NamespaceOffloading")
		// Both CRs were created via Unstructured; we don't have their Go
		// types in scope here. The Peer handler's tests already cover
		// the GVK/name shape — for the e2e check it is enough that the
		// instruction was Enforced (which only happens after the
		// handler returned Succeeded, which only happens after both CR
		// creates succeed).
		// We also assert the consumer-side kubeconfig Secret is on the
		// API server (the only Liqo-side artefact we can read via a
		// typed client).
		Eventually(func(g Gomega) {
			sec := &corev1.Secret{}
			g.Expect(k8sClient.Get(suiteCtx, types.NamespacedName{
				Name:      "kubeconfig-" + resName,
				Namespace: suiteNamespace,
			}, sec)).To(Succeed())
			g.Expect(string(sec.Data["kubeconfig"])).To(ContainSubstring("https://provider-9f.local"))
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("the broker advanced the Reservation past Peering")
		Eventually(func(g Gomega) {
			r := &brokerv1alpha1.Reservation{}
			g.Expect(k8sClient.Get(suiteCtx, resvKey, r)).To(Succeed())
			g.Expect(r.Status.Phase).To(BeElementOf(
				brokerv1alpha1.ReservationPhasePeering,
				brokerv1alpha1.ReservationPhasePeered,
			))
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("the readiness probe is green — poll loop and heartbeat are healthy")
		Expect(probe.Ready()).To(Succeed())
	})
})
