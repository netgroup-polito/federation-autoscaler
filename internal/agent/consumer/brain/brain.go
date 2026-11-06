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
	"sync"

	"github.com/go-logr/logr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
	"github.com/netgroup-polito/federation-autoscaler/internal/broker/chunk"
)

// ReservationLister lists this consumer's own reservations
// (GET /api/v1/reservations). Satisfied by *agentclient.Client.
type ReservationLister interface {
	ListReservations(ctx context.Context) (*brokerapi.ReservationListResponse, error)
}

// LatencyProber measures round-trip times to probe endpoints (+Inf for one
// that does not answer). Satisfied by *latency.Prober.
type LatencyProber interface {
	Measure(ctx context.Context, endpoints []string) map[string]float64
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

	// Prober measures the round-trip time to each candidate site's probe
	// endpoint whenever a latency bound applies, under any placement policy.
	// Nil means no measurement: the bound is checked on the distance estimate.
	Prober LatencyProber

	// Logger defaults to controller-runtime's logger named "brain".
	Logger logr.Logger

	// Sizer turns capacity into chunks. Nil means the default sizer.
	Sizer chunk.Sizer
}

// Brain is the placement decision in local mode: the consumer chooses from the
// catalog itself instead of asking the Broker to choose. Booking still goes
// through the Broker; only the choice moved.
type Brain struct {
	opts Options
	full *FullProviders
	log  logr.Logger

	choiceMu   sync.Mutex
	lastChoice string
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
	logger := opts.Logger
	if logger.GetSink() == nil {
		logger = log.Log.WithName("brain")
	}
	return &Brain{opts: opts, full: NewFullProviders(), log: logger}, nil
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
	bound := EffectiveBound(spec.LatencyBound, requestBound)
	var measured map[string]float64
	if bound != nil && b.opts.Prober != nil {
		// Measure every candidate site, not a shortlist: the bound is a hard
		// filter, so each site must be checked on a real round-trip.
		measured = b.opts.Prober.Measure(ctx, probeEndpoints(offers))
	}

	resp, estimated := nodeGroups(Input{
		Offers:            offers,
		OwnReserved:       own,
		InFlight:          inFlight,
		Consumer:          consumer,
		LatencyBound:      bound,
		MeasuredLatencyMs: measured,
		Full:              b.full.Snapshot(),
		Sizer:             b.opts.Sizer,
	})
	if len(estimated) > 0 {
		b.log.V(1).Info("latency bound checked on the distance estimate (no measurement) for some sites",
			"maxLatencyMs", bound.MaxLatencyMs, "estimated", estimated)
	}
	return &resp, nil
}

// ObserveReservation records the outcome of a reservation attempt on
// provider: a "no free chunk" refusal marks it full for FullCooldown so the
// next decision moves on to the next-best provider. Other outcomes are
// ignored.
func (b *Brain) ObserveReservation(provider string, err error) {
	b.full.MarkIfNoCapacity(provider, err)
}

// RecordChoice remembers which provider the decision served to the Cluster
// Autoscaler left growable (the first one, in list order; "" when none), so
// the heartbeat can report it for the Broker dashboard. Call it with the final
// view, after any latency re-mask or ConsumerChoice masking.
func (b *Brain) RecordChoice(resp *brokerapi.NodeGroupListResponse) {
	chosen := ""
	for _, ng := range resp.NodeGroups {
		if ng.MaxSize > ng.CurrentReserved {
			chosen = ng.ProviderClusterID
			break
		}
	}
	b.choiceMu.Lock()
	b.lastChoice = chosen
	b.choiceMu.Unlock()
}

// LastChoice returns the provider recorded by the latest RecordChoice ("" when
// none yet, or when nothing was growable).
func (b *Brain) LastChoice() string {
	b.choiceMu.Lock()
	defer b.choiceMu.Unlock()
	return b.lastChoice
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
