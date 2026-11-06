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

package brain

import (
	"context"
	"errors"
	"fmt"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/broker/chunk"
)

// ReservationLister lists this consumer's own reservations
// (GET /api/v1/reservations). Satisfied by *agentclient.Client.
type ReservationLister interface {
	ListReservations(ctx context.Context) (*brokerapi.ReservationListResponse, error)
}

// Options configures a Brain.
type Options struct {
	// Catalog is where the provider offers are read from. Required.
	Catalog Reader

	// Reservations lists this consumer's own reservations. Required.
	Reservations ReservationLister

	// LocalClient reads the consumer's ConsumerPolicy. Required.
	LocalClient ctrlclient.Reader

	// Namespace is where the ConsumerPolicy lives.
	Namespace string

	// Location returns this consumer's coordinates, ok=false while unknown
	// (the location the heartbeat reports). Nil means no location.
	Location func() (lat, lon float64, ok bool)

	// MeasuredLatencyMs returns the latest round-trip times this consumer
	// measured per provider (the latency prober). Nil means none.
	MeasuredLatencyMs func() map[string]float64

	// Sizer turns capacity into chunks. Nil means the default sizer.
	Sizer chunk.Sizer
}

// Brain is the placement decision in local mode: the consumer chooses from the
// catalog itself instead of asking the Broker to choose. Booking still goes
// through the Broker; only the choice moved.
type Brain struct {
	opts Options
	full *FullProviders
}

// New validates opts and returns a Brain.
func New(opts Options) (*Brain, error) {
	switch {
	case opts.Catalog == nil:
		return nil, errors.New("brain: Catalog is required")
	case opts.Reservations == nil:
		return nil, errors.New("brain: Reservations is required")
	case opts.LocalClient == nil:
		return nil, errors.New("brain: LocalClient is required")
	}
	if opts.Sizer == nil {
		opts.Sizer = chunk.NewDefaultSizer()
	}
	return &Brain{opts: opts, full: NewFullProviders()}, nil
}

// NodeGroups makes one decision: it reads the offers, this consumer's own
// reservations and its ConsumerPolicy, and returns the node-group view the
// Cluster Autoscaler grows (the same shape the Broker returns in central mode).
func (b *Brain) NodeGroups(ctx context.Context) (*brokerapi.NodeGroupListResponse, error) {
	return b.NodeGroupsWithin(ctx, nil)
}

// NodeGroupsWithin is NodeGroups with a per-request latency bound (a
// ResourceRequest's spec.latencyBound). A non-nil requestBound overrides the
// ConsumerPolicy's bound for this one decision (see EffectiveBound).
func (b *Brain) NodeGroupsWithin(
	ctx context.Context, requestBound *autoscalingv1alpha1.LatencyBound,
) (*brokerapi.NodeGroupListResponse, error) {
	offers, err := b.opts.Catalog.ListOffers(ctx)
	if err != nil {
		return nil, fmt.Errorf("brain: list offers: %w", err)
	}
	resvs, err := b.opts.Reservations.ListReservations(ctx)
	if err != nil {
		return nil, fmt.Errorf("brain: list own reservations: %w", err)
	}
	spec, err := b.currentPolicy(ctx)
	if err != nil {
		return nil, err
	}

	own, inFlight := FromReservations(resvs.Reservations)
	consumer := brokerapi.ConsumerEntry{Placement: spec.Placement}
	if b.opts.Location != nil {
		consumer.Latitude, consumer.Longitude, consumer.HasLocation = b.opts.Location()
	}
	var measured map[string]float64
	if b.opts.MeasuredLatencyMs != nil {
		measured = b.opts.MeasuredLatencyMs()
	}

	resp := NodeGroups(Input{
		Offers:            offers,
		OwnReserved:       own,
		InFlight:          inFlight,
		Consumer:          consumer,
		LatencyBound:      EffectiveBound(spec.LatencyBound, requestBound),
		MeasuredLatencyMs: measured,
		Full:              b.full.Snapshot(),
		Sizer:             b.opts.Sizer,
	})
	return &resp, nil
}

// ObserveReservation records the outcome of a reservation attempt on
// provider: a "no free chunk" refusal marks it full for FullCooldown so the
// next decision moves on to the next-best provider. Other outcomes are
// ignored.
func (b *Brain) ObserveReservation(provider string, err error) {
	b.full.MarkIfNoCapacity(provider, err)
}

// currentPolicy returns the spec of the consumer's ConsumerPolicy, or the zero
// spec (Standard, no bound) when there is none. With several, the
// lowest-named wins, the same rule the heartbeat uses.
func (b *Brain) currentPolicy(ctx context.Context) (autoscalingv1alpha1.ConsumerPolicySpec, error) {
	var list autoscalingv1alpha1.ConsumerPolicyList
	if err := b.opts.LocalClient.List(ctx, &list, ctrlclient.InNamespace(b.opts.Namespace)); err != nil {
		return autoscalingv1alpha1.ConsumerPolicySpec{}, fmt.Errorf("brain: read ConsumerPolicy: %w", err)
	}
	if len(list.Items) == 0 {
		return autoscalingv1alpha1.ConsumerPolicySpec{}, nil
	}
	chosen := &list.Items[0]
	for i := range list.Items {
		if list.Items[i].Name < chosen.Name {
			chosen = &list.Items[i]
		}
	}
	return chosen.Spec, nil
}
