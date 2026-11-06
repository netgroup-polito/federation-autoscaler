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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	brokerv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/broker/v1alpha1"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

const testNS = "federation-autoscaler-system"

// fakeLister serves a fixed reservation list (or an error).
type fakeLister struct {
	list []brokerapi.ReservationResponse
	err  error
}

func (f fakeLister) ListReservations(context.Context) (*brokerapi.ReservationListResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &brokerapi.ReservationListResponse{Reservations: f.list}, nil
}

// failingCatalog is a Reader that always fails.
type failingCatalog struct{}

func (failingCatalog) GetOffer(context.Context, string, string) (Offer, error) {
	return Offer{}, errors.New("catalog down")
}

func (failingCatalog) ListOffers(context.Context) ([]Offer, error) {
	return nil, errors.New("catalog down")
}

func kubeWith(t *testing.T, objs ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := autoscalingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func consumerPolicy(name string, spec autoscalingv1alpha1.ConsumerPolicySpec) *autoscalingv1alpha1.ConsumerPolicy {
	return &autoscalingv1alpha1.ConsumerPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec:       spec,
	}
}

func catalogWith(t *testing.T, offers ...Offer) *MemoryCatalog {
	t.Helper()
	c := NewMemoryCatalog()
	for _, o := range offers {
		if err := c.UpsertOffer(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestBrainDecidesFromCatalogReservationsAndPolicy(t *testing.T) {
	// Price, bounded to 3 ms from Asti. p-rome is the cheapest but out of the
	// bound; p-alba is the cheapest within it; this consumer already holds one
	// chunk on p-asti.
	cat := catalogWith(t,
		withPrice(located(siteOffer("p-rome", "rome", 6, 12), romeLat, romeLon), "0.01"),
		withPrice(located(siteOffer("p-alba", "alba", 6, 12), albaLat, albaLon), "0.03"),
		withPrice(located(siteOffer("p-asti", "asti", 6, 12), astiLat, astiLon), "0.05"),
	)
	pol := consumerPolicy("default", autoscalingv1alpha1.ConsumerPolicySpec{
		Placement:    autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyPrice},
		LatencyBound: boundFromAsti(3),
	})
	b, err := New(Options{
		Catalog:      cat,
		Reservations: fakeLister{list: []brokerapi.ReservationResponse{resv("r1", "p-asti", brokerv1alpha1.ReservationPhasePeered)}},
		LocalClient:  kubeWith(t, pol),
		Namespace:    testNS,
		Location:     func() (float64, float64, bool) { return turinLat, turinLon, true },
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := b.NodeGroups(context.Background())
	if err != nil {
		t.Fatalf("NodeGroups: %v", err)
	}
	if resp.AppliedPlacement != autoscalingv1alpha1.PlacementStrategyPrice {
		t.Errorf("policy not read: applied = %q", resp.AppliedPlacement)
	}
	if got := growable(*resp); len(got) != 1 || got[0] != "p-alba" {
		t.Errorf("growable = %v, want [p-alba] (cheapest within the bound)", got)
	}
	views := viewsByProvider(*resp)
	if _, listed := views["p-rome"]; listed {
		t.Error("p-rome is outside the bound and holds nothing of ours: it must not be listed")
	}
	// Our own chunk is counted; p-asti is not Price's pick, so it is masked.
	if v := views["p-asti"]; v.CurrentReserved != 1 || v.MaxSize != 1 {
		t.Errorf("p-asti CurrentReserved/MaxSize = %d/%d, want 1/1 (own chunk, masked)", v.CurrentReserved, v.MaxSize)
	}

	// The Broker refuses p-alba (another consumer took the last chunk first):
	// the next decision moves on to p-asti.
	b.ObserveReservation("p-alba", noCapacityErr())
	resp, err = b.NodeGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := growable(*resp); len(got) != 1 || got[0] != "p-asti" {
		t.Errorf("after the refusal: growable = %v, want [p-asti]", got)
	}
	if v := viewsByProvider(*resp)["p-asti"]; v.CurrentReserved != 1 || v.MaxSize != 4 {
		t.Errorf("p-asti CurrentReserved/MaxSize = %d/%d, want 1/4 (own + 3 free)", v.CurrentReserved, v.MaxSize)
	}
}

func TestBrainWithoutPolicyIsStandard(t *testing.T) {
	green := siteOffer("p-green", "default", 6, 12)
	green.Renewable = true
	b, _ := New(Options{
		Catalog:      catalogWith(t, siteOffer("p-a", "default", 6, 12), green),
		Reservations: fakeLister{},
		LocalClient:  kubeWith(t),
		Namespace:    testNS,
	})
	resp, err := b.NodeGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resp.AppliedPlacement != "" {
		t.Errorf("no ConsumerPolicy means Standard, got %q", resp.AppliedPlacement)
	}
	if got := growable(*resp); len(got) != 1 || got[0] != "p-green" {
		t.Errorf("growable = %v, want [p-green]", got)
	}
}

func TestBrainLowestNamedPolicyWins(t *testing.T) {
	b, _ := New(Options{
		Catalog:      catalogWith(t, siteOffer("p1", "default", 6, 12)),
		Reservations: fakeLister{},
		LocalClient: kubeWith(t,
			consumerPolicy("zz-other", autoscalingv1alpha1.ConsumerPolicySpec{
				Placement: autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyEco},
			}),
			consumerPolicy("aa-main", autoscalingv1alpha1.ConsumerPolicySpec{
				Placement: autoscalingv1alpha1.PlacementPolicy{Type: autoscalingv1alpha1.PlacementStrategyRandom},
			}),
		),
		Namespace: testNS,
	})
	resp, err := b.NodeGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resp.AppliedPlacement != autoscalingv1alpha1.PlacementStrategyRandom {
		t.Errorf("applied = %q, want Random (from aa-main)", resp.AppliedPlacement)
	}
}

func TestBrainErrors(t *testing.T) {
	kube := kubeWith(t)
	b, _ := New(Options{Catalog: failingCatalog{}, Reservations: fakeLister{}, LocalClient: kube})
	if _, err := b.NodeGroups(context.Background()); err == nil {
		t.Error("a catalog failure must be reported")
	}
	b, _ = New(Options{Catalog: NewMemoryCatalog(), Reservations: fakeLister{err: errors.New("broker down")}, LocalClient: kube})
	if _, err := b.NodeGroups(context.Background()); err == nil {
		t.Error("a reservation-list failure must be reported")
	}
}

func TestNewValidates(t *testing.T) {
	kube := kubeWith(t)
	for name, opts := range map[string]Options{
		"no catalog":      {Reservations: fakeLister{}, LocalClient: kube},
		"no reservations": {Catalog: NewMemoryCatalog(), LocalClient: kube},
		"no local client": {Catalog: NewMemoryCatalog(), Reservations: fakeLister{}},
	} {
		if _, err := New(opts); err == nil {
			t.Errorf("%s: New must fail", name)
		}
	}
}
