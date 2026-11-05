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
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

func newTestFull() (*FullProviders, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
	f := NewFullProviders()
	f.Now = clk.now
	return f, clk
}

func noCapacityErr() error {
	return fmt.Errorf("reserve on p1: %w", &client.Error{
		Category: client.CategoryConflict, Status: http.StatusConflict,
		Code: brokerapi.ErrCodeInsufficientCapacity, Message: "no free chunk",
	})
}

func TestFullProvidersExpire(t *testing.T) {
	f, clk := newTestFull()
	f.Mark("p1")
	if !f.Snapshot()["p1"] {
		t.Fatal("a freshly marked provider must be full")
	}
	clk.t = clk.t.Add(FullCooldown - time.Second)
	if !f.Snapshot()["p1"] {
		t.Error("still full just before FullCooldown")
	}
	clk.t = clk.t.Add(time.Second)
	if f.Snapshot()["p1"] {
		t.Error("the mark must expire after FullCooldown")
	}
}

func TestFullProvidersOnlyOnNoCapacity(t *testing.T) {
	f, _ := newTestFull()
	idempotencyClash := &client.Error{
		Category: client.CategoryConflict, Status: http.StatusConflict, Code: brokerapi.ErrCodeConflict,
	}
	for name, err := range map[string]error{
		"nil":                  nil,
		"network":              errors.New("connection reset"),
		"idempotency conflict": idempotencyClash,
	} {
		if f.MarkIfNoCapacity("p-"+name, err) {
			t.Errorf("%s must not mark the provider full", name)
		}
	}
	if !f.MarkIfNoCapacity("p1", noCapacityErr()) {
		t.Error("an InsufficientCapacity refusal must mark the provider full")
	}
	if got := f.Snapshot(); len(got) != 1 || !got["p1"] {
		t.Errorf("full = %v, want only p1", got)
	}
}

func TestNodeGroupsSkipsFullProvider(t *testing.T) {
	// Price picks the cheapest; once the Broker says it is full, the next
	// decision exposes the next-cheapest instead.
	offers := []Offer{
		withPrice(siteOffer("p-cheap", "default", 6, 12), "0.01"),
		withPrice(siteOffer("p-mid", "default", 6, 12), "0.05"),
	}
	price := policy(autoscalingv1alpha1.PlacementStrategyPrice)

	before := NodeGroups(Input{Offers: offers, Consumer: price})
	if got := growable(before); len(got) != 1 || got[0] != "p-cheap" {
		t.Fatalf("before the refusal: growable = %v, want [p-cheap]", got)
	}

	f, _ := newTestFull()
	f.MarkIfNoCapacity("p-cheap", noCapacityErr())
	after := NodeGroups(Input{
		Offers:      offers,
		Consumer:    price,
		Full:        f.Snapshot(),
		OwnReserved: map[string]int32{"p-cheap": 1},
	})
	if got := growable(after); len(got) != 1 || got[0] != "p-mid" {
		t.Errorf("after the refusal: growable = %v, want [p-mid]", got)
	}
	cheap := viewsByProvider(after)["p-cheap"]
	if cheap.MaxSize != 1 || cheap.CurrentReserved != 1 {
		t.Errorf("full provider stays listed with our chunk only: MaxSize/CurrentReserved = %d/%d, want 1/1",
			cheap.MaxSize, cheap.CurrentReserved)
	}
}
