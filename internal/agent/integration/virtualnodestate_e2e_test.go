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
	"encoding/json"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/instructions"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver"
	gAgentClient "github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/agentclient"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/protos"
)

// liqoVirtualNodeGVK is duplicated here (rather than imported from the
// reconciler) to keep this test self-contained and immune to the
// reconciler's package changing its exported name.
var liqoVirtualNodeGVK = schema.GroupVersionKind{
	Group:   "offloading.liqo.io",
	Version: "v1beta1",
	Kind:    "VirtualNode",
}

var _ = Describe("Step 11 end-to-end: Peer → VirtualNodeState → reconciler → localapi → gRPC server", func() {
	const (
		providerCluster = "provider-11f"
		consumerCluster = "consumer-11f"
		reservationID   = "res-11f"
		liqoVNName      = "rs-res-11f"
		nodeGroupID     = "ng-provider-11f-standard"
	)

	AfterEach(func() {
		// Best-effort cleanup so consecutive runs of the suite don't
		// trip over leftovers; IsNotFound on each delete is harmless.
		_ = k8sClient.Delete(suiteCtx, &autoscalingv1alpha1.VirtualNodeState{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vns-" + reservationID,
				Namespace: suiteNamespace,
			},
		})
		_ = k8sClient.Delete(suiteCtx, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: liqoVNName},
		})
		vn := &unstructured.Unstructured{}
		vn.SetGroupVersionKind(liqoVirtualNodeGVK)
		vn.SetName(liqoVNName)
		vn.SetNamespace(suiteNamespace)
		_ = k8sClient.Delete(suiteCtx, vn)
		_ = k8sClient.Delete(suiteCtx, &brokerv1alpha1.ClusterAdvertisement{
			ObjectMeta: metav1.ObjectMeta{Name: providerCluster, Namespace: suiteNamespace},
		})
	})

	It("projects Liqo VirtualNode state through to the gRPC server", func() {
		By("seeding a ClusterAdvertisement so the broker advertises the matching node group")
		// PricingNodePrice resolves the price via /local/nodegroups —
		// without an advertisement the gRPC server's findNodeGroup
		// would return NotFound mid-chain, hiding the projection bug
		// we actually want to catch.
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
			if err := k8sClient.Get(suiteCtx,
				types.NamespacedName{Name: providerCluster, Namespace: suiteNamespace}, c); err != nil {
				return err
			}
			now := metav1.Now()
			c.Status.Available = true
			c.Status.LastSeen = &now
			c.Status.TotalChunks = 2
			c.Status.AvailableChunks = 2
			return k8sClient.Status().Update(suiteCtx, c)
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("running the consumer Peer handler with a stubbed liqoctl — produces the VirtualNodeState CR")
		peerHandler := instructions.NewPeerHandler(instructions.PeerConfig{
			LocalClient: k8sClient,
			Namespace:   suiteNamespace,
			Run: func(_ context.Context, _ string, _ ...string) ([]byte, []byte, error) {
				return nil, nil, nil // pretend liqoctl peer succeeded
			},
		})
		peerView := &brokerapi.InstructionView{
			ID:                    "peer-" + reservationID,
			Kind:                  string(autoscalingv1alpha1.ReservationInstructionPeer),
			ReservationID:         reservationID,
			ProviderClusterID:     providerCluster,
			ProviderLiqoClusterID: "liqo-" + providerCluster,
			Kubeconfig:            "apiVersion: v1\nkind: Config",
			ResourceSliceResources: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			ChunkCount: 1,
		}
		res, err := peerHandler(suiteCtx, peerView)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Status).To(Equal(brokerapi.ResultStatusSucceeded))

		By("verifying the VirtualNodeState CR has the expected linkage")
		vnsKey := types.NamespacedName{Name: "vns-" + reservationID, Namespace: suiteNamespace}
		var vns autoscalingv1alpha1.VirtualNodeState
		Expect(k8sClient.Get(suiteCtx, vnsKey, &vns)).To(Succeed())
		Expect(vns.Spec.NodeGroupID).To(Equal(nodeGroupID))
		Expect(vns.Spec.ProviderClusterID).To(Equal(providerCluster))
		Expect(vns.Spec.ReservationID).To(Equal(reservationID))

		By("simulating Liqo materialising the VirtualNode + v1.Node")
		// Liqo VirtualNode carries the reservation label so the
		// reconciler's watch map-func enqueues the right VNS even
		// before Status.VirtualNodeName has been cached.
		vn := &unstructured.Unstructured{}
		vn.SetGroupVersionKind(liqoVirtualNodeGVK)
		vn.SetName(liqoVNName)
		vn.SetNamespace(suiteNamespace)
		vn.SetLabels(map[string]string{
			"federation-autoscaler.io/reservation": reservationID,
		})
		_ = unstructured.SetNestedField(vn.Object, map[string]interface{}{
			"clusterID": "liqo-" + providerCluster,
		}, "spec")
		Expect(k8sClient.Create(suiteCtx, vn)).To(Succeed())
		// Status is a subresource — patch separately so the apiserver
		// accepts it.
		Expect(k8sClient.Get(suiteCtx, types.NamespacedName{Name: liqoVNName, Namespace: suiteNamespace}, vn)).To(Succeed())
		_ = unstructured.SetNestedSlice(vn.Object, []interface{}{
			map[string]interface{}{"type": "Node", "status": "Running"},
		}, "status", "conditions")
		Expect(k8sClient.Status().Update(suiteCtx, vn)).To(Succeed())

		// The v1.Node Liqo creates is what carries the authoritative
		// Allocatable the reconciler projects into Status.Allocatable.
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: liqoVNName},
		}
		Expect(k8sClient.Create(suiteCtx, node)).To(Succeed())
		node.Status.Allocatable = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("7800Mi"),
		}
		Expect(k8sClient.Status().Update(suiteCtx, node)).To(Succeed())

		By("waiting for the reconciler to project Liqo state onto VirtualNodeState.Status")
		Eventually(func(g Gomega) {
			var got autoscalingv1alpha1.VirtualNodeState
			g.Expect(k8sClient.Get(suiteCtx, vnsKey, &got)).To(Succeed())
			g.Expect(got.Status.Phase).To(Equal(autoscalingv1alpha1.VirtualNodeStatePhaseRunning))
			g.Expect(got.Status.VirtualNodeName).To(Equal(liqoVNName))
			g.Expect(got.Status.Allocatable).To(HaveKeyWithValue(corev1.ResourceCPU, resource.MustParse("4")))
			g.Expect(got.Status.Conditions).To(ContainElement(SatisfyAll(
				HaveField("Type", autoscalingv1alpha1.VirtualNodeStateConditionReady),
				HaveField("Status", metav1.ConditionTrue),
			)))
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("building a broker client (only needed to satisfy localapi.New; the routes we touch never call it)")
		consumerCert, err := bundleBuilder_.issueAgentCert(consumerCluster, 800)
		Expect(err).NotTo(HaveOccurred())
		brokerClient, err := agentclient.New(agentclient.Options{
			BrokerURL: brokerURL(),
			TLS: agentclient.TLSConfig{
				CertFile:     consumerCert.certPath,
				KeyFile:      consumerCert.keyPath,
				BrokerCAFile: bundle.caPath,
				ServerName:   serverNameFromAddr(brokerListen),
			},
			RequestTimeout: 5 * time.Second,
		})
		Expect(err).NotTo(HaveOccurred())

		By("mounting the consumer localapi on an httptest server")
		localServer, err := localapi.New(localapi.Options{
			BindAddress: "127.0.0.1:0",
			Client:      brokerClient,
			LocalClient: k8sClient,
			Namespace:   suiteNamespace,
		})
		Expect(err).NotTo(HaveOccurred())
		localTS := httptest.NewServer(localServer.Handler())
		defer localTS.Close()

		By("starting the gRPC server with mTLS, pointed at the localapi httptest URL")
		grpcAgent, err := gAgentClient.New(gAgentClient.Options{BaseURL: localTS.URL})
		Expect(err).NotTo(HaveOccurred())
		grpcCertDir := stageGrpcCertDir()
		grpcBind := pickListener()
		gs, err := grpcserver.New(grpcserver.Options{
			BindAddress: grpcBind,
			TLS: grpcserver.TLSConfig{
				CertDir:  grpcCertDir,
				CertName: "tls.crt",
				KeyName:  "tls.key",
				CAName:   "ca.crt",
			},
			AgentClient:     grpcAgent,
			ShutdownTimeout: time.Second,
		})
		Expect(err).NotTo(HaveOccurred())
		grpcCtx, grpcCancel := context.WithCancel(suiteCtx)
		grpcDone := make(chan error, 1)
		go func() { grpcDone <- gs.Run(grpcCtx) }()
		defer func() {
			grpcCancel()
			Eventually(grpcDone, 3*time.Second, 50*time.Millisecond).Should(Receive())
		}()
		Eventually(func() bool { return gs.Addr() != nil }, time.Second, 10*time.Millisecond).Should(BeTrue())

		By("dialling the gRPC server as Cluster Autoscaler would")
		conn, err := grpc.NewClient(gs.Addr().String(),
			grpc.WithTransportCredentials(credentials.NewTLS(grpcClientTLS("ca-11f", 700))))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = conn.Close() }()
		client := protos.NewCloudProviderClient(conn)

		ctx, cancel := context.WithTimeout(suiteCtx, 10*time.Second)
		defer cancel()

		By("NodeGroupTargetSize reflects the materialised chunk")
		Eventually(func(g Gomega) {
			resp, err := client.NodeGroupTargetSize(ctx, &protos.NodeGroupTargetSizeRequest{Id: nodeGroupID})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(resp.TargetSize).To(Equal(int32(1)))
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("NodeGroupForNode resolves the Liqo VirtualNode back to its group")
		resForNode, err := client.NodeGroupForNode(ctx, &protos.NodeGroupForNodeRequest{
			Node: &protos.ExternalGrpcNode{Name: liqoVNName},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(resForNode.NodeGroup.Id).To(Equal(nodeGroupID))

		By("NodeGroupNodes lists the chunk with Status=Running")
		resNodes, err := client.NodeGroupNodes(ctx, &protos.NodeGroupNodesRequest{Id: nodeGroupID})
		Expect(err).NotTo(HaveOccurred())
		Expect(resNodes.Instances).To(HaveLen(1))
		Expect(resNodes.Instances[0].Id).To(Equal(liqoVNName))
		Expect(resNodes.Instances[0].Status.InstanceState).To(Equal(protos.InstanceStatus_instanceRunning))

		By("waiting for the broker to advertise the matching node group through /local/nodegroups")
		Eventually(func(g Gomega) {
			ng, err := client.NodeGroups(ctx, &protos.NodeGroupsRequest{})
			g.Expect(err).NotTo(HaveOccurred())
			ids := make([]string, 0, len(ng.NodeGroups))
			for _, x := range ng.NodeGroups {
				ids = append(ids, x.Id)
			}
			g.Expect(ids).To(ContainElement(nodeGroupID))
		}, suiteTimeout, suiteInterval).Should(Succeed())

		By("PricingNodePrice succeeds end-to-end (no cost advertised → 0)")
		resPrice, err := client.PricingNodePrice(ctx, &protos.PricingNodePriceRequest{
			Node:           &protos.ExternalGrpcNode{Name: liqoVNName},
			StartTimestamp: timestamppb.Now(),
			EndTimestamp:   timestamppb.New(time.Now().Add(time.Hour)),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(resPrice.Price).To(Equal(float64(0)))

		By("the localapi /local/virtual-nodes surface contains the full projection")
		resp, err := localTS.Client().Get(localTS.URL + "/local/virtual-nodes")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		var list localapi.VirtualNodeListResponse
		Expect(json.NewDecoder(resp.Body).Decode(&list)).To(Succeed())
		// Other specs in this suite (e.g. the consumer-9f happy-path) may
		// have left their own VirtualNodeState CRs in the shared envtest
		// namespace, so filter to the one this spec created instead of
		// asserting on the whole list length.
		var v *localapi.VirtualNodeView
		for i := range list.VirtualNodes {
			if list.VirtualNodes[i].ReservationID == reservationID {
				v = &list.VirtualNodes[i]
				break
			}
		}
		Expect(v).NotTo(BeNil(), "no VirtualNodeView for reservation %q in %+v",
			reservationID, list.VirtualNodes)
		Expect(v.Name).To(Equal(liqoVNName))
		Expect(v.NodeGroupID).To(Equal(nodeGroupID))
		Expect(v.Phase).To(Equal(autoscalingv1alpha1.VirtualNodeStatePhaseRunning))
		Expect(v.Allocatable).To(HaveKeyWithValue(corev1.ResourceCPU, resource.MustParse("4")))
	})
})
