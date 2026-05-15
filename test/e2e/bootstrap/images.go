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

package bootstrap

import (
	"context"
	"fmt"

	"github.com/netgroup-polito/federation-autoscaler/test/e2e/kind"
)

// ImagePrefix matches the Makefile's IMG_PREFIX default. Image refs the
// e2e suite loads into Kind: <prefix>/<component>:<tag>.
const ImagePrefix = "federation-autoscaler"

// ImageTag is the tag the suite assumes already exists in the host's
// local docker daemon. `make docker-build` produces this tag; the
// suite never builds — it just loads what's there into Kind so iterating
// stays fast.
const ImageTag = "latest"

// LoadImageOptions configures LoadImage.
type LoadImageOptions struct {
	// ClusterName is the Kind cluster name (e.g. "fa-consumer-1").
	// Required.
	ClusterName string

	// Image is the fully-qualified image reference to load. Required.
	Image string

	// KindBinary overrides the kind executable. Empty resolves to $KIND
	// or "kind" on $PATH.
	KindBinary string
}

// LoadImage loads a locally-built docker image into the target Kind
// cluster (`kind load docker-image`). Idempotent — kind silently no-ops
// on a repeated load of the same image into the same cluster.
func LoadImage(ctx context.Context, opts LoadImageOptions) error {
	switch {
	case opts.ClusterName == "":
		return fmt.Errorf("LoadImage: ClusterName %w", errEmpty)
	case opts.Image == "":
		return fmt.Errorf("LoadImage: Image %w", errEmpty)
	}
	kindBin, err := resolveBinary(opts.KindBinary, "KIND", "kind")
	if err != nil {
		return err
	}
	return runCommand(ctx, kindBin,
		"load", "docker-image", opts.Image, "--name", opts.ClusterName)
}

// ImagesForRole returns the federation-autoscaler images that need to
// be loaded into a given role's Kind cluster:
//
//	central    → broker
//	consumer-1 → agent + grpc-server
//	provider-* → agent
func ImagesForRole(role kind.Role) []string {
	full := func(component string) string {
		return fmt.Sprintf("%s/%s:%s", ImagePrefix, component, ImageTag)
	}
	switch role {
	case kind.RoleCentral:
		return []string{full("broker")}
	case kind.RoleConsumer1:
		return []string{full("agent"), full("grpc-server")}
	case kind.RoleProvider1, kind.RoleProvider2:
		return []string{full("agent")}
	default:
		return nil
	}
}
