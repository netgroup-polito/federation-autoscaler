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

package testlib

import (
	"strings"
	"testing"
)

// The happy path is covered by any real run: the transition check passes on the
// first poll and nobody notices it. These tests cover the paths a run cannot
// reach on demand -- a leak that never clears, and a leak that clears late --
// because the whole point of the check is what it does when something is wrong,
// and that behaviour would otherwise ship unexercised.

func cap32(total int32, held map[string]int32, ids ...string) FederationCapacity {
	if held == nil {
		held = map[string]int32{}
	}
	return FederationCapacity{TotalReserved: total, ReservedBy: held, NodeGroupIDs: ids}
}

func TestFederationCapacityMatches(t *testing.T) {
	want := cap32(0, nil, "ng-a", "ng-b")

	tests := []struct {
		name string
		got  FederationCapacity
		ok   bool
	}{
		{"identical", cap32(0, nil, "ng-a", "ng-b"), true},
		{"chunk still held", cap32(1, map[string]int32{"ng-a": 1}, "ng-a", "ng-b"), false},
		{"node group vanished", cap32(0, nil, "ng-a"), false},
		// A late-registering or briefly-stale-then-recovered provider adds a
		// node group that was not in the baseline. That is capacity arriving,
		// not capacity lost, so it must not fail the check.
		{"node group appeared", cap32(0, nil, "ng-a", "ng-b", "ng-c"), true},
		// A provider going stale takes its reserved chunks out of the sum with
		// it, so the total alone would read as clean. This is the case the
		// node-group comparison exists for.
		{"stale provider hides its own leak", cap32(0, nil, "ng-b"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.got.Matches(want); got != tc.ok {
				t.Errorf("Matches() = %v, want %v", got, tc.ok)
			}
		})
	}
}

func TestFederationCapacityMatchesAllowsExtraNodeGroupsWithZeroTotal(t *testing.T) {
	// The specific scenario Matches must not abort on: a provider shows up
	// after the baseline snapshot (or recovers from a brief stale window) and
	// nothing is actually reserved anywhere.
	want := cap32(0, nil, "ng-a")
	got := cap32(0, nil, "ng-a", "ng-new")
	if !got.Matches(want) {
		t.Errorf("Matches() = false for an extra empty node group, want true (it holds nothing, so nothing was lost)")
	}
}

func TestFederationCapacityMatchesStillCatchesRealLeakAlongsideNewArrival(t *testing.T) {
	// A new provider showing up must not mask an unrelated leak elsewhere.
	want := cap32(0, nil, "ng-a", "ng-b")
	got := cap32(1, map[string]int32{"ng-a": 1}, "ng-a", "ng-b", "ng-new")
	if got.Matches(want) {
		t.Errorf("Matches() = true, want false: ng-a still holds a chunk baseline did not")
	}
}

func TestFederationCapacityDiffNamesCulprits(t *testing.T) {
	want := cap32(0, nil, "ng-a", "ng-b", "ng-c")
	got := cap32(3, map[string]int32{"ng-a": 1, "ng-c": 2}, "ng-a", "ng-b", "ng-c")

	diff := got.Diff(want)
	// An operator reading this after a two-hour run needs to know which
	// providers are short and by how much, not just that the total is wrong.
	for _, want := range []string{"reserved chunks 3, want 0", "ng-a=1", "ng-c=2"} {
		if !strings.Contains(diff, want) {
			t.Errorf("Diff() = %q, missing %q", diff, want)
		}
	}
	if strings.Contains(diff, "ng-b") {
		t.Errorf("Diff() = %q, should not blame ng-b, which holds nothing", diff)
	}
}

func TestFederationCapacityDiffReportsMissingNodeGroup(t *testing.T) {
	want := cap32(0, nil, "ng-a", "ng-b")
	got := cap32(0, nil, "ng-a")

	diff := got.Diff(want)
	if !strings.Contains(diff, "node groups gone: ng-b") {
		t.Errorf("Diff() = %q, want it to name the missing node group", diff)
	}
}

func TestFederationCapacityDiffOnEqual(t *testing.T) {
	want := cap32(0, nil, "ng-a")
	if diff := want.Diff(want); diff != "no difference" {
		t.Errorf("Diff() = %q, want %q", diff, "no difference")
	}
}
