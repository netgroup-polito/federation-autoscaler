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
	"testing"

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
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/grpcserver/agentclient"
)

// --- fake ReservationClient --------------------------------------------------

type fakeReservationClient struct {
	nodeGroups []brokerapi.NodeGroupView
	getErr     error
	postErr    error
	deleteErr  error

	postCalls   int
	lastPostID  string
	lastPostReq *brokerapi.ReservationRequest

	deleteCalls  int
	lastDeleteID string
}

func (f *fakeReservationClient) GetNodeGroups(context.Context) (*brokerapi.NodeGroupListResponse, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &brokerapi.NodeGroupListResponse{NodeGroups: f.nodeGroups}, nil
}

func (f *fakeReservationClient) PostReservation(_ context.Context, id string, req *brokerapi.ReservationRequest) (*brokerapi.ReservationResponse, error) {
	f.postCalls++
	f.lastPostID, f.lastPostReq = id, req
	if f.postErr != nil {
		return nil, f.postErr
	}
	return &brokerapi.ReservationResponse{}, nil
}

func (f *fakeReservationClient) DeleteReservation(_ context.Context, id string) (*brokerapi.ReleaseResponse, error) {
	f.deleteCalls++
	f.lastDeleteID = id
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

func TestResourceRequestReserveAndRelease(t *testing.T) {
	ctx := context.Background()
	rr := &autoscalingv1alpha1.ResourceRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "batch-job", Namespace: "default", UID: "abc123"},
		Spec: autoscalingv1alpha1.ResourceRequestSpec{
			Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("6Gi")},
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
	if agent.lastPostReq.ProviderClusterID != "provider-1" || agent.lastPostReq.ChunkCount != 2 {
		t.Errorf("post req = %+v, want provider-1 / 2 chunks", agent.lastPostReq)
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
	for i := 0; i < 3; i++ { // finalize, reserve, hold
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
