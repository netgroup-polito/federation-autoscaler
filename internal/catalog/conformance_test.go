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
	"context"
	"encoding/json"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// conformanceBackend is what one implementation (Memory, Redis) hands to the
// shared specs. The specs only use these fields, so the exact same checks run
// on every backend.
type conformanceBackend struct {
	w Writer
	r Reader

	// advance moves the backend's clock forward. The real TTL is 90 seconds,
	// so the specs cannot afford to wait for it: every backend must give the
	// specs a way to move time (a fake clock for Memory, FastForward for a
	// miniredis-backed Redis).
	advance func(d time.Duration)

	// putRaw stores raw JSON under a key with a fresh TTL, bypassing the Offer
	// type. It simulates a newer writer that adds a field this version does
	// not know.
	putRaw func(providerID, siteID string, raw []byte)

	// asProvider returns a Writer logged in as one provider account. A backend
	// with no access control leaves it nil and that spec is skipped.
	asProvider func(providerID string) Writer
}

// RunCatalogConformance defines the behaviour every catalog backend must have
// (catalog contract v1). Call it once per implementation; if a new backend
// passes these specs, swapping to it is safe.
//
// newBackend must return a fresh, empty backend.
func RunCatalogConformance(newBackend func() conformanceBackend) {
	var (
		ctx context.Context
		b   conformanceBackend
	)

	BeforeEach(func() {
		ctx = context.Background()
		b = newBackend()
	})

	It("returns what was stored", func() {
		want := sampleOffer("fibercop", "asti")
		Expect(b.w.UpsertOffer(ctx, want)).To(Succeed())

		got, err := b.r.GetOffer(ctx, "fibercop", "asti")
		Expect(err).NotTo(HaveOccurred())
		Expect(asJSON(got)).To(MatchJSON(asJSON(want)))
	})

	It("returns ErrNotFound for an offer that was never published", func() {
		_, err := b.r.GetOffer(ctx, "fibercop", "asti")
		Expect(err).To(MatchError(ErrNotFound))
	})

	It("returns an empty list, not an error, when the catalog is empty", func() {
		all, err := b.r.ListOffers(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(BeEmpty())
	})

	// -----------------------------------------------------------------------
	// expiry
	// -----------------------------------------------------------------------

	It("keeps an offer alive until OfferTTL has passed, then drops it", func() {
		Expect(b.w.UpsertOffer(ctx, sampleOffer("fibercop", "asti"))).To(Succeed())

		b.advance(OfferTTL - time.Second)
		_, err := b.r.GetOffer(ctx, "fibercop", "asti")
		Expect(err).NotTo(HaveOccurred())

		b.advance(2 * time.Second)
		_, err = b.r.GetOffer(ctx, "fibercop", "asti")
		Expect(err).To(MatchError(ErrNotFound))

		all, err := b.r.ListOffers(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(BeEmpty())
	})

	It("restarts the TTL when the provider publishes again", func() {
		o := sampleOffer("fibercop", "asti")
		Expect(b.w.UpsertOffer(ctx, o)).To(Succeed())

		b.advance(60 * time.Second)
		Expect(b.w.UpsertOffer(ctx, o)).To(Succeed())

		// 120 s since the first write, but only 60 s since the refresh.
		b.advance(60 * time.Second)
		_, err := b.r.GetOffer(ctx, "fibercop", "asti")
		Expect(err).NotTo(HaveOccurred())

		// 91 s since the refresh.
		b.advance(31 * time.Second)
		_, err = b.r.GetOffer(ctx, "fibercop", "asti")
		Expect(err).To(MatchError(ErrNotFound))
	})

	It("lists the live offers and leaves out the expired ones", func() {
		Expect(b.w.UpsertOffer(ctx, sampleOffer("fibercop", "asti"))).To(Succeed())
		b.advance(60 * time.Second)
		Expect(b.w.UpsertOffer(ctx, sampleOffer("fibercop", "cuneo"))).To(Succeed())
		b.advance(40 * time.Second) // asti is 100 s old, cuneo is 40 s old

		all, err := b.r.ListOffers(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(1))
		Expect(all[0].SiteID).To(Equal("cuneo"))
	})

	// -----------------------------------------------------------------------
	// one offer per (provider, site)
	// -----------------------------------------------------------------------

	It("keeps one offer per provider and site", func() {
		for _, o := range []Offer{
			sampleOffer("fibercop", "asti"),
			sampleOffer("fibercop", "cuneo"),
			sampleOffer("fibercop", "alba"),
			sampleOffer("acme", "asti"), // same site name, different provider
		} {
			Expect(b.w.UpsertOffer(ctx, o)).To(Succeed())
		}

		all, err := b.r.ListOffers(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(4))
	})

	It("overwrites an existing offer rather than duplicating it", func() {
		Expect(b.w.UpsertOffer(ctx, sampleOffer("fibercop", "asti"))).To(Succeed())
		Expect(b.w.UpsertOffer(ctx, sampleOffer("fibercop", "cuneo"))).To(Succeed())

		changed := sampleOffer("fibercop", "asti")
		changed.Available[corev1.ResourceCPU] = resource.MustParse("4")
		Expect(b.w.UpsertOffer(ctx, changed)).To(Succeed())

		all, err := b.r.ListOffers(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(2))

		got, err := b.r.GetOffer(ctx, "fibercop", "asti")
		Expect(err).NotTo(HaveOccurred())
		Expect(asJSON(got)).To(MatchJSON(asJSON(changed)))
	})

	It("removes an offer on delete, and deleting a missing one is not an error", func() {
		Expect(b.w.UpsertOffer(ctx, sampleOffer("fibercop", "asti"))).To(Succeed())
		Expect(b.w.DeleteOffer(ctx, "fibercop", "asti")).To(Succeed())

		_, err := b.r.GetOffer(ctx, "fibercop", "asti")
		Expect(err).To(MatchError(ErrNotFound))

		Expect(b.w.DeleteOffer(ctx, "fibercop", "asti")).To(Succeed())
	})

	// -----------------------------------------------------------------------
	// evolution of the format
	// -----------------------------------------------------------------------

	It("ignores fields it does not know", func() {
		// A newer writer may add a field. A reader must not fail because of
		// it, or the first new field would break every older reader.
		b.putRaw("fibercop", "asti", []byte(`{
			"schemaVersion": 1,
			"providerId": "fibercop",
			"siteId": "asti",
			"available": {"cpu": "4"},
			"someFutureField": {"x": 1},
			"updatedAt": "2026-11-03T10:00:00Z"
		}`))

		got, err := b.r.GetOffer(ctx, "fibercop", "asti")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.ProviderID).To(Equal("fibercop"))
		cpu := got.Available[corev1.ResourceCPU]
		Expect(cpu.Cmp(resource.MustParse("4"))).To(BeZero())

		all, err := b.r.ListOffers(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(1))
	})

	// -----------------------------------------------------------------------
	// keys
	// -----------------------------------------------------------------------

	for _, k := range []struct{ name, provider, site string }{
		{"an empty provider", "", "asti"},
		{"an empty site", "fibercop", ""},
		{"a colon in the provider", "fiber:cop", "asti"},
		{"a colon in the site", "fibercop", "as:ti"},
		{"a star in the provider", "p*", "asti"},
		{"a question mark in the provider", "p?", "asti"},
		{"brackets in the provider", "p[1]", "asti"},
		{"an uppercase provider", "P1", "asti"},
		{"a leading dash in the provider", "-p1", "asti"},
		{"a 64-character provider", strings.Repeat("a", 64), "asti"},
		{"a star in the site", "fibercop", "as*"},
		{"an uppercase site", "fibercop", "Asti"},
		{"a 64-character site", "fibercop", strings.Repeat("a", 64)},
	} {
		It("rejects "+k.name, func() {
			// The Redis key is catalog:offer:<provider>:<site>. A colon would
			// let two different pairs land on the same key.
			err := b.w.UpsertOffer(ctx, sampleOffer(k.provider, k.site))
			Expect(err).To(MatchError(ErrInvalidOffer))

			err = b.w.DeleteOffer(ctx, k.provider, k.site)
			Expect(err).To(MatchError(ErrInvalidOffer))
		})
	}
	for _, v := range []struct{ provider, site string }{
		{"p1", "default"},
		{"crownlabs-polito", "asti"},
		{"fibercop", "default"},
	} {
		It("accepts "+v.provider+"/"+v.site, func() {
			Expect(b.w.UpsertOffer(ctx, sampleOffer(v.provider, v.site))).To(Succeed())
		})
	}

	It("skips an offer whose content disagrees with its key", func() {
		Expect(b.w.UpsertOffer(ctx, sampleOffer("fibercop", "asti"))).To(Succeed())

		// Stored under fibercop/cuneo, but the content says another provider.
		otherProvider, err := json.Marshal(sampleOffer("acme", "cuneo"))
		Expect(err).NotTo(HaveOccurred())
		b.putRaw("fibercop", "cuneo", otherProvider)

		// Stored under fibercop/alba, but the content says another site.
		otherSite, err := json.Marshal(sampleOffer("fibercop", "default"))
		Expect(err).NotTo(HaveOccurred())
		b.putRaw("fibercop", "alba", otherSite)

		_, err = b.r.GetOffer(ctx, "fibercop", "cuneo")
		Expect(err).To(MatchError(ErrNotFound))
		_, err = b.r.GetOffer(ctx, "fibercop", "alba")
		Expect(err).To(MatchError(ErrNotFound))

		all, err := b.r.ListOffers(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(1))
		Expect(all[0].ProviderID).To(Equal("fibercop"))
		Expect(all[0].SiteID).To(Equal("asti"))
	})
	// -----------------------------------------------------------------------
	// isolation
	// -----------------------------------------------------------------------

	It("hands back a copy the caller cannot use to reach into the store", func() {
		// A real backend serialises, so a caller never holds a reference to
		// what it stored. Every implementation has to behave that way, or
		// code that works on one backend corrupts another.
		want := sampleOffer("fibercop", "asti")
		Expect(b.w.UpsertOffer(ctx, want)).To(Succeed())

		got, err := b.r.GetOffer(ctx, "fibercop", "asti")
		Expect(err).NotTo(HaveOccurred())
		got.Extra["note"] = "changed by the caller"
		got.Available[corev1.ResourceCPU] = resource.MustParse("999")

		again, err := b.r.GetOffer(ctx, "fibercop", "asti")
		Expect(err).NotTo(HaveOccurred())
		Expect(asJSON(again)).To(MatchJSON(asJSON(want)))
	})

	// -----------------------------------------------------------------------
	// access control
	// -----------------------------------------------------------------------

	It("does not let a provider write or delete another provider's key", func() {
		if b.asProvider == nil {
			Skip("this backend has no access control (Redis ACL only)")
		}
		acme := b.asProvider("acme")

		Expect(acme.UpsertOffer(ctx, sampleOffer("acme", "default"))).To(Succeed(),
			"a provider must be able to write its own key")

		Expect(acme.UpsertOffer(ctx, sampleOffer("fibercop", "asti"))).NotTo(Succeed(),
			"acme must not be able to write a fibercop key")
		Expect(acme.DeleteOffer(ctx, "fibercop", "asti")).NotTo(Succeed(),
			"acme must not be able to delete a fibercop key")

		_, err := b.r.GetOffer(ctx, "fibercop", "asti")
		Expect(err).To(MatchError(ErrNotFound), "the forbidden write must leave nothing behind")
	})
}

func f64(v float64) *float64 { return &v }

// asJSON turns an offer into JSON so two offers can be compared with
// MatchJSON. reflect.DeepEqual would be wrong here: a resource.Quantity that
// went through JSON keeps hidden internal state and can differ from the
// original even when the value is the same.
func asJSON(o Offer) string {
	data, err := json.Marshal(o)
	Expect(err).NotTo(HaveOccurred())
	return string(data)
}

// sampleOffer is a complete, valid offer used across the specs.
func sampleOffer(providerID, siteID string) Offer {
	return Offer{
		SchemaVersion: 1,
		ProviderID:    providerID,
		LiqoClusterID: providerID,
		SiteID:        siteID,
		Location: Location{
			Region:    "Piedmont",
			City:      siteID,
			Latitude:  f64(44.90),
			Longitude: f64(8.21),
		},
		Available: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("12"),
			corev1.ResourceMemory: resource.MustParse("24Gi"),
			"nvidia.com/gpu":      resource.MustParse("2"),
		},
		UnitPrices: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("0.03"),
			corev1.ResourceMemory: resource.MustParse("0.004"),
		},
		ExpectedLatencyMs: f64(2),
		ProbeEndpoint:     siteID + ".example:7777",
		Extra:             map[string]any{"note": "renewable at night"},
		UpdatedAt:         time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC),
	}
}
