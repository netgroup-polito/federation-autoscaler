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

package brokers

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// ConfigMapName is the ConfigMap holding the brokers a consumer agent learned.
const ConfigMapName = "agent-brokers"

// configMapKey holds the URLs, one per line.
const configMapKey = "urls"

// ConfigMapStore saves the learned brokers in a ConfigMap of the agent's
// namespace.
type ConfigMapStore struct {
	Client    ctrlclient.Client
	Namespace string
}

// Load returns the saved URLs (none when the ConfigMap does not exist).
func (s *ConfigMapStore) Load(ctx context.Context) ([]string, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Name: ConfigMapName, Namespace: s.Namespace}
	if err := s.Client.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return strings.Fields(cm.Data[configMapKey]), nil
}

// Save writes the URLs, creating the ConfigMap when needed.
func (s *ConfigMapStore) Save(ctx context.Context, urls []string) error {
	data := map[string]string{configMapKey: strings.Join(urls, "\n")}
	var cm corev1.ConfigMap
	key := types.NamespacedName{Name: ConfigMapName, Namespace: s.Namespace}
	switch err := s.Client.Get(ctx, key, &cm); {
	case apierrors.IsNotFound(err):
		return s.Client.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: s.Namespace},
			Data:       data,
		})
	case err != nil:
		return err
	}
	cm.Data = data
	return s.Client.Update(ctx, &cm)
}
