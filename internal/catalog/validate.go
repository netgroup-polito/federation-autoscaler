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
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// ErrInvalidOffer is returned when an offer cannot be stored because its key,
// TTL or validity is not valid. Use errors.Is(err, ErrInvalidOffer) to test
// for it.
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

// MinTTLSeconds and MaxTTLSeconds bound Offer.TTLSeconds.
const (
	MinTTLSeconds = 60
	MaxTTLSeconds = 3600
)

// offerTTL returns how long the offer lives without a refresh: OfferTTL when
// TTLSeconds is 0, else TTLSeconds. A value outside
// [MinTTLSeconds, MaxTTLSeconds] is rejected with ErrInvalidOffer.
func offerTTL(o Offer) (time.Duration, error) {
	if o.TTLSeconds == 0 {
		return OfferTTL, nil
	}
	if o.TTLSeconds < MinTTLSeconds || o.TTLSeconds > MaxTTLSeconds {
		return 0, fmt.Errorf("%w: ttlSeconds %d is outside %d..%d",
			ErrInvalidOffer, o.TTLSeconds, MinTTLSeconds, MaxTTLSeconds)
	}
	return time.Duration(o.TTLSeconds) * time.Second, nil
}

// MaxValiditySeconds is the longest validity a provider can state: 30 days.
const MaxValiditySeconds = 2592000

// checkValidity rejects a negative or too large ValiditySeconds.
// 0 means "not stated" and is always allowed.
func checkValidity(o Offer) error {
	if o.ValiditySeconds < 0 || o.ValiditySeconds > MaxValiditySeconds {
		return fmt.Errorf("%w: validitySeconds %d is outside 0..%d",
			ErrInvalidOffer, o.ValiditySeconds, MaxValiditySeconds)
	}
	return nil
}
