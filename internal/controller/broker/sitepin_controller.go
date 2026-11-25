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

package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// ResourceSliceGVK is Liqo's ResourceSlice. The consumer agent creates one per
// booking, named rs-<reservation>; Liqo replicates it to the provider.
var ResourceSliceGVK = schema.GroupVersionKind{
	Group:   "authentication.liqo.io",
	Version: "v1beta1",
	Kind:    "ResourceSlice",
}

// resourceSlicePrefix is the consumer agent's naming of a booking's slice.
const resourceSlicePrefix = "rs-"

// SitePinReconciler pins a booking's pods to the edge site its region
// orchestrator chose (M3). It runs only in a region orchestrator, which lives
// in the region's own cluster: it sets status.nodeSelector on the provider's
// copy of the consumer's ResourceSlice to {Label: Reservation.Spec.Site}.
// Liqo copies that selector to the consumer's virtual node, and every pod
// offloaded through it gets it, so the pods run only on that site's nodes.
//
// It never calls an agent: the slice is a local object of its own cluster.
type SitePinReconciler struct {
	client.Client

	// Label is the node label naming a node's edge site (--site-label).
	Label string

	// Namespace is where the Broker's Reservations live.
	Namespace string
}

// Reconcile sets the slice's node selector to its booking's site.
func (r *SitePinReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !strings.HasPrefix(req.Name, resourceSlicePrefix) {
		return ctrl.Result{}, nil // not a slice of one of our bookings
	}

	slice := &unstructured.Unstructured{}
	slice.SetGroupVersionKind(ResourceSliceGVK)
	if err := r.Get(ctx, req.NamespacedName, slice); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	resv := &brokerv1alpha1.Reservation{}
	key := types.NamespacedName{Name: strings.TrimPrefix(req.Name, resourceSlicePrefix), Namespace: r.Namespace}
	if err := r.Get(ctx, key, resv); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // a slice this orchestrator did not book
		}
		return ctrl.Result{}, err
	}
	site := resv.Spec.Site
	if site == "" {
		return ctrl.Result{}, nil
	}

	current, _, err := unstructured.NestedStringMap(slice.Object, "status", "nodeSelector")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read status.nodeSelector of %s: %w", req.NamespacedName, err)
	}
	if current[r.Label] == site {
		return ctrl.Result{}, nil
	}

	patch, err := json.Marshal(map[string]any{
		"status": map[string]any{"nodeSelector": map[string]string{r.Label: site}},
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Status().Patch(ctx, slice, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return ctrl.Result{}, fmt.Errorf("pin %s to site %q: %w", req.NamespacedName, site, err)
	}
	log.Info("pinned booking to its edge site", "slice", req.NamespacedName, "reservation", key.Name)
	return ctrl.Result{}, nil
}

// SetupWithManager watches Liqo ResourceSlices in every namespace (Liqo keeps
// them in the tenant namespaces).
func (r *SitePinReconciler) SetupWithManager(mgr ctrl.Manager) error {
	slice := &unstructured.Unstructured{}
	slice.SetGroupVersionKind(ResourceSliceGVK)
	return ctrl.NewControllerManagedBy(mgr).
		Named("broker-sitepin").
		For(slice).
		Complete(r)
}
