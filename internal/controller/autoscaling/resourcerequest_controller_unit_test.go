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

package autoscaling

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/agentclient"
)

// --- fake ReservationClient --------------------------------------------------

type fakeReservationClient struct {
	nodeGroups       []brokerapi.NodeGroupView
	appliedPlacement autoscalingv1alpha1.PlacementStrategy
	waiting          string
	getErr           error
	postErr          error
	deleteErr        error

	postCalls   int
	lastPostID  string
	lastPostReq *brokerapi.ReservationRequest
	postIDs     []string
	failOnPost  int // when > 0, the failOnPost-th post fails (1-based)

	deleteCalls  int
	lastDeleteID string
	deleteIDs    []string
}

func (f *fakeReservationClient) GetNodeGroups(context.Context) (*brokerapi.NodeGroupListResponse, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &brokerapi.NodeGroupListResponse{NodeGroups: f.nodeGroups, AppliedPlacement: f.appliedPlacement, Waiting: f.waiting}, nil
}

func (f *fakeReservationClient) PostReservation(_ context.Context, id string, req *brokerapi.ReservationRequest) (*brokerapi.ReservationResponse, error) {
	f.postCalls++
	f.lastPostID, f.lastPostReq = id, req
	f.postIDs = append(f.postIDs, id)
	if f.failOnPost > 0 && f.postCalls == f.failOnPost {
		return nil, errors.New("no free chunk")
	}
	if f.postErr != nil {
		return nil, f.postErr
	}
	return &brokerapi.ReservationResponse{}, nil
}

func (f *fakeReservationClient) DeleteReservation(_ context.Context, id string) (*brokerapi.ReleaseResponse, error) {
	f.deleteCalls++
	f.lastDeleteID = id
	f.deleteIDs = append(f.deleteIDs, id)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &brokerapi.ReleaseResponse{}, nil
}

// --- pure helpers ------------------------------------------------------------

func TestChunksFor(t *testing.T) {
	chunk := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("2"),
		corev1.ResourceMemory: resource.MustParse("4Gi"),
	}
	gpuChunk := corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("2"),
		gpuResourceName:    resource.MustParse("1"),
	}
	cases := []struct {
		name  string
		req   corev1.ResourceList
		chunk corev1.ResourceList
		want  int32
	}{
		{"exact one chunk", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")}, chunk, 1},
		{"cpu rounds up", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3")}, chunk, 2},
		{"memory drives the count", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("10Gi")}, chunk, 3},
		{"zero-valued resource ignored", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), gpuResourceName: resource.MustParse("0")}, chunk, 1},
		{"resource absent from chunk → 0", corev1.ResourceList{gpuResourceName: resource.MustParse("1")}, chunk, 0},
		{"gpu chunk fits gpu request", corev1.ResourceList{gpuResourceName: resource.MustParse("2")}, gpuChunk, 2},
		{"empty request → 0", corev1.ResourceList{}, chunk, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chunksFor(tc.req, tc.chunk); got != tc.want {
				t.Errorf("chunksFor = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPickProviderAndSize(t *testing.T) {
	std := func(id string, max, reserved int32) brokerapi.NodeGroupView {
		return brokerapi.NodeGroupView{
			ID: id, ProviderClusterID: id, Type: brokerv1alpha1.ChunkTypeStandard,
			MaxSize: max, CurrentReserved: reserved,
			ChunkResources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
		}
	}
	req := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3")} // needs 2 chunks

	t.Run("picks the group with the most free capacity that fits", func(t *testing.T) {
		groups := []brokerapi.NodeGroupView{std("a", 3, 2) /* avail 1 */, std("b", 8, 2) /* avail 6 */}
		g, n, ok := pickProviderAndSize(groups, req)
		if !ok || g.ID != "b" || n != 2 {
			t.Fatalf("got (%v, %d, %v), want (b, 2, true)", g, n, ok)
		}
	})

	t.Run("no group with enough room → not ok", func(t *testing.T) {
		groups := []brokerapi.NodeGroupView{std("a", 3, 2)} // avail 1 < needed 2
		if _, _, ok := pickProviderAndSize(groups, req); ok {
			t.Fatal("expected ok=false when nothing fits")
		}
	})

	t.Run("gpu request skips standard groups", func(t *testing.T) {
		groups := []brokerapi.NodeGroupView{std("a", 8, 0)}
		gpuReq := corev1.ResourceList{gpuResourceName: resource.MustParse("1")}
		if _, _, ok := pickProviderAndSize(groups, gpuReq); ok {
			t.Fatal("expected ok=false: no gpu node group available")
		}
	})
}

// --- reconcile: reserve on create, release on delete -------------------------

func newReconcileFixture(t *testing.T, agent *fakeReservationClient, objs ...client.Object) (*ResourceRequestReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := autoscalingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&autoscalingv1alpha1.ResourceRequest{}, &autoscalingv1alpha1.VirtualNodeState{}).
		WithObjects(objs...).
		Build()
	return &ResourceRequestReconciler{Client: cl, Scheme: scheme, Agent: agent}, cl
}

// wantResID is the deterministic reservation id for the fixture below
// (mr-<uid>); centralised so the assertions share one literal.
const wantResID = "mr-abc123"

// wantMigratedResID is the fresh id minted for the first migration (mr-<uid>-m1).
const wantMigratedResID = "mr-abc123-m1"

func TestResourceRequestReserveAndRelease(t *testing.T) {
	ctx := context.Background()
	rr := &autoscalingv1alpha1.ResourceRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "batch-job", Namespace: "default", UID: "abc123"},
		Spec: autoscalingv1alpha1.ResourceRequestSpec{
			Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
		},
	}
	agent := &fakeReservationClient{nodeGroups: []brokerapi.NodeGroupView{{
		ID: "ng-p1", ProviderClusterID: "provider-1", Type: brokerv1alpha1.ChunkTypeStandard,
		MaxSize: 10, CurrentReserved: 0,
		ChunkResources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
	}}}
	r, cl := newReconcileFixture(t, agent, rr)
	key := types.NamespacedName{Namespace: "default", Name: "batch-job"}
	reconcile := func() {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	// 1) adds the finalizer.
	reconcile()
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatalf("get after finalize: %v", err)
	}
	if len(rr.Finalizers) == 0 {
		t.Fatal("finalizer not added")
	}
	if agent.postCalls != 0 {
		t.Fatal("must not reserve before the finalizer is persisted")
	}

	// 2) reserves on the chosen provider, sized to whole chunks.
	reconcile()
	if agent.postCalls != 1 {
		t.Fatalf("postCalls = %d, want 1", agent.postCalls)
	}
	if agent.lastPostID != wantResID {
		t.Errorf("reservation id = %q, want mr-abc123 (deterministic from UID)", agent.lastPostID)
	}
	if agent.lastPostReq.ProviderClusterID != "provider-1" || agent.lastPostReq.ChunkCount != 1 {
		t.Errorf("post req = %+v, want provider-1 / 1 chunk", agent.lastPostReq)
	}
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatalf("get after reserve: %v", err)
	}
	if rr.Status.Phase != autoscalingv1alpha1.ResourceRequestReserved || rr.Status.ReservationID != wantResID {
		t.Errorf("status = %+v, want Reserved / mr-abc123", rr.Status)
	}

	// 3) a re-reconcile is a no-op (idempotent; does not double-reserve).
	reconcile()
	if agent.postCalls != 1 {
		t.Errorf("re-reconcile reserved again: postCalls = %d", agent.postCalls)
	}

	// 4) delete → finalizer releases the reservation, then the object is GC'd.
	if err := cl.Delete(ctx, rr); err != nil {
		t.Fatalf("delete: %v", err)
	}
	reconcile()
	if agent.deleteCalls != 1 || agent.lastDeleteID != wantResID {
		t.Errorf("release: deleteCalls=%d id=%q, want 1 / mr-abc123", agent.deleteCalls, agent.lastDeleteID)
	}
	if err := cl.Get(ctx, key, rr); !apierrors.IsNotFound(err) {
		t.Errorf("expected NotFound after release, got %v", err)
	}
}

func TestResourceRequestPendingWhenNoCapacity(t *testing.T) {
	ctx := context.Background()
	rr := &autoscalingv1alpha1.ResourceRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "waiting", Namespace: "default", UID: "u1"},
		Spec:       autoscalingv1alpha1.ResourceRequestSpec{Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}},
	}
	agent := &fakeReservationClient{nodeGroups: nil} // no providers advertising
	r, cl := newReconcileFixture(t, agent, rr)
	key := types.NamespacedName{Namespace: "default", Name: "waiting"}

	// finalize, then attempt to reserve → stays Pending, requeues, does not fail.
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected a requeue while Pending")
	}
	if agent.postCalls != 0 {
		t.Error("must not reserve when no provider fits")
	}
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatalf("get: %v", err)
	}
	if rr.Status.Phase != autoscalingv1alpha1.ResourceRequestPending {
		t.Errorf("phase = %q, want Pending", rr.Status.Phase)
	}
}

// sixCPUGroups is one provider with 2-CPU chunks and room for 10.
func sixCPUGroups() []brokerapi.NodeGroupView {
	return []brokerapi.NodeGroupView{{
		ID: "ng-p1", ProviderClusterID: "provider-1", Type: brokerv1alpha1.ChunkTypeStandard,
		MaxSize: 10, CurrentReserved: 0,
		ChunkResources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
	}}
}

func cpuRequest(name, uid, cpu string) *autoscalingv1alpha1.ResourceRequest {
	return &autoscalingv1alpha1.ResourceRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(uid)},
		Spec: autoscalingv1alpha1.ResourceRequestSpec{
			Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
		},
	}
}

// A request bigger than one node becomes one single-node reservation per node,
// all on the chosen provider, held and released together.
func TestResourceRequestReservesSeveralNodes(t *testing.T) {
	ctx := context.Background()
	rr := cpuRequest("big", "big1", "6") // 6 CPU against 2-CPU chunks ⇒ 3 nodes
	agent := &fakeReservationClient{nodeGroups: sixCPUGroups()}
	r, cl := newReconcileFixture(t, agent, rr)
	key := types.NamespacedName{Namespace: "default", Name: "big"}

	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key}) // finalize
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	want := []string{"mr-big1", "mr-big1-c1", "mr-big1-c2"}
	if strings.Join(agent.postIDs, ",") != strings.Join(want, ",") {
		t.Fatalf("posted ids = %v, want %v", agent.postIDs, want)
	}
	if agent.lastPostReq.ChunkCount != 1 || agent.lastPostReq.ProviderClusterID != "provider-1" {
		t.Errorf("each reservation must be one node on provider-1, got %+v", agent.lastPostReq)
	}
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != autoscalingv1alpha1.ResourceRequestReserved || rr.Status.ChunkCount != 3 ||
		rr.Status.ReservationID != "mr-big1" || strings.Join(rr.Status.ReservationIDs, ",") != strings.Join(want, ",") {
		t.Errorf("status = %+v, want Reserved with the 3 ids", rr.Status)
	}

	if err := cl.Delete(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if strings.Join(agent.deleteIDs, ",") != strings.Join(want, ",") {
		t.Errorf("released ids = %v, want all of %v", agent.deleteIDs, want)
	}
}

// Over the per-request limit the request stays Pending and says why.
func TestResourceRequestPendingOverNodeLimit(t *testing.T) {
	ctx := context.Background()
	rr := cpuRequest("huge", "huge1", "10") // 5 nodes > 4
	agent := &fakeReservationClient{nodeGroups: sixCPUGroups()}
	r, cl := newReconcileFixture(t, agent, rr)
	key := types.NamespacedName{Namespace: "default", Name: "huge"}

	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if agent.postCalls != 0 {
		t.Errorf("must not reserve over the limit; postCalls = %d", agent.postCalls)
	}
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != autoscalingv1alpha1.ResourceRequestPending || !strings.Contains(rr.Status.Message, "5 nodes") {
		t.Errorf("status = %q %q, want Pending saying 5 nodes", rr.Status.Phase, rr.Status.Message)
	}
}

// All or nothing: when a later node cannot be reserved, the earlier ones are
// released and the request fails (and is retried).
func TestResourceRequestPartialFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	rr := cpuRequest("half", "half1", "6")
	agent := &fakeReservationClient{nodeGroups: sixCPUGroups(), failOnPost: 2}
	r, cl := newReconcileFixture(t, agent, rr)
	key := types.NamespacedName{Namespace: "default", Name: "half"}

	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if strings.Join(agent.deleteIDs, ",") != "mr-half1" {
		t.Errorf("released = %v, want the first node rolled back", agent.deleteIDs)
	}
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != autoscalingv1alpha1.ResourceRequestFailed || rr.Status.ReservationID != "" {
		t.Errorf("status = %+v, want Failed with nothing held", rr.Status)
	}
}

// A multi-node request is Active only when every node is up.
func TestResourceRequestHoldWaitsForEveryNode(t *testing.T) {
	ctx := context.Background()
	rr := cpuRequest("pair", "two1", "4") // 2 nodes
	vns := func(id, node string) *autoscalingv1alpha1.VirtualNodeState {
		return &autoscalingv1alpha1.VirtualNodeState{
			ObjectMeta: metav1.ObjectMeta{
				Name: "vns-" + id, Namespace: "federation-autoscaler-system",
				Labels: map[string]string{virtualNodeReservationLabel: id},
			},
			Spec: autoscalingv1alpha1.VirtualNodeStateSpec{
				ProviderClusterID: "provider-1", ProviderLiqoClusterID: "liqo-p1",
				NodeGroupID: "ng-p1", ReservationID: id,
				Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			},
			Status: autoscalingv1alpha1.VirtualNodeStateStatus{VirtualNodeName: node},
		}
	}
	first := vns("mr-two1", "node-a")
	nodeA := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
	agent := &fakeReservationClient{nodeGroups: sixCPUGroups()}
	r, cl := newReconcileFixture(t, agent, rr, first, nodeA)
	key := types.NamespacedName{Namespace: "default", Name: "pair"}
	for i := range 3 { // finalize, reserve, hold
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != autoscalingv1alpha1.ResourceRequestReserved || !strings.Contains(rr.Status.Message, "1 of 2") {
		t.Fatalf("with one node up: status = %q %q, want Reserved, 1 of 2", rr.Status.Phase, rr.Status.Message)
	}

	if err := cl.Create(ctx, vns("mr-two1-c1", "node-b")); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != autoscalingv1alpha1.ResourceRequestActive {
		t.Errorf("with both nodes up: phase = %q, want Active", rr.Status.Phase)
	}
}

func TestResourceRequestHoldProtectsNode(t *testing.T) {
	ctx := context.Background()
	// UID "p1" ⇒ reservation id "mr-p1" ⇒ VNS "vns-mr-p1".
	rr := &autoscalingv1alpha1.ResourceRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "held", Namespace: "default", UID: "p1"},
		Spec:       autoscalingv1alpha1.ResourceRequestSpec{Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}},
	}
	// The consumer agent has already materialised the virtual node for this
	// reservation (labelled VNS + a v1.Node) by the time we hold it.
	vns := &autoscalingv1alpha1.VirtualNodeState{
		ObjectMeta: metav1.ObjectMeta{
			Name: "vns-mr-p1", Namespace: "federation-autoscaler-system",
			Labels: map[string]string{virtualNodeReservationLabel: "mr-p1"},
		},
		Spec: autoscalingv1alpha1.VirtualNodeStateSpec{
			ProviderClusterID: "provider-1", ProviderLiqoClusterID: "liqo-p1",
			NodeGroupID: "ng-p1", ChunkIndex: 0, ReservationID: "mr-p1",
			Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		},
		Status: autoscalingv1alpha1.VirtualNodeStateStatus{VirtualNodeName: "liqo-node-x"},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "liqo-node-x"}}
	agent := &fakeReservationClient{nodeGroups: []brokerapi.NodeGroupView{{
		ID: "ng-p1", ProviderClusterID: "provider-1", Type: brokerv1alpha1.ChunkTypeStandard,
		MaxSize: 4, CurrentReserved: 0,
		ChunkResources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
	}}}
	r, cl := newReconcileFixture(t, agent, rr, vns, node)
	key := types.NamespacedName{Namespace: "default", Name: "held"}
	for i := range 3 { // finalize, reserve, hold
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	// The node is now protected from CA scale-down.
	var n corev1.Node
	if err := cl.Get(ctx, types.NamespacedName{Name: "liqo-node-x"}, &n); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if n.Annotations[scaleDownDisabledAnnotation] != scaleDownDisabledValue {
		t.Errorf("node not protected: annotations=%v", n.Annotations)
	}
	if n.Labels[autoscalingv1alpha1.HeldByLabel] != "held" {
		t.Errorf("node must be labelled as held by the request: labels=%v", n.Labels)
	}
	// And the request reflects it as Active (held).
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatalf("get rr: %v", err)
	}
	if rr.Status.Phase != autoscalingv1alpha1.ResourceRequestActive {
		t.Errorf("phase = %q, want Active", rr.Status.Phase)
	}
}

func TestResourceRequestReleaseToleratesMissingReservation(t *testing.T) {
	ctx := context.Background()
	rr := &autoscalingv1alpha1.ResourceRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "stale", Namespace: "default", UID: "g1"},
		Spec:       autoscalingv1alpha1.ResourceRequestSpec{Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}},
	}
	// The broker no longer knows this reservation (e.g. it was released before
	// this fix); DeleteReservation returns NotFound.
	agent := &fakeReservationClient{
		nodeGroups: []brokerapi.NodeGroupView{{
			ID: "ng-p1", ProviderClusterID: "provider-1", Type: brokerv1alpha1.ChunkTypeStandard,
			MaxSize: 4, CurrentReserved: 0,
			ChunkResources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		}},
		deleteErr: &agentclient.Error{Category: agentclient.CategoryNotFound},
	}
	r, cl := newReconcileFixture(t, agent, rr)
	key := types.NamespacedName{Namespace: "default", Name: "stale"}

	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key}) // finalize
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key}) // reserve (sets ReservationID)
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatalf("get after reserve: %v", err)
	}
	if err := cl.Delete(ctx, rr); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Release must not get stuck on the NotFound — the finalizer is dropped.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("release should tolerate a missing reservation: %v", err)
	}
	if agent.deleteCalls != 1 {
		t.Errorf("deleteCalls = %d, want 1", agent.deleteCalls)
	}
	if err := cl.Get(ctx, key, rr); !apierrors.IsNotFound(err) {
		t.Errorf("expected the request to be GC'd after release, got %v", err)
	}
}

func TestResourceRequestFailsOnReserveError(t *testing.T) {
	ctx := context.Background()
	rr := &autoscalingv1alpha1.ResourceRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "default", UID: "u2"},
		Spec:       autoscalingv1alpha1.ResourceRequestSpec{Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}},
	}
	agent := &fakeReservationClient{
		nodeGroups: []brokerapi.NodeGroupView{{
			ID: "ng-p1", ProviderClusterID: "provider-1", Type: brokerv1alpha1.ChunkTypeStandard,
			MaxSize: 4, CurrentReserved: 0,
			ChunkResources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		}},
		postErr: errors.New("insufficient capacity"),
	}
	r, cl := newReconcileFixture(t, agent, rr)
	key := types.NamespacedName{Namespace: "default", Name: "bad"}

	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key}) // finalize
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile should surface the failure via status, not an error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected a requeue to retry a failed reservation")
	}
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatalf("get: %v", err)
	}
	if rr.Status.Phase != autoscalingv1alpha1.ResourceRequestFailed {
		t.Errorf("phase = %q, want Failed", rr.Status.Phase)
	}
}

// --- reconcile: periodic re-evaluation + migration (feature 7) ----------------

// reservedRR builds an already-reserved manual request bound to provider-1 with
// reservation id "mr-abc123", whose last transition was `ago` in the past (to
// drive the re-eval debounce).
func reservedRR(ago time.Duration) *autoscalingv1alpha1.ResourceRequest {
	lt := metav1.NewTime(time.Now().Add(-ago))
	return &autoscalingv1alpha1.ResourceRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name: "job", Namespace: "default", UID: types.UID("abc123"),
			Finalizers: []string{resourceRequestFinalizer},
		},
		Spec: autoscalingv1alpha1.ResourceRequestSpec{
			Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
		},
		Status: autoscalingv1alpha1.ResourceRequestStatus{
			Phase:              autoscalingv1alpha1.ResourceRequestActive,
			ReservationID:      wantResID,
			ProviderClusterID:  "provider-1",
			ChunkCount:         1,
			LastTransitionTime: &lt,
		},
	}
}

// growable is a node group the masking left growable (the policy winner).
func growable(provider string) brokerapi.NodeGroupView {
	return brokerapi.NodeGroupView{
		ID: "ng-" + provider, ProviderClusterID: provider, Type: brokerv1alpha1.ChunkTypeStandard,
		MaxSize: 10, CurrentReserved: 0,
		ChunkResources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
	}
}

// metered is a node group carrying an explicit placement metric (lower = better),
// as the Broker now stamps on every view. maxSize/reserved set its head-room, so a
// full provider (avail 0) can still carry its true metric — the input the re-eval
// guard uses to avoid a spurious same-provider migration.
func metered(provider string, maxSize, reserved int32, metric float64) brokerapi.NodeGroupView {
	g := growable(provider)
	g.MaxSize, g.CurrentReserved = maxSize, reserved
	g.PlacementMetric, g.HasMetric = metric, true
	return g
}

// TestReEvalNoMigrationWhenCurrentFullButStillBest is the regression for the
// self-occupancy confound: the current provider is masked as non-growable only
// because THIS reservation filled it, so the Broker exposes the next-best provider
// as the growable survivor. The old guard ("a different provider is growable")
// migrated there and back every interval; the metric guard must recognise the
// current provider is still the best (lower metric) and NOT migrate.
func TestReEvalNoMigrationWhenCurrentFullButStillBest(t *testing.T) {
	ctx := context.Background()
	rr := reservedRR(2 * time.Hour) // current = provider-1
	agent := &fakeReservationClient{
		nodeGroups: []brokerapi.NodeGroupView{
			metered("provider-1", 1, 1, 1.0),  // current: FULL (avail 0) but the lowest metric
			metered("provider-2", 10, 0, 2.0), // spill target: growable but a WORSE metric
		},
		appliedPlacement: autoscalingv1alpha1.PlacementStrategyPrice,
	}
	r, cl := newReconcileFixture(t, agent, rr)
	r.ReEvalInterval = time.Hour
	key := types.NamespacedName{Namespace: "default", Name: "job"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if agent.deleteCalls != 0 {
		t.Fatalf("must not migrate off a full-but-still-best provider; delete=%d", agent.deleteCalls)
	}
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.ReservationID != wantResID || rr.Status.MigrationCount != 0 {
		t.Fatalf("reservation must be untouched: id=%q count=%d", rr.Status.ReservationID, rr.Status.MigrationCount)
	}
}

// TestReEvalMigratesWhenCandidateStrictlyBetter confirms the guard still lets a
// GENUINE improvement through: the current provider is present but its metric got
// worse, so a strictly-better provider must trigger the break-before-make.
func TestReEvalMigratesWhenCandidateStrictlyBetter(t *testing.T) {
	ctx := context.Background()
	rr := reservedRR(2 * time.Hour) // current = provider-1
	agent := &fakeReservationClient{
		nodeGroups: []brokerapi.NodeGroupView{
			metered("provider-1", 1, 1, 5.0),  // current: now the WORSE metric
			metered("provider-2", 10, 0, 1.0), // growable AND strictly better
		},
		appliedPlacement: autoscalingv1alpha1.PlacementStrategyPrice,
	}
	r, cl := newReconcileFixture(t, agent, rr)
	r.ReEvalInterval = time.Hour
	key := types.NamespacedName{Namespace: "default", Name: "job"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if agent.deleteCalls != 1 || agent.lastDeleteID != wantResID {
		t.Fatalf("a strictly-better provider must start a migration; delete=%d id=%q", agent.deleteCalls, agent.lastDeleteID)
	}
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != autoscalingv1alpha1.ResourceRequestMigrating ||
		rr.Status.ReservationID != wantMigratedResID || rr.Status.MigrationCount != 1 {
		t.Fatalf("want Migrating with fresh id mr-abc123-m1 (count 1); got %+v", rr.Status)
	}
}

func TestReEvalMigratesToBetterProvider(t *testing.T) {
	ctx := context.Background()
	rr := reservedRR(2 * time.Hour)
	agent := &fakeReservationClient{
		nodeGroups:       []brokerapi.NodeGroupView{growable("provider-2")}, // broker masked to p2
		appliedPlacement: autoscalingv1alpha1.PlacementStrategyLatency,
	}
	r, cl := newReconcileFixture(t, agent, rr)
	r.ReEvalInterval = time.Hour
	key := types.NamespacedName{Namespace: "default", Name: "job"}

	// Step 1: release the old reservation and enter Migrating with a fresh id.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("step 1: %v", err)
	}
	if agent.deleteCalls != 1 || agent.lastDeleteID != wantResID {
		t.Fatalf("step 1 must release the old reservation; delete=%d id=%q", agent.deleteCalls, agent.lastDeleteID)
	}
	if agent.postCalls != 0 {
		t.Fatal("step 1 must NOT create the new reservation yet (break-before-make)")
	}
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != autoscalingv1alpha1.ResourceRequestMigrating {
		t.Fatalf("phase = %q, want Migrating", rr.Status.Phase)
	}
	if rr.Status.ReservationID != wantMigratedResID || rr.Status.MigrationCount != 1 {
		t.Fatalf("fresh id/count wrong: id=%q count=%d", rr.Status.ReservationID, rr.Status.MigrationCount)
	}

	// Step 2: peer the new provider under the fresh id.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("step 2: %v", err)
	}
	if agent.postCalls != 1 || agent.lastPostID != wantMigratedResID {
		t.Fatalf("step 2 must create the new reservation; post=%d id=%q", agent.postCalls, agent.lastPostID)
	}
	if agent.lastPostReq.ProviderClusterID != "provider-2" {
		t.Fatalf("new reservation provider = %q, want provider-2", agent.lastPostReq.ProviderClusterID)
	}
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.ProviderClusterID != "provider-2" {
		t.Fatalf("provider after migration = %q, want provider-2", rr.Status.ProviderClusterID)
	}
	if rr.Status.Phase == autoscalingv1alpha1.ResourceRequestMigrating {
		t.Fatal("should have left Migrating after step 2")
	}
}

func TestReEvalSkipsStandardPolicy(t *testing.T) {
	ctx := context.Background()
	rr := reservedRR(2 * time.Hour)
	agent := &fakeReservationClient{
		nodeGroups:       []brokerapi.NodeGroupView{growable("provider-2")}, // p2 is "better"...
		appliedPlacement: autoscalingv1alpha1.PlacementStrategyStandard,     // ...but Standard never migrates
	}
	r, cl := newReconcileFixture(t, agent, rr)
	r.ReEvalInterval = time.Hour
	key := types.NamespacedName{Namespace: "default", Name: "job"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if agent.deleteCalls != 0 {
		t.Fatalf("Standard policy must not migrate; delete=%d", agent.deleteCalls)
	}
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.ReservationID != wantResID || rr.Status.MigrationCount != 0 {
		t.Fatalf("reservation must be untouched: id=%q count=%d", rr.Status.ReservationID, rr.Status.MigrationCount)
	}
}

func TestReEvalDebouncedWithinInterval(t *testing.T) {
	ctx := context.Background()
	rr := reservedRR(time.Minute) // transitioned recently
	agent := &fakeReservationClient{
		nodeGroups:       []brokerapi.NodeGroupView{growable("provider-2")},
		appliedPlacement: autoscalingv1alpha1.PlacementStrategyLatency,
	}
	r, _ := newReconcileFixture(t, agent, rr)
	r.ReEvalInterval = time.Hour // 1m < 1h → debounced
	key := types.NamespacedName{Namespace: "default", Name: "job"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if agent.deleteCalls != 0 {
		t.Fatalf("debounce must block a migration within the interval; delete=%d", agent.deleteCalls)
	}
}

func TestReEvalNoOpWhenCurrentIsBest(t *testing.T) {
	ctx := context.Background()
	rr := reservedRR(2 * time.Hour)
	agent := &fakeReservationClient{
		nodeGroups:       []brokerapi.NodeGroupView{growable("provider-1")}, // current is still the winner
		appliedPlacement: autoscalingv1alpha1.PlacementStrategyLatency,
	}
	r, _ := newReconcileFixture(t, agent, rr)
	r.ReEvalInterval = time.Hour
	key := types.NamespacedName{Namespace: "default", Name: "job"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if agent.deleteCalls != 0 {
		t.Fatalf("no migration when the current provider is still best; delete=%d", agent.deleteCalls)
	}
}

func TestReEvalDisabledByZeroInterval(t *testing.T) {
	ctx := context.Background()
	rr := reservedRR(2 * time.Hour)
	agent := &fakeReservationClient{
		nodeGroups:       []brokerapi.NodeGroupView{growable("provider-2")},
		appliedPlacement: autoscalingv1alpha1.PlacementStrategyLatency,
	}
	r, _ := newReconcileFixture(t, agent, rr) // ReEvalInterval defaults to 0 (disabled)
	key := types.NamespacedName{Namespace: "default", Name: "job"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if agent.deleteCalls != 0 {
		t.Fatalf("re-eval disabled (interval 0) must not migrate; delete=%d", agent.deleteCalls)
	}
}

// --- per-request latency bound -----------------------------------------------

// boundedFakeClient is a fakeReservationClient that also accepts a latency
// bound, like the real agent client, and records what it was asked.
type boundedFakeClient struct {
	fakeReservationClient
	withinCalls int
	lastBound   *autoscalingv1alpha1.LatencyBound
}

func (f *boundedFakeClient) GetNodeGroupsWithin(
	ctx context.Context, bound *autoscalingv1alpha1.LatencyBound,
) (*brokerapi.NodeGroupListResponse, error) {
	f.withinCalls++
	f.lastBound = bound
	return f.GetNodeGroups(ctx)
}

func TestNodeGroupsForPassesRequestBound(t *testing.T) {
	bound := &autoscalingv1alpha1.LatencyBound{MaxLatencyMs: 3}
	withBound := &autoscalingv1alpha1.ResourceRequest{Spec: autoscalingv1alpha1.ResourceRequestSpec{LatencyBound: bound}}
	noBound := &autoscalingv1alpha1.ResourceRequest{}

	agent := &boundedFakeClient{}
	r := &ResourceRequestReconciler{Agent: agent}
	if _, err := r.nodeGroupsFor(context.Background(), withBound); err != nil {
		t.Fatal(err)
	}
	if agent.withinCalls != 1 || agent.lastBound != bound {
		t.Errorf("a request's bound must be passed to the agent: calls=%d bound=%+v", agent.withinCalls, agent.lastBound)
	}
	if _, err := r.nodeGroupsFor(context.Background(), noBound); err != nil {
		t.Fatal(err)
	}
	if agent.withinCalls != 1 {
		t.Errorf("a request without a bound must use the plain call, within calls = %d", agent.withinCalls)
	}

	// A client that cannot take a bound still works (unbounded view).
	plain := &ResourceRequestReconciler{Agent: &fakeReservationClient{}}
	if _, err := plain.nodeGroupsFor(context.Background(), withBound); err != nil {
		t.Errorf("a plain client must fall back to GetNodeGroups: %v", err)
	}
}

// --- per-request plain-language text -----------------------------------------

// queryFakeClient also accepts the whole per-request query, like the real agent
// client, and records the last one.
type queryFakeClient struct {
	fakeReservationClient
	queryCalls int
	lastQuery  localapi.NodeGroupQuery
}

func (f *queryFakeClient) GetNodeGroupsFor(
	ctx context.Context, q localapi.NodeGroupQuery,
) (*brokerapi.NodeGroupListResponse, error) {
	f.queryCalls++
	f.lastQuery = q
	return f.GetNodeGroups(ctx)
}

func TestNodeGroupsForPassesPromptSizeAndBound(t *testing.T) {
	bound := &autoscalingv1alpha1.LatencyBound{MaxLatencyMs: 3}
	rr := &autoscalingv1alpha1.ResourceRequest{Spec: autoscalingv1alpha1.ResourceRequestSpec{
		Resources:    corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		LatencyBound: bound,
		UserPrompt:   "I want the clusters closest to me.",
	}}
	agent := &queryFakeClient{}
	r := &ResourceRequestReconciler{Agent: agent}
	if _, err := r.nodeGroupsFor(context.Background(), rr); err != nil {
		t.Fatal(err)
	}
	q := agent.lastQuery
	if agent.queryCalls != 1 || q.Prompt != rr.Spec.UserPrompt || q.Bound != bound {
		t.Fatalf("query = %+v (calls %d), want the request's text and bound", q, agent.queryCalls)
	}
	if cpu := q.Need[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("query need = %v, want the request's resources", q.Need)
	}
}

func TestReEvalSkippedForRequestWithOwnText(t *testing.T) {
	ctx := context.Background()
	rr := reservedRR(2 * time.Hour)
	rr.Spec.UserPrompt = "closest to me"
	agent := &fakeReservationClient{
		nodeGroups:       []brokerapi.NodeGroupView{growable("provider-2")},
		appliedPlacement: autoscalingv1alpha1.PlacementStrategyLatency,
	}
	r, _ := newReconcileFixture(t, agent, rr)
	r.ReEvalInterval = time.Hour
	key := types.NamespacedName{Namespace: "default", Name: "job"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if agent.deleteCalls != 0 {
		t.Fatalf("a request placed by the AI for its own text must not migrate; delete=%d", agent.deleteCalls)
	}
}

// While the agent's AI is still choosing, the request says so (not "no
// capacity") and retries within seconds.
func TestResourceRequestWaitingWhileTheAIChooses(t *testing.T) {
	ctx := context.Background()
	rr := &autoscalingv1alpha1.ResourceRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "asking", Namespace: "default", UID: "u1"},
		Spec: autoscalingv1alpha1.ResourceRequestSpec{
			Resources:  corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			UserPrompt: "closest to me",
		},
	}
	agent := &fakeReservationClient{waiting: brokerapi.WaitingAIChoosing} // everything masked meanwhile
	r, cl := newReconcileFixture(t, agent, rr)
	key := types.NamespacedName{Namespace: "default", Name: "asking"}

	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key}) // finalizer
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != waitingRequeue {
		t.Errorf("requeue = %v, want the quick %v", res.RequeueAfter, waitingRequeue)
	}
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatalf("get: %v", err)
	}
	if rr.Status.Phase != autoscalingv1alpha1.ResourceRequestPending || rr.Status.Message != brokerapi.WaitingAIChoosing {
		t.Errorf("status = %q %q, want Pending with %q", rr.Status.Phase, rr.Status.Message, brokerapi.WaitingAIChoosing)
	}
}

// With a latency limit and no provider within it, the message says so.
func TestResourceRequestPendingMentionsTheLatencyLimit(t *testing.T) {
	ctx := context.Background()
	rr := cpuRequest("near", "near1", "2")
	rr.Spec.LatencyBound = &autoscalingv1alpha1.LatencyBound{MaxLatencyMs: 1}
	agent := &fakeReservationClient{nodeGroups: nil}
	r, cl := newReconcileFixture(t, agent, rr)
	key := types.NamespacedName{Namespace: "default", Name: "near"}
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rr.Status.Message, "within 1 ms") {
		t.Errorf("message = %q, want it to mention the 1 ms limit", rr.Status.Message)
	}
}
