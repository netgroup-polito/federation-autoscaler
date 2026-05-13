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

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// installLiqoStubCRDs registers minimal CustomResourceDefinitions for
// the two Liqo types the consumer Peer / Unpeer / Cleanup handlers
// create via unstructured.Unstructured:
//
//   - authentication.liqo.io/v1beta1/ResourceSlice
//   - offloading.liqo.io/v1beta1/NamespaceOffloading
//
// envtest's apiserver rejects creates of unregistered GVKs with
// "no matches for kind", so the consumer e2e test would otherwise fail
// before exercising the wire contract. We install permissive schemas
// (x-kubernetes-preserve-unknown-fields) so the agent's minimal Spec
// shape is accepted without us mirroring Liqo's full validation rules.
//
// Production deployments use Liqo's real CRD YAMLs; this stub is
// strictly for the integration suite.
func installLiqoStubCRDs(ctx context.Context, c ctrlclient.Client) error {
	crds := []*apiextensionsv1.CustomResourceDefinition{
		permissiveCRD("resourceslices.authentication.liqo.io", "authentication.liqo.io", "ResourceSlice", "resourceslices", "resourceslice", "v1beta1", apiextensionsv1.NamespaceScoped),
		permissiveCRD("namespaceoffloadings.offloading.liqo.io", "offloading.liqo.io", "NamespaceOffloading", "namespaceoffloadings", "namespaceoffloading", "v1beta1", apiextensionsv1.NamespaceScoped),
		permissiveCRDWithStatus("virtualnodes.offloading.liqo.io", "offloading.liqo.io", "VirtualNode", "virtualnodes", "virtualnode", "v1beta1", apiextensionsv1.NamespaceScoped),
	}
	for _, crd := range crds {
		if err := c.Create(ctx, crd); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

// permissiveCRD constructs a CustomResourceDefinition whose schema
// preserves unknown fields — enough for envtest to accept any payload
// the agent might POST, without us mirroring Liqo's validation. The
// singular name must be a DNS-1035 label (lowercase), distinct from
// the camel-cased Kind.
func permissiveCRD(name, group, kind, plural, singular, version string, scope apiextensionsv1.ResourceScope) *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:     kind,
				ListKind: kind + "List",
				Plural:   plural,
				Singular: singular,
			},
			Scope: scope,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    version,
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type:                   "object",
						XPreserveUnknownFields: &preserve,
					},
				},
			}},
		},
	}
}

// permissiveCRDWithStatus is permissiveCRD plus a /status subresource
// — required by the VirtualNodeStateReconciler's test fixtures, which
// patch the Liqo VirtualNode's status separately to simulate Liqo
// materialising the node.
func permissiveCRDWithStatus(name, group, kind, plural, singular, version string, scope apiextensionsv1.ResourceScope) *apiextensionsv1.CustomResourceDefinition {
	crd := permissiveCRD(name, group, kind, plural, singular, version, scope)
	crd.Spec.Versions[0].Subresources = &apiextensionsv1.CustomResourceSubresources{
		Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
	}
	return crd
}
