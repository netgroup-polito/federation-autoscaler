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

package brokers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// fakeBroker records what a consumer agent sent it.
type fakeBroker struct {
	srv *httptest.Server

	mu         sync.Mutex
	calls      []string // "METHOD path", in order
	bookings   []string
	heartbeats int

	// loseReply: a booking is made but the reply is lost (502).
	loseReply bool
	// noBooking: the broker never answers a booking and makes none (502).
	noBooking bool
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	fb := &fakeBroker{}
	fb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		fb.calls = append(fb.calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/heartbeat":
			fb.heartbeats++
			_ = json.NewEncoder(w).Encode(brokerapi.HeartbeatResponse{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/reservations":
			id := r.Header.Get(brokerapi.HeaderReservationID)
			if fb.noBooking {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			fb.bookings = append(fb.bookings, id)
			if fb.loseReply {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(brokerapi.ReservationResponse{ReservationID: id})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/reservations/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/reservations/")
			for _, b := range fb.bookings {
				if b == id {
					_ = json.NewEncoder(w).Encode(brokerapi.ReservationResponse{ReservationID: id})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(brokerapi.ErrorResponse{Code: brokerapi.ErrCodeNotFound, Message: "no such reservation"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/reservations":
			out := brokerapi.ReservationListResponse{Reservations: []brokerapi.ReservationResponse{}}
			for _, id := range fb.bookings {
				out.Reservations = append(out.Reservations, brokerapi.ReservationResponse{ReservationID: id})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/reservations/"):
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(brokerapi.ReleaseResponse{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fb.srv.Close)
	return fb
}

// url is the broker's address as an agent would know it.
func (fb *fakeBroker) url() string {
	u, _ := url.Parse(fb.srv.URL)
	return "https://" + u.Host
}

func (fb *fakeBroker) called(call string) int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	n := 0
	for _, c := range fb.calls {
		if c == call {
			n++
		}
	}
	return n
}

// rewrite sends the agent's https request to the plain-http test server.
type rewrite struct {
	base   http.RoundTripper
	target *url.URL
}

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = r.target.Scheme
	req.URL.Host = r.target.Host
	return r.base.RoundTrip(req)
}

// clientsFor builds agent clients for the fake brokers, keyed by URL.
func clientsFor(t *testing.T, fbs ...*fakeBroker) func(string) (*agentclient.Client, error) {
	t.Helper()
	byURL := map[string]*fakeBroker{}
	for _, fb := range fbs {
		byURL[fb.url()] = fb
	}
	return func(u string) (*agentclient.Client, error) {
		fb := byURL[u]
		target, _ := url.Parse(fb.srv.URL)
		return agentclient.New(agentclient.Options{
			BrokerURL:  u,
			Transport:  rewrite{base: fb.srv.Client().Transport, target: target},
			MaxRetries: 1,
		})
	}
}

// memStore keeps the saved brokers in memory.
type memStore struct{ urls []string }

func (m *memStore) Load(context.Context) ([]string, error) { return m.urls, nil }
func (m *memStore) Save(_ context.Context, urls []string) error {
	m.urls = append([]string(nil), urls...)
	return nil
}

func newPool(t *testing.T, home *fakeBroker, store Store, others ...*fakeBroker) *Pool {
	t.Helper()
	newClient := clientsFor(t, append([]*fakeBroker{home}, others...)...)
	h, err := newClient(home.url())
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(context.Background(), Options{
		HomeURL: home.url(), Home: h, NewClient: newClient, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBookingGoesToTheProvidersBroker(t *testing.T) {
	home, region := newFakeBroker(t), newFakeBroker(t)
	store := &memStore{}
	p := newPool(t, home, store, region)
	ctx := context.Background()

	if _, err := p.PostHeartbeat(ctx, &brokerapi.HeartbeatRequest{ClusterID: "consumer-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PostReservation(ctx, "res-home", &brokerapi.ReservationRequest{ProviderClusterID: "provider-1"}); err != nil {
		t.Fatal(err)
	}
	p.SetBookingEndpoint("piemonte", region.url()+"/")
	if _, err := p.PostReservation(ctx, "res-region", &brokerapi.ReservationRequest{ProviderClusterID: "piemonte"}); err != nil {
		t.Fatal(err)
	}

	if home.called("POST /api/v1/reservations") != 1 || region.called("POST /api/v1/reservations") != 1 {
		t.Fatalf("bookings: home %v, region %v", home.calls, region.calls)
	}
	// The new region broker heard from the consumer before its first booking.
	if region.calls[0] != "POST /api/v1/heartbeat" {
		t.Fatalf("region calls = %v; want a heartbeat first", region.calls)
	}
	if len(store.urls) != 1 || store.urls[0] != region.url() {
		t.Fatalf("saved brokers = %v; want the region", store.urls)
	}
	if got := len(p.Clients()); got != 2 {
		t.Fatalf("Clients() = %d brokers; want 2", got)
	}

	// Heartbeats now reach both brokers.
	if _, err := p.PostHeartbeat(ctx, &brokerapi.HeartbeatRequest{ClusterID: "consumer-1"}); err != nil {
		t.Fatal(err)
	}
	if home.heartbeats != 2 || region.heartbeats != 2 {
		t.Fatalf("heartbeats: home %d, region %d; want 2 and 2", home.heartbeats, region.heartbeats)
	}

	// A release goes to the broker holding the booking.
	if _, err := p.DeleteReservation(ctx, "res-region"); err != nil {
		t.Fatal(err)
	}
	if region.called("DELETE /api/v1/reservations/res-region") != 1 ||
		home.called("DELETE /api/v1/reservations/res-region") != 0 {
		t.Fatalf("release: home %v, region %v", home.calls, region.calls)
	}
}

func TestARestartedAgentFindsItsBrokersAndBookings(t *testing.T) {
	home, region := newFakeBroker(t), newFakeBroker(t)
	store := &memStore{}
	ctx := context.Background()

	before := newPool(t, home, store, region)
	before.SetBookingEndpoint("piemonte", region.url())
	if _, err := before.PostReservation(ctx, "res-region", &brokerapi.ReservationRequest{ProviderClusterID: "piemonte"}); err != nil {
		t.Fatal(err)
	}

	// After a restart: the saved region broker is back, and the release finds
	// the broker holding the booking without being told.
	after := newPool(t, home, store, region)
	if got := after.URLs(); len(got) != 2 || got[1] != region.url() {
		t.Fatalf("URLs after restart = %v; want home and region", got)
	}
	if _, err := after.DeleteReservation(ctx, "res-region"); err != nil {
		t.Fatal(err)
	}
	if region.called("DELETE /api/v1/reservations/res-region") != 1 {
		t.Fatalf("region calls = %v; want the release there", region.calls)
	}

	list, err := after.ListReservations(ctx)
	if err != nil || len(list.Reservations) != 1 {
		t.Fatalf("ListReservations = %v, %v; want the region's one booking", list, err)
	}
}

func TestALostReplyIsLookedUpNotTakenAsARefusal(t *testing.T) {
	home := newFakeBroker(t)
	home.loseReply = true // the booking is made, the reply never arrives
	p := newPool(t, home, nil)

	resp, err := p.PostReservation(context.Background(), "res-lost", &brokerapi.ReservationRequest{ProviderClusterID: "provider-1"})
	if err != nil || resp.ReservationID != "res-lost" {
		t.Fatalf("PostReservation = %v, %v; want the booking the broker holds", resp, err)
	}
	if home.called("GET /api/v1/reservations/res-lost") != 1 {
		t.Fatalf("calls = %v; want a status lookup", home.calls)
	}
	if n := len(home.bookings); n != 1 {
		// The client retried with the same ID; the fake keeps each attempt,
		// a real broker answers a retry with the same booking.
		t.Logf("booking attempts with the same ID: %d", n)
	}
}

func TestNoAnswerAndNoBookingIsReportedAsNotMade(t *testing.T) {
	home := newFakeBroker(t)
	home.noBooking = true
	p := newPool(t, home, nil)

	_, err := p.PostReservation(context.Background(), "res-none", &brokerapi.ReservationRequest{ProviderClusterID: "provider-1"})
	if err == nil || !strings.Contains(err.Error(), "was not made") {
		t.Fatalf("err = %v; want \"was not made\"", err)
	}
}

func TestARefusalIsNotLookedUp(t *testing.T) {
	home := newFakeBroker(t)
	// A refusal is an answer: the broker replies 409 with a reason.
	home.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		home.mu.Lock()
		home.calls = append(home.calls, r.Method+" "+r.URL.Path)
		home.mu.Unlock()
		w.Header().Set("Content-Type", brokerapi.ContentTypeJSON)
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(brokerapi.ErrorResponse{Code: brokerapi.ErrCodeLatencyLimitNotMet, Message: "too far"})
	})
	p := newPool(t, home, nil)

	_, err := p.PostReservation(context.Background(), "res-far", &brokerapi.ReservationRequest{ProviderClusterID: "provider-1"})
	if err == nil || !strings.Contains(err.Error(), string(brokerapi.ErrCodeLatencyLimitNotMet)) {
		t.Fatalf("err = %v; want the refusal reason", err)
	}
	if home.called("GET /api/v1/reservations/res-far") != 0 {
		t.Fatalf("calls = %v; a refusal must not be looked up", home.calls)
	}
}
