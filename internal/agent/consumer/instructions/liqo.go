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

package instructions

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// Liqo GroupVersionKind constants. We model the CRs via
// unstructured.Unstructured rather than pulling in the full liqo Go
// module — the agent only ever creates and deletes these objects, so
// the Liqo webhook validates the Spec on the way in. If Liqo changes
// schemas across versions, this is the single place to update.
var (
	resourceSliceGVK = schema.GroupVersionKind{
		Group:   "authentication.liqo.io",
		Version: "v1beta1",
		Kind:    "ResourceSlice",
	}

	// foreignClusterGVK identifies the cluster-scoped object Liqo uses to
	// represent a peering relationship. `liqoctl unpeer` disables the
	// networking/auth/offloading modules and deletes the control-plane
	// Identity + Tenant, but it leaves the ForeignCluster object behind as
	// a dangling shell (it never deletes it). That shell is what keeps
	// `liqoctl info` reporting an "Active peering" with Authentication
	// Healthy after scale-down, so the consumer agent deletes it as the
	// final unpeer step. The object is named after the provider's Liqo
	// cluster ID.
	foreignClusterGVK = schema.GroupVersionKind{
		Group:   "core.liqo.io",
		Version: "v1beta1",
		Kind:    "ForeignCluster",
	}
)

// Liqo's own labels/annotation on a locally-originated ResourceSlice. Without
// ALL of these the object is inert: the crdReplicator never ships it to the
// provider and Liqo's VirtualNodeCreatorReconciler skips it (it requires the
// remote-cluster-id label and the create-virtual-node annotation), so no
// VirtualNode and no v1.Node are ever produced. Mirrors
// liqo/pkg/liqo-controller-manager/authentication/forge.MutateResourceSlice;
// values are liqo/pkg/consts.{ReplicationRequestedLabel,
// ReplicationDestinationLabel,RemoteClusterID,CreateVirtualNodeAnnotation}.
const (
	liqoReplicationLabel      = "liqo.io/replication"
	liqoReplicationLabelValue = "true"
	liqoRemoteIDLabel         = "liqo.io/remoteID"
	liqoRemoteClusterIDLabel  = "liqo.io/remote-cluster-id"
	liqoCreateVirtualNodeAnn  = "liqo.io/create-virtual-node"

	// liqoTenantNamespaceLabel marks a Liqo tenant namespace; combined with
	// liqoRemoteClusterIDLabel it identifies the one for a given provider.
	liqoTenantNamespaceLabel = "liqo.io/tenant-namespace"

	// liqoTenantNamespacePrefix is the conventional tenant-namespace name
	// ("liqo-tenant-<clusterID>"), used only as a fallback when the label
	// lookup finds nothing.
	liqoTenantNamespacePrefix = "liqo-tenant-"
)

// resourceSliceName returns the deterministic ResourceSlice name for a
// reservation. Deterministic naming means re-issuing the same Peer
// instruction is a no-op (AlreadyExists → success).
//
// It is also, by construction, the name of the node: Liqo propagates
// ResourceSlice.Name → VirtualNode.Name → v1.Node.Name unchanged. Naming the
// slice per RESERVATION (rather than per provider, as `liqoctl peer` does) is
// what allows one provider to yield more than one borrowed node.
func resourceSliceName(reservationID string) string {
	return "rs-" + reservationID
}

// tenantNamespaceFor resolves the Liqo tenant namespace for a provider — the
// namespace a locally-originated ResourceSlice must live in to be replicated.
// It matches Liqo's own lookup (tenantnamespace.Manager.GetNamespace): select on
// remote-cluster-id AND the tenant-namespace marker. Falls back to the
// conventional "liqo-tenant-<id>" name when the lookup finds nothing, so a
// cluster whose namespace predates the labels still works.
func tenantNamespaceFor(
	ctx context.Context,
	c ctrlclient.Client,
	providerLiqoClusterID string,
) (string, error) {
	var list corev1.NamespaceList
	if err := c.List(ctx, &list,
		ctrlclient.MatchingLabels{liqoRemoteClusterIDLabel: providerLiqoClusterID},
		ctrlclient.HasLabels{liqoTenantNamespaceLabel},
	); err != nil {
		return "", fmt.Errorf("list tenant namespaces for %q: %w", providerLiqoClusterID, err)
	}
	switch len(list.Items) {
	case 0:
		return liqoTenantNamespacePrefix + providerLiqoClusterID, nil
	case 1:
		return list.Items[0].Name, nil
	default:
		// Liqo treats this as a hard error too — picking one at random would
		// put the slice somewhere non-deterministic.
		return "", fmt.Errorf("multiple tenant namespaces found for provider %q", providerLiqoClusterID)
	}
}

// ensureResourceSlice creates a Liqo ResourceSlice claiming `resources`
// from the provider identified by `providerLiqoClusterID`. Idempotent:
// returns nil on AlreadyExists. Returns the created (or pre-existing)
// object's name.
func ensureResourceSlice(
	ctx context.Context,
	c ctrlclient.Client,
	reservationID, providerLiqoClusterID string,
	resources corev1.ResourceList,
) (string, error) {
	namespace, err := tenantNamespaceFor(ctx, c, providerLiqoClusterID)
	if err != nil {
		return "", err
	}

	name := resourceSliceName(reservationID)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(resourceSliceGVK)
	obj.SetName(name)
	obj.SetNamespace(namespace)
	// The liqo.io labels/annotation are what make this object DO anything —
	// see the const block above. The federation-autoscaler label is ours, for
	// correlating the slice back to its reservation.
	obj.SetLabels(map[string]string{
		"federation-autoscaler.io/reservation": reservationID,
		liqoReplicationLabel:                   liqoReplicationLabelValue,
		liqoRemoteIDLabel:                      providerLiqoClusterID,
		liqoRemoteClusterIDLabel:               providerLiqoClusterID,
	})
	obj.SetAnnotations(map[string]string{
		liqoCreateVirtualNodeAnn: "true",
	})

	// `resources` is ONE chunk's worth (Reservation.Spec.Resources is
	// per-chunk, and a Reservation now carries exactly one chunk). Leaving it
	// unset would grant the provider's full allocatable, making the borrowed
	// node the whole provider instead of one chunk.
	spec := map[string]interface{}{
		"providerClusterID": providerLiqoClusterID,
		"class":             "default",
		"resources":         resourceListToInterface(resources),
	}
	_ = unstructured.SetNestedField(obj.Object, spec, "spec")

	if err := c.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return name, nil
		}
		return "", fmt.Errorf("create ResourceSlice %q in %q: %w", name, namespace, err)
	}
	return name, nil
}

// deleteResourceSlice removes the ResourceSlice for a reservation from the
// provider's tenant namespace (the same one ensureResourceSlice resolved).
// Deleting it is what takes the borrowed node away. Idempotent on missing.
func deleteResourceSlice(
	ctx context.Context,
	c ctrlclient.Client,
	reservationID, providerLiqoClusterID string,
) error {
	namespace, err := tenantNamespaceFor(ctx, c, providerLiqoClusterID)
	if err != nil {
		return err
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(resourceSliceGVK)
	obj.SetName(resourceSliceName(reservationID))
	obj.SetNamespace(namespace)
	if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ResourceSlice: %w", err)
	}
	return nil
}

// deleteForeignCluster removes the cluster-scoped ForeignCluster shell
// Liqo leaves behind after `liqoctl unpeer` (see foreignClusterGVK).
// Named after the provider's Liqo cluster ID; idempotent on missing.
// Safe to call only once the unpeer has succeeded — at that point the
// networking/auth/offloading modules and the Identity/Tenant are already
// gone, so the ForeignCluster is an inert record.
func deleteForeignCluster(
	ctx context.Context,
	c ctrlclient.Client,
	providerLiqoClusterID string,
) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(foreignClusterGVK)
	obj.SetName(providerLiqoClusterID)
	if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ForeignCluster %q: %w", providerLiqoClusterID, err)
	}
	return nil
}

// NamespaceOffloading is intentionally NOT managed by the agent. It is
// a per-K8s-namespace singleton (the Liqo admission webhook rejects any
// name other than "offloading"); creating or deleting it per-reservation
// would either collide with sibling reservations targeting the same
// namespace or rip offloading out from under them. The bootstrap-time
// NSO for the `default` namespace is stamped by Ansible (see
// deploy/ansible/roles/fa_consumer); workloads in other namespaces stamp
// their own as a one-shot operator action.

// resourceListToInterface converts a ResourceList into the
// map[string]interface{} shape Unstructured expects.
func resourceListToInterface(rl corev1.ResourceList) map[string]interface{} {
	out := make(map[string]interface{}, len(rl))
	for k, v := range rl {
		out[string(k)] = v.String()
	}
	return out
}

// objectExists is a small helper that returns true when the named
// object is present on the cluster. Used by tests; production code
// doesn't need it but it lives in the package to keep the test surface
// minimal. Tests currently only invoke it with namespace=="default",
// hence the //nolint:unparam below.
//
//nolint:unparam
func objectExists(
	ctx context.Context, c ctrlclient.Client,
	gvk schema.GroupVersionKind, namespace, name string,
) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj)
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, err
}
