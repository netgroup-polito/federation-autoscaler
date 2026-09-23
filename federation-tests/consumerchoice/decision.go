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

package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/federation-tests/testlib"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/ollama"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// Selection sources.
const (
	sourceAI              = "ai"
	sourceFallback        = "fallback"
	sourceSingleCandidate = "single-candidate"
	sourceNone            = "none"
)

// Failure categories beyond the selector's own ErrorKinds.
const (
	failProviderNotListed   = "provider_no_longer_listed"
	failProviderNoCapacity  = "provider_no_capacity"
	failMissingReservation  = "missing_reservation_fields"
	failPolicyChanged       = "policy_changed_before_reservation"
	failFallbackNoCandidate = "fallback_no_eligible_provider"
	failReservationRejected = "reservation_rejected"
	failReservationTimeout  = "reservation_timeout"
	failReservationBadPhase = "reservation_failed_phase"
	failReleaseError        = "release_error"
	failCapacityLeak        = "capacity_leak"
	failBrokerUnavailable   = "broker_unavailable"
)

// isModelAnswerKind reports whether an answer failure is the model's own doing:
// it did not finish in time (typically a repetition loop), did not write the
// requested JSON, or named no provider or one it was not given. Like a missed
// criterion, that is a result about the model, and the fallback covers it.
// Every other failure -- Ollama unreachable or answering with an error, the
// Broker changing under the selection, a fallback with nothing to pick -- is
// the system's.
func isModelAnswerKind(kind string) bool {
	switch ollama.ErrorKind(kind) {
	case ollama.ErrKindTimeout, ollama.ErrKindInvalidJSON, ollama.ErrKindEmptyProviderID,
		ollama.ErrKindUnknownProviderID:
		return true
	}
	return false
}

// Validation records every check applied to the model's answer before any
// reservation is made. The answer is untrusted input: each check is recorded
// individually so a failure can be traced to the exact step that rejected it.
type Validation struct {
	SelectedProviderID   string `json:"selectedProviderId"`
	ModelProviderIDField string `json:"modelProviderIdField,omitempty"`
	RankingConsistent    *bool  `json:"providerIdMatchesRankingHead,omitempty"`
	// UnknownIDsInRanking are invented IDs anywhere in the model's ranking. Only
	// one in first place invalidates the answer; the rest are recorded.
	UnknownIDsInRanking       []string `json:"unknownIdsInRanking,omitempty"`
	ValidJSON                 bool     `json:"validJson"`
	NonEmpty                  bool     `json:"nonEmpty"`
	InCandidateList           bool     `json:"inCandidateList"`
	StillListed               bool     `json:"stillListedByBroker"`
	StillHasCapacity          bool     `json:"stillHasCapacity"`
	PolicyStillConsumerChoice bool     `json:"policyStillConsumerChoice"`
	ReservationFieldsPresent  bool     `json:"reservationFieldsPresent"`
	Valid                     bool     `json:"valid"`
	Errors                    []string `json:"errors,omitempty"`
	FallbackApplied           bool     `json:"fallbackApplied"`
	FallbackMode              string   `json:"fallbackMode"`
	FallbackProviderID        string   `json:"fallbackProviderId,omitempty"`
}

// Decision is the outcome of one ConsumerChoice decision.
type Decision struct {
	Trace           *ollama.Trace                    `json:"-"`
	Validation      Validation                       `json:"validation"`
	Source          string                           `json:"source"`
	FinalProviderID string                           `json:"finalProviderId,omitempty"`
	FinalNodeGroup  *brokerapi.NodeGroupView         `json:"-"`
	DecisionLatency time.Duration                    `json:"-"`
	Failures        []string                         `json:"failures,omitempty"`
	FreshSnapshot   *brokerapi.NodeGroupListResponse `json:"-"`
}

// AICalled reports whether the model was actually asked.
func (d *Decision) AICalled() bool { return d.Trace != nil && !d.Trace.SingleCandidate }

// ModelAnswerFailure reports whether the model was asked and its answer was
// rejected only for reasons that are the model's own (see isModelAnswerKind).
func (d *Decision) ModelAnswerFailure() bool {
	if !d.AICalled() || d.Validation.Valid || len(d.Failures) == 0 {
		return false
	}
	for _, f := range d.Failures {
		if !isModelAnswerKind(f) {
			return false
		}
	}
	return true
}

// decide runs one selection through the agent's own selector, validates the
// answer against the list it was given and against a fresh Broker read, and
// applies the configured fallback when validation fails.
func decide(ctx context.Context, rt *ollamaRuntime, broker *testlib.BrokerClient, cc *ChoiceConfig,
	sc Scenario, candidates []brokerapi.NodeGroupView, consumer *ollama.Location) *Decision {
	client := ollama.NewWithOptions(rt.baseURL, rt.cfg.Model, rt.clientOptions(consumer))
	trace, selErr := client.SelectDetailed(ctx, sc.UserRequest, candidates)

	validation, failures := validateAnswer(trace, selErr)
	validation.FallbackMode = cc.Fallback.Mode
	d := &Decision{Trace: trace, Validation: validation, Source: sourceNone, Failures: failures}
	d.checkAgainstBroker(ctx, broker)

	v := &d.Validation
	// A single candidate means the model was never asked, so there is no JSON
	// to have been valid; every other check still applies.
	answerOK := v.ValidJSON || trace.SingleCandidate
	v.Valid = selErr == nil && answerOK && v.NonEmpty && v.InCandidateList && v.StillListed &&
		v.StillHasCapacity && v.ReservationFieldsPresent && v.PolicyStillConsumerChoice
	d.DecisionLatency = time.Since(trace.StartedAt)

	switch {
	case v.Valid && trace.SingleCandidate:
		d.Source, d.FinalProviderID = sourceSingleCandidate, v.SelectedProviderID
	case v.Valid:
		d.Source, d.FinalProviderID = sourceAI, v.SelectedProviderID
	default:
		d.FinalNodeGroup = nil
		applyFallback(d, cc.Fallback.Mode)
	}
	return d
}

// validateAnswer applies the checks that need only the selector's trace.
//
// The model's selection is the first entry of its ranking as the model wrote
// it. The selector's filtered ranking (trace.Ranked) is not used for this: it
// drops invented IDs, so a model that ranked a non-existent provider first
// would have its runner-up reported as its choice, and the invalid answer would
// count as a valid one.
func validateAnswer(trace *ollama.Trace, selErr error) (Validation, []string) {
	var v Validation
	var failures []string
	fail := func(category, msg string) {
		v.Errors = append(v.Errors, msg)
		failures = append(failures, category)
	}

	switch p := trace.Parsed; {
	case p != nil && len(p.RankedList) > 0:
		v.SelectedProviderID = p.RankedList[0]
		if p.ProviderID != "" {
			consistent := p.ProviderID == p.RankedList[0]
			v.RankingConsistent = &consistent
		}
	case p != nil:
		v.SelectedProviderID = p.ProviderID
	case trace.SingleCandidate && len(trace.Ranked) > 0:
		v.SelectedProviderID = trace.Ranked[0]
	}
	if trace.Parsed != nil {
		v.ValidJSON = true
		v.ModelProviderIDField = trace.Parsed.ProviderID
	}
	v.UnknownIDsInRanking = trace.UnknownIDs

	if selErr != nil {
		// The selector already rejected the answer; its kind is the category.
		fail(string(trace.ErrorKind), selErr.Error())
	}
	v.NonEmpty = v.SelectedProviderID != ""
	for _, p := range trace.Providers {
		if p.ProviderID == v.SelectedProviderID {
			v.InCandidateList = true
			break
		}
	}
	if selErr == nil && !v.NonEmpty {
		fail(string(ollama.ErrKindEmptyProviderID), "the model's ranking starts with an empty provider ID")
	}
	if selErr == nil && v.NonEmpty && !v.InCandidateList {
		fail(string(ollama.ErrKindUnknownProviderID),
			fmt.Sprintf("the model ranked %q first, which is not in the candidate list sent to it", v.SelectedProviderID))
	}
	return v, failures
}

// fail records a failed check.
func (d *Decision) fail(category, msg string) {
	d.Validation.Errors = append(d.Validation.Errors, msg)
	d.Failures = append(d.Failures, category)
}

// checkAgainstBroker re-reads the Broker, since the list may have moved while
// the model was thinking, and sets FinalNodeGroup when the selection can still
// be reserved.
func (d *Decision) checkAgainstBroker(ctx context.Context, broker *testlib.BrokerClient) {
	v := &d.Validation
	fresh, err := broker.GetNodeGroups(ctx)
	if err != nil {
		d.fail(failBrokerUnavailable, fmt.Sprintf("re-read Broker node groups: %v", err))
		return
	}
	d.FreshSnapshot = fresh
	v.PolicyStillConsumerChoice = fresh.AppliedPlacement == autoscalingv1alpha1.PlacementStrategyConsumerChoice
	if !v.PolicyStillConsumerChoice {
		d.fail(failPolicyChanged, fmt.Sprintf("Broker now applies %q", fresh.AppliedPlacement))
	}
	if !v.InCandidateList {
		return
	}
	ng := standardGroup(fresh.NodeGroups, v.SelectedProviderID)
	if ng == nil {
		d.fail(failProviderNotListed, v.SelectedProviderID+" is no longer listed by the Broker")
		return
	}
	v.StillListed = true
	v.StillHasCapacity = ng.MaxSize > ng.CurrentReserved
	v.ReservationFieldsPresent = ng.ID != "" && ng.ProviderClusterID != "" && ng.Type != ""
	if !v.StillHasCapacity {
		d.fail(failProviderNoCapacity, v.SelectedProviderID+" has no free capacity any more")
	}
	if !v.ReservationFieldsPresent {
		d.fail(failMissingReservation, v.SelectedProviderID+" lacks a field needed to reserve it")
	}
	if v.StillHasCapacity && v.ReservationFieldsPresent {
		d.FinalNodeGroup = ng
	}
}

// applyFallback picks a provider deterministically from the fresh Broker list
// when the model's answer failed validation, and always records that it did so.
func applyFallback(d *Decision, mode string) {
	if mode == fallbackFail || d.FreshSnapshot == nil ||
		d.FreshSnapshot.AppliedPlacement != autoscalingv1alpha1.PlacementStrategyConsumerChoice {
		return
	}
	eligible := testlib.GrowableNodeGroups(d.FreshSnapshot.NodeGroups)
	var standard []brokerapi.NodeGroupView
	for _, ng := range eligible {
		if ng.Type == brokerv1alpha1.ChunkTypeStandard {
			standard = append(standard, ng)
		}
	}
	var id string
	switch mode {
	case fallbackAgentDefault:
		if ranked, ok := ollama.DeterministicFallback(standard); ok {
			id = ranked[0]
		}
	case fallbackCheapest:
		id = pickLowest(standard, func(ng brokerapi.NodeGroupView) *float64 {
			if ng.Cost == nil {
				return nil
			}
			c := ng.Cost.AsApproximateFloat64()
			return &c
		}, func(ng brokerapi.NodeGroupView) *float64 { return ng.CarbonIntensity })
	case fallbackLowestCarbon:
		id = pickLowest(standard, func(ng brokerapi.NodeGroupView) *float64 { return ng.CarbonIntensity },
			func(ng brokerapi.NodeGroupView) *float64 {
				if ng.Cost == nil {
					return nil
				}
				c := ng.Cost.AsApproximateFloat64()
				return &c
			})
	}
	d.Validation.FallbackApplied = true
	if id == "" {
		d.Failures = append(d.Failures, failFallbackNoCandidate)
		return
	}
	d.Validation.FallbackProviderID = id
	d.Source, d.FinalProviderID = sourceFallback, id
	d.FinalNodeGroup = standardGroup(d.FreshSnapshot.NodeGroups, id)
}

// pickLowest returns the provider with the lowest primary metric, breaking ties
// on the secondary metric and then on ID, so the choice never depends on list order.
func pickLowest(ngs []brokerapi.NodeGroupView, primary, secondary func(brokerapi.NodeGroupView) *float64) string {
	sorted := append([]brokerapi.NodeGroupView(nil), ngs...)
	less := func(a, b *float64) (bool, bool) {
		switch {
		case a != nil && b == nil:
			return true, true
		case a == nil && b != nil:
			return false, true
		case a != nil && b != nil && *a != *b:
			return *a < *b, true
		}
		return false, false
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		if r, decided := less(primary(sorted[i]), primary(sorted[j])); decided {
			return r
		}
		if r, decided := less(secondary(sorted[i]), secondary(sorted[j])); decided {
			return r
		}
		return sorted[i].ProviderClusterID < sorted[j].ProviderClusterID
	})
	if len(sorted) == 0 || primary(sorted[0]) == nil {
		return ""
	}
	return sorted[0].ProviderClusterID
}

// standardGroup returns a copy of the provider's standard node group, if listed.
func standardGroup(ngs []brokerapi.NodeGroupView, providerID string) *brokerapi.NodeGroupView {
	for _, ng := range ngs {
		if ng.ProviderClusterID == providerID && ng.Type == brokerv1alpha1.ChunkTypeStandard {
			copied := ng
			return &copied
		}
	}
	return nil
}

// eligibleCandidates is the list handed to the model: every standard node
// group with free capacity, exactly as the Broker returned it.
func eligibleCandidates(resp *brokerapi.NodeGroupListResponse) []brokerapi.NodeGroupView {
	var out []brokerapi.NodeGroupView
	for _, ng := range testlib.GrowableNodeGroups(resp.NodeGroups) {
		if ng.Type == brokerv1alpha1.ChunkTypeStandard {
			out = append(out, ng)
		}
	}
	return out
}
