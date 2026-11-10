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

package catalog

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// ErrInvalidOffer is returned when an offer cannot be stored because its key
// is not valid. Use errors.Is(err, ErrInvalidOffer) to test for it.
var ErrInvalidOffer = errors.New("catalog: invalid offer")

// validateKey checks that providerID and siteID are safe to embed in a
// catalog key (catalog:offer:<providerId>:<siteId>) and in Redis SCAN and
// ACL patterns. Both must be DNS-1123 labels, the same rule Liqo applies to
// cluster IDs. This rejects empty IDs, ':' (key collisions) and Redis glob
// characters (* ? [ ] \).
func validateKey(providerID, siteID string) error {
	for _, f := range []struct{ name, id string }{
		{"providerId", providerID},
		{"siteId", siteID},
	} {
		if errs := validation.IsDNS1123Label(f.id); len(errs) > 0 {
			return fmt.Errorf("%w: %s %q is invalid: %s",
				ErrInvalidOffer, f.name, f.id, strings.Join(errs, "; "))
		}
	}
	return nil
}
