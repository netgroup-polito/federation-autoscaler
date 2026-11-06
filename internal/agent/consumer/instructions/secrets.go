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
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// KubeconfigSecretDataKey is the data key under which the peering-user
// kubeconfig lives in the consumer-side staging Secret. It mirrors the
// broker-side `kubeconfig` key so the two halves of the pipeline agree.
const KubeconfigSecretDataKey = "kubeconfig"

// kubeconfigSecretName returns the consumer-side staging-Secret name for a
// reservation: one Secret per reservation. It deliberately differs from the
// Broker-side Secret, which is one per (consumer, provider) pair because the
// Liqo peering user is a per-pair singleton (kubeconfigSecretName in
// internal/controller/broker/reservation_controller.go).
func kubeconfigSecretName(reservationID string) string {
	return "kubeconfig-" + reservationID
}

// persistKubeconfig upserts a Secret holding the peering-user
// kubeconfig the broker piggy-backed on the Peer instruction. The
// Secret survives agent restarts so a re-fetched Peer instruction can
// reuse it without re-asking the broker.
func persistKubeconfig(
	ctx context.Context,
	c ctrlclient.Client,
	namespace, reservationID, kubeconfig string,
) error {
	if kubeconfig == "" {
		return fmt.Errorf("kubeconfig is empty")
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeconfigSecretName(reservationID),
			Namespace: namespace,
			Labels: map[string]string{
				"federation-autoscaler.io/reservation": reservationID,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{KubeconfigSecretDataKey: []byte(kubeconfig)},
	}
	if err := c.Create(ctx, sec); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create kubeconfig secret: %w", err)
		}
		// Update with the latest bytes — covers the rare case where the
		// broker re-issues Peer with a refreshed kubeconfig.
		existing := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{
			Name: sec.Name, Namespace: namespace,
		}, existing); err != nil {
			return fmt.Errorf("get existing kubeconfig secret: %w", err)
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data[KubeconfigSecretDataKey] = []byte(kubeconfig)
		if err := c.Update(ctx, existing); err != nil {
			return fmt.Errorf("update kubeconfig secret: %w", err)
		}
	}
	return nil
}

// deleteKubeconfigSecret removes the staging Secret. Idempotent on
// missing — Unpeer with LastChunk=true calls this after the broker has
// already marked the reservation Released, so the absence of the
// Secret is not an error.
func deleteKubeconfigSecret(
	ctx context.Context,
	c ctrlclient.Client,
	namespace, reservationID string,
) error {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubeconfigSecretName(reservationID),
			Namespace: namespace,
		},
	}
	if err := c.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete kubeconfig secret: %w", err)
	}
	return nil
}

// writeKubeconfigToTempFile writes kubeconfig to a uniquely-named file
// under os.TempDir and returns the path plus a cleanup func the caller
// MUST defer. The file is mode 0600 so it is not world-readable.
func writeKubeconfigToTempFile(reservationID, kubeconfig string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "kubeconfig-"+reservationID+"-")
	if err != nil {
		return "", func() {}, fmt.Errorf("mkdir temp: %w", err)
	}
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", func() {}, fmt.Errorf("write kubeconfig: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	return path, cleanup, nil
}
