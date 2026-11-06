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

// Package agentclient is the typed HTTP client the gRPC server uses to
// reach the co-located Consumer Agent's loopback REST API
// (internal/agent/consumer/localapi). The agent is the only egress
// from the gRPC server: every Cluster Autoscaler RPC flows through
// these methods and never touches the broker directly.
//
// The transport is plain HTTP — loopback only, no mTLS. The agent
// binds 127.0.0.1:9090 by default; production wires the URL through
// cmd/grpc-server's --agent-local-api-url flag.
package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/netgroup-polito/federation-autoscaler/api/autoscaling/v1alpha1"
	"github.com/netgroup-polito/federation-autoscaler/internal/agent/consumer/localapi"
	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// DefaultRequestTimeout bounds a single HTTP attempt. Loopback calls
// are fast; if the agent doesn't respond in 5 s something is wrong and
// the gRPC server should fail loudly rather than hang Cluster
// Autoscaler's loop.
const DefaultRequestTimeout = 5 * time.Second

// Options bundles the construction-time settings of a Client.
type Options struct {
	// BaseURL is the agent's loopback REST base URL — e.g.
	// "http://127.0.0.1:9090". Path is ignored. Required.
	BaseURL string

	// Transport overrides the underlying http.RoundTripper. Tests
	// inject httptest-aware transports; production leaves it nil for
	// http.DefaultTransport.
	Transport http.RoundTripper

	// RequestTimeout caps a single HTTP attempt. Defaults to
	// DefaultRequestTimeout.
	RequestTimeout time.Duration

	// Logger is the structured logger every request logs through.
	Logger logr.Logger
}

// Client is the gRPC server's typed view of the Consumer Agent's
// loopback REST API. Safe for concurrent use; cheap to construct.
type Client struct {
	baseURL *url.URL
	http    *http.Client
	log     logr.Logger
}

// New validates opts and returns a ready-to-use Client. Performs no
// network I/O.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("agentclient: BaseURL is required")
	}
	parsed, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("agentclient: parse BaseURL %q: %w", opts.BaseURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("agentclient: BaseURL must be http(s) (got scheme %q)", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("agentclient: BaseURL %q has no host", opts.BaseURL)
	}
	parsed = &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}

	logger := opts.Logger
	if logger.GetSink() == nil {
		logger = log.Log.WithName("grpc-agentclient")
	}
	timeout := opts.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	transport := opts.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{
		baseURL: parsed,
		http:    &http.Client{Transport: transport, Timeout: timeout},
		log:     logger,
	}, nil
}

// -----------------------------------------------------------------------------
// Endpoint methods
// -----------------------------------------------------------------------------

// GetNodeGroups proxies CA's view of available node groups through the
// agent (which proxies to the broker's GET /api/v1/nodegroups).
func (c *Client) GetNodeGroups(ctx context.Context) (*brokerapi.NodeGroupListResponse, error) {
	return c.GetNodeGroupsWithin(ctx, nil)
}

// GetNodeGroupsWithin is GetNodeGroups with a per-request latency bound (a
// ResourceRequest's spec.latencyBound), sent as query parameters. A nil bound
// is the plain GetNodeGroups. Only the agent's local brain applies the bound.
func (c *Client) GetNodeGroupsWithin(
	ctx context.Context, bound *autoscalingv1alpha1.LatencyBound,
) (*brokerapi.NodeGroupListResponse, error) {
	path := "/local/nodegroups"
	if q := localapi.EncodeLatencyBound(bound); q != nil {
		path += "?" + q.Encode()
	}
	out := &brokerapi.NodeGroupListResponse{}
	if err := c.do(ctx, http.MethodGet, path, nil, out, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// PostReservation requests a new reservation against the agent's
// loopback REST. reservationID is the X-Reservation-Id header value
// the agent propagates verbatim to the broker for CRD-backed
// idempotency. Empty reservationID is rejected — the gRPC server
// computes a deterministic key from CA's call params (substep 10d).
func (c *Client) PostReservation(
	ctx context.Context, reservationID string, req *brokerapi.ReservationRequest,
) (*brokerapi.ReservationResponse, error) {
	if reservationID == "" {
		return nil, errors.New("agentclient: PostReservation: reservationID is required")
	}
	if req == nil {
		return nil, errors.New("agentclient: PostReservation: req is nil")
	}
	out := &brokerapi.ReservationResponse{}
	if err := c.do(ctx, http.MethodPost, "/local/reservations", req, out, reservationID); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteReservation releases an existing reservation. v1 always
// releases all chunks; the agent translates this to the broker's
// DELETE /api/v1/reservations/{id}.
func (c *Client) DeleteReservation(ctx context.Context, reservationID string) (*brokerapi.ReleaseResponse, error) {
	if reservationID == "" {
		return nil, errors.New("agentclient: DeleteReservation: reservationID is required")
	}
	path := "/local/reservations/" + url.PathEscape(reservationID)
	out := &brokerapi.ReleaseResponse{}
	if err := c.do(ctx, http.MethodDelete, path, nil, out, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// GetVirtualNodes lists the consumer-cluster's VirtualNodeState CRs
// projected through the agent. Used by CA's read-mostly RPCs (substep
// 10c) to discover which virtual nodes belong to which node group.
func (c *Client) GetVirtualNodes(ctx context.Context) (*localapi.VirtualNodeListResponse, error) {
	out := &localapi.VirtualNodeListResponse{}
	if err := c.do(ctx, http.MethodGet, "/local/virtual-nodes", nil, out, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Internal helper
// -----------------------------------------------------------------------------

// do issues a single HTTP request against the agent. Marshals body to
// JSON, attaches a freshly-minted X-Request-Id (and optional
// reservationID as X-Reservation-Id), and decodes the response (or
// the broker-shaped ErrorResponse body for non-2xx).
func (c *Client) do(
	ctx context.Context,
	method, path string,
	body, out any,
	reservationID string,
) error {
	requestID := uuid.NewString()

	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return &Error{
				Category:  CategoryBadRequest,
				Message:   fmt.Sprintf("encode request body: %v", err),
				RequestID: requestID,
				Cause:     err,
			}
		}
		bodyReader = bytes.NewReader(buf)
	}

	u := *c.baseURL
	u.Path = path
	if p, query, ok := strings.Cut(path, "?"); ok {
		u.Path, u.RawQuery = p, query
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		return &Error{
			Category:  CategoryBadRequest,
			Message:   fmt.Sprintf("build HTTP request: %v", err),
			RequestID: requestID,
			Cause:     err,
		}
	}
	req.Header.Set(brokerapi.HeaderRequestID, requestID)
	if reservationID != "" {
		req.Header.Set(brokerapi.HeaderReservationID, reservationID)
	}
	req.Header.Set("Accept", brokerapi.ContentTypeJSON)
	if body != nil {
		req.Header.Set("Content-Type", brokerapi.ContentTypeJSON)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{
			Category:  CategoryTransient,
			Message:   fmt.Sprintf("HTTP %s %s: %v", method, u.String(), err),
			RequestID: requestID,
			Cause:     err,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	respID := resp.Header.Get(brokerapi.HeaderRequestID)
	if respID == "" {
		respID = requestID
	}

	if resp.StatusCode >= 400 {
		return decodeErrorBody(resp, respID)
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &Error{
			Category:  CategoryTransient,
			Status:    resp.StatusCode,
			Message:   fmt.Sprintf("decode response body: %v", err),
			RequestID: respID,
			Cause:     err,
		}
	}
	return nil
}

// decodeErrorBody parses an ErrorResponse off the wire. Empty bodies
// or decode failures still yield a usable typed Error so callers never
// have to reach into resp.Body themselves.
func decodeErrorBody(resp *http.Response, requestID string) *Error {
	out := &Error{
		Category:  classify(resp.StatusCode),
		Status:    resp.StatusCode,
		RequestID: requestID,
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		out.Message = http.StatusText(resp.StatusCode)
		return out
	}
	var env brokerapi.ErrorResponse
	if err := json.Unmarshal(body, &env); err != nil {
		out.Message = strings.TrimSpace(string(body))
		return out
	}
	out.Code = env.Code
	out.Message = env.Message
	if env.RequestID != "" {
		out.RequestID = env.RequestID
	}
	return out
}
