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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
)

// DefaultAdvertisementStaleAfter is the default value of
// ClusterAdvertisementReconciler.StaleAfter. Three advertisement cycles
// (the Provider Agent advertises every 30 s, see docs/design.md §7.3.1)
// is the standard "tolerate two missed beats before flipping unavailable"
// trade-off. A future ConfigMap-backed override (chunk-config
// agent-heartbeat-timeout) lands with the rest of the dynamic settings.
const DefaultAdvertisementStaleAfter = 90 * time.Second

// ClusterAdvertisementReconciler enforces the freshness invariant of every
// ClusterAdvertisement: status.available stays true only as long as
// status.lastSeen is newer than StaleAfter. The HTTP advertisement handler
// is what flips Available to true (and bumps LastSeen) on each successful
// POST; this reconciler is the only path through which Available ever
// becomes false again.
//
// Because the reconciler triggers on watch events alone would miss the
// "no event arrives because the agent is gone" case, every Reconcile
// returns RequeueAfter set to a fraction of StaleAfter so the controller
// re-evaluates freshness on a steady cadence even when the CR has not
// changed.
type ClusterAdvertisementReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// StaleAfter overrides the freshness window. Zero means
	// DefaultAdvertisementStaleAfter; set to a small duration in tests.
	StaleAfter time.Duration
}

// +kubebuilder:rbac:groups=broker.federation-autoscaler.io,resources=clusteradvertisements,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=broker.federation-autoscaler.io,resources=clusteradvertisements/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=broker.federation-autoscaler.io,resources=clusteradvertisements/finalizers,verbs=update

// Reconcile updates status.available based on how recent status.lastSeen
// is. Returns RequeueAfter == staleAfter()/3 so the next stale check fires
// well before the agent's next 30 s tick could mask a real outage.
func (r *ClusterAdvertisementReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cadv brokerv1alpha1.ClusterAdvertisement
	if err := r.Get(ctx, req.NamespacedName, &cadv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	stale := isAdvertisementStale(cadv.Status.LastSeen, r.staleAfter())

	patched := cadv.DeepCopy()
	patched.Status.ObservedGeneration = cadv.Generation
	patched.Status.Available = !stale
	if stale {
		// AvailableChunks must drop to zero so /nodegroups (which it
		// already filters by Available, but a downstream consumer may
		// still trust this number) cannot keep advertising capacity
		// nobody is reporting on.
		patched.Status.AvailableChunks = 0
	}

	needUpdate := patched.Status.Available != cadv.Status.Available ||
		patched.Status.ObservedGeneration != cadv.Status.ObservedGeneration ||
		(stale && cadv.Status.AvailableChunks != 0)

	if needUpdate {
		if err := r.Status().Update(ctx, patched); err != nil {
			return ctrl.Result{}, err
		}
		if stale {
			log.Info("ClusterAdvertisement flipped unavailable",
				"name", cadv.Name, "lastSeen", cadv.Status.LastSeen)
		}
	}

	return ctrl.Result{RequeueAfter: r.staleAfter() / 3}, nil
}

// staleAfter resolves StaleAfter against its default.
func (r *ClusterAdvertisementReconciler) staleAfter() time.Duration {
	if r.StaleAfter > 0 {
		return r.StaleAfter
	}
	return DefaultAdvertisementStaleAfter
}

// isAdvertisementStale returns true when lastSeen is missing or older than
// ttl. A nil LastSeen is treated as stale because no advertisement has
// ever arrived for this provider — Available must therefore be false
// until the first POST lands.
func isAdvertisementStale(lastSeen *metav1.Time, ttl time.Duration) bool {
	if lastSeen == nil {
		return true
	}
	return time.Since(lastSeen.Time) > ttl
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterAdvertisementReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&brokerv1alpha1.ClusterAdvertisement{}).
		Named("broker-clusteradvertisement").
		Complete(r)
}
