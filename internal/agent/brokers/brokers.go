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

// Package brokers lets one consumer agent work with several brokers: its home
// broker (--broker-url, the central broker in M1 and M2) and every region
// orchestrator it has booked at (M3). The Pool has the same methods as the
// single broker client the agent used before, so the heartbeat, the local API
// and the brain use it unchanged:
//
//   - heartbeats go to every broker (a broker refuses bookings from a consumer
//     it has not heard from), and a new broker gets one before its first
//     booking;
//   - a booking goes to the provider's booking address when known (from its
//     catalog offer), otherwise to the home broker;
//   - a release goes to the broker that holds the booking;
//   - the node groups come from the home broker;
//   - the reservations list merges every broker's.
//
// The brokers it has learned are saved (Store), so a restarted agent still
// polls them and can release its bookings there.
package brokers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/go-logr/logr"

	agentclient "github.com/netgroup-polito/federation-autoscaler/internal/agent/client"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// Store keeps the URLs of the brokers learned at run time across restarts.
type Store interface {
	Load(ctx context.Context) ([]string, error)
	Save(ctx context.Context, urls []string) error
}

// Options configures a Pool.
type Options struct {
	// HomeURL and Home are the home broker. Required.
	HomeURL string
	Home    *agentclient.Client

	// NewClient builds a client for another broker (same identity and CA).
	// Nil: the Pool stays with the home broker only.
	NewClient func(url string) (*agentclient.Client, error)

	// Store saves the learned brokers. Nil: they are kept in memory only.
	Store Store

	Logger logr.Logger
}

// Pool is the set of brokers one consumer agent talks to.
type Pool struct {
	homeURL   string
	newClient func(url string) (*agentclient.Client, error)
	store     Store
	log       logr.Logger

	mu        sync.Mutex
	urls      []string // home first, then learned brokers in order
	clients   map[string]*agentclient.Client
	endpoints map[string]string // provider ID -> booking address
	owners    map[string]string // reservation ID -> broker URL
	heartbeat *brokerapi.HeartbeatRequest
}

// New builds a Pool and reconnects to the brokers saved in the Store.
func New(ctx context.Context, opts Options) (*Pool, error) {
	if opts.Home == nil || opts.HomeURL == "" {
		return nil, errors.New("brokers: the home broker is required")
	}
	logger := opts.Logger
	if logger.GetSink() == nil {
		logger = logr.Discard()
	}
	home := Normalize(opts.HomeURL)
	p := &Pool{
		homeURL:   home,
		newClient: opts.NewClient,
		store:     opts.Store,
		log:       logger,
		urls:      []string{home},
		clients:   map[string]*agentclient.Client{home: opts.Home},
		endpoints: map[string]string{},
		owners:    map[string]string{},
	}
	if p.store != nil && p.newClient != nil {
		saved, err := p.store.Load(ctx)
		if err != nil {
			logger.Info("could not load the saved brokers; starting with the home broker only", "err", err.Error())
		}
		for _, url := range saved {
			url = Normalize(url)
			if url == "" || p.clients[url] != nil {
				continue
			}
			c, err := p.newClient(url)
			if err != nil {
				logger.Info("skipping a saved broker", "url", url, "err", err.Error())
				continue
			}
			p.clients[url] = c
			p.urls = append(p.urls, url)
		}
	}
	return p, nil
}

// Normalize trims spaces and a trailing slash, so one broker has one key.
func Normalize(url string) string {
	return strings.TrimRight(strings.TrimSpace(url), "/")
}

// Clients returns every broker's client, the home broker first.
func (p *Pool) Clients() []*agentclient.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*agentclient.Client, 0, len(p.urls))
	for _, url := range p.urls {
		out = append(out, p.clients[url])
	}
	return out
}

// URLs returns every broker's URL, the home broker first.
func (p *Pool) URLs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.urls...)
}

// SetBookingEndpoint records where a provider takes bookings (its catalog
// offer's bookingEndpoint). An empty address clears it: the home broker.
func (p *Pool) SetBookingEndpoint(providerID, url string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	url = Normalize(url)
	if url == "" || url == p.homeURL {
		delete(p.endpoints, providerID)
		return
	}
	p.endpoints[providerID] = url
}

// PostHeartbeat sends the heartbeat to every broker. The home broker's answer
// is returned; another broker's failure is only logged.
func (p *Pool) PostHeartbeat(
	ctx context.Context, req *brokerapi.HeartbeatRequest,
) (*brokerapi.HeartbeatResponse, error) {
	p.mu.Lock()
	p.heartbeat = req
	p.mu.Unlock()

	var (
		homeResp *brokerapi.HeartbeatResponse
		homeErr  error
	)
	for i, c := range p.Clients() {
		resp, err := c.PostHeartbeat(ctx, req)
		if i == 0 {
			homeResp, homeErr = resp, err
			continue
		}
		if err != nil && ctx.Err() == nil {
			p.log.V(1).Info("heartbeat to a region broker failed", "err", err.Error())
		}
	}
	return homeResp, homeErr
}

// GetNodeGroups asks the home broker.
func (p *Pool) GetNodeGroups(ctx context.Context) (*brokerapi.NodeGroupListResponse, error) {
	return p.home().GetNodeGroups(ctx)
}

// PostReservation books at the provider's booking address when known,
// otherwise at the home broker, and remembers which broker holds the booking.
func (p *Pool) PostReservation(
	ctx context.Context, reservationID string, req *brokerapi.ReservationRequest,
) (*brokerapi.ReservationResponse, error) {
	p.mu.Lock()
	url, ok := p.endpoints[req.ProviderClusterID]
	p.mu.Unlock()
	if !ok {
		url = p.homeURL
	}
	c, err := p.client(ctx, url)
	if err != nil {
		return nil, err
	}
	resp, err := c.PostReservation(ctx, reservationID, req)
	if err != nil && noAnswer(err) {
		// Silence is not a refusal: the booking may have succeeded and only
		// the reply was lost. The client already retried with the same ID;
		// now ask the broker before reporting a failure, so the caller never
		// books elsewhere while this one may hold room.
		resp, err = p.lookUp(ctx, c, reservationID, err)
	}
	if err == nil {
		p.mu.Lock()
		p.owners[reservationID] = url
		p.mu.Unlock()
	}
	return resp, err
}

// lookUp asks the broker for a booking whose reply was lost: it is returned
// when the broker has it; otherwise the original error stands, said to be
// final (not booked) or unknown (the broker could not be asked either).
func (p *Pool) lookUp(
	ctx context.Context, c *agentclient.Client, reservationID string, original error,
) (*brokerapi.ReservationResponse, error) {
	resp, err := c.GetReservation(ctx, reservationID)
	switch {
	case err == nil:
		p.log.Info("a booking's reply was lost, but the broker has it", "reservationId", reservationID)
		return resp, nil
	case agentclient.IsNotFound(err):
		return nil, fmt.Errorf("booking %s was not made (no answer, and the broker does not have it): %w",
			reservationID, original)
	default:
		return nil, fmt.Errorf("booking %s: no answer, and its status is unknown (%v); retry with the same ID: %w",
			reservationID, err, original)
	}
}

// noAnswer reports whether a broker call got no real answer: no HTTP
// response, or a gateway error or timeout in front of the broker. A refusal
// (any answer with a reason, 4xx or 503) is not a lost reply.
func noAnswer(err error) bool {
	var ce *agentclient.Error
	if !errors.As(err, &ce) {
		return true
	}
	switch ce.Status {
	case 0, http.StatusBadGateway, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// DeleteReservation releases a booking at the broker that holds it.
func (p *Pool) DeleteReservation(
	ctx context.Context, reservationID string,
) (*brokerapi.ReleaseResponse, error) {
	return p.owner(ctx, reservationID).DeleteReservation(ctx, reservationID)
}

// ListReservations merges this consumer's reservations at every broker. The
// home broker's failure is returned; another broker's is only logged.
func (p *Pool) ListReservations(ctx context.Context) (*brokerapi.ReservationListResponse, error) {
	out := &brokerapi.ReservationListResponse{Reservations: []brokerapi.ReservationResponse{}}
	urls := p.URLs()
	for i, url := range urls {
		resp, err := p.clientFor(url).ListReservations(ctx)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			p.log.V(1).Info("listing reservations at a region broker failed", "url", url, "err", err.Error())
			continue
		}
		p.mu.Lock()
		for _, r := range resp.Reservations {
			p.owners[r.ReservationID] = url
		}
		p.mu.Unlock()
		out.Reservations = append(out.Reservations, resp.Reservations...)
	}
	return out, nil
}

// owner returns the client of the broker holding a reservation: from memory,
// else by asking every broker (after a restart), else the home broker.
func (p *Pool) owner(ctx context.Context, reservationID string) *agentclient.Client {
	p.mu.Lock()
	url, ok := p.owners[reservationID]
	p.mu.Unlock()
	if ok {
		return p.clientFor(url)
	}
	if len(p.URLs()) > 1 {
		if _, err := p.ListReservations(ctx); err == nil {
			p.mu.Lock()
			url, ok = p.owners[reservationID]
			p.mu.Unlock()
			if ok {
				return p.clientFor(url)
			}
		}
	}
	return p.home()
}

// client returns the client for url, adding the broker to the Pool (saved,
// and sent the last heartbeat) when it is new.
func (p *Pool) client(ctx context.Context, url string) (*agentclient.Client, error) {
	p.mu.Lock()
	if c := p.clients[url]; c != nil {
		p.mu.Unlock()
		return c, nil
	}
	if p.newClient == nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("brokers: no client can be built for %s", url)
	}
	c, err := p.newClient(url)
	if err != nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("brokers: build a client for %s: %w", url, err)
	}
	p.clients[url] = c
	p.urls = append(p.urls, url)
	urls := append([]string(nil), p.urls[1:]...)
	hb := p.heartbeat
	p.mu.Unlock()

	p.log.Info("talking to a new broker", "url", url)
	if p.store != nil {
		if err := p.store.Save(ctx, urls); err != nil {
			p.log.Info("could not save the brokers list", "err", err.Error())
		}
	}
	// A broker refuses bookings from a consumer it has never heard from.
	if hb != nil {
		if _, err := c.PostHeartbeat(ctx, hb); err != nil {
			p.log.Info("first heartbeat to a new broker failed", "url", url, "err", err.Error())
		}
	}
	return c, nil
}

// clientFor returns a known broker's client, or the home broker's.
func (p *Pool) clientFor(url string) *agentclient.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.clients[url]; c != nil {
		return c
	}
	return p.clients[p.homeURL]
}

func (p *Pool) home() *agentclient.Client { return p.clientFor(p.homeURL) }
