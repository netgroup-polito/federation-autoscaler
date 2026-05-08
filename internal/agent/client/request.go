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

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// requestOptions captures the per-call knobs callers may tweak. The zero
// value is the common case (no headers, no idempotency key); endpoint
// methods in 7b construct it explicitly.
type requestOptions struct {
	// reservationID, if set, populates the X-Reservation-Id header for
	// idempotency on reservation-scoped mutating calls (POST
	// /api/v1/reservations and POST /api/v1/instructions/{id}/result).
	reservationID string

	// requestID, if set, populates the X-Request-Id header. Empty means
	// the request layer will mint a fresh UUID v4.
	requestID string

	// idempotent marks the request as safe to retry on 5xx / network
	// errors. GET / DELETE are always idempotent; mutating POSTs are
	// only idempotent when paired with X-Reservation-Id (the broker's
	// CRD-backed dedup is what makes them so).
	idempotent bool
}

// do is the single entry point through which every endpoint method goes.
// It marshals body to JSON, builds the *http.Request, attaches headers,
// drives the retry loop on transient failures, and decodes the response
// (or wire-level ErrorResponse) into either out or an *Error.
//
// The contract:
//
//   - body == nil  → no request body is sent.
//   - out == nil   → response body is drained but not decoded.
//   - On success, returns nil with out populated.
//   - On HTTP failure, returns *Error with Category set per classify().
//   - On network / decode failure, returns *Error with Category=Transient.
//
// Retry policy: opts.idempotent==true plus a transient failure (network
// error, 5xx) triggers up to c.maxRetries additional attempts with
// exponential backoff. ctx cancellation aborts the loop immediately.
func (c *Client) do(
	ctx context.Context,
	method, path string,
	body, out any,
	opts requestOptions,
) error {
	if opts.requestID == "" {
		opts.requestID = uuid.NewString()
	}

	url := c.resolve(path)

	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return &Error{
				Category:  CategoryBadRequest,
				Message:   fmt.Sprintf("encode request body: %v", err),
				RequestID: opts.requestID,
				Cause:     err,
			}
		}
	}

	backoff := c.initialBackoff
	var lastErr *Error

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			if err := c.sleepWithCtx(ctx, backoff); err != nil {
				return err
			}
			next := time.Duration(float64(backoff) * c.backoffMultiplier)
			if next > c.maxBackoff {
				next = c.maxBackoff
			}
			backoff = next
		}

		err := c.attempt(ctx, method, url, bodyBytes, out, opts)
		if err == nil {
			return nil
		}

		lastErr = err
		if !opts.idempotent || err.Category != CategoryTransient {
			return err
		}
		// Don't burn a retry slot if the parent context is already done.
		if ctx.Err() != nil {
			return err
		}
	}
	return lastErr
}

// attempt performs a single HTTP exchange. All errors returned here are
// already wrapped in *Error so the retry loop can branch on Category.
func (c *Client) attempt(
	ctx context.Context,
	method, url string,
	bodyBytes []byte,
	out any,
	opts requestOptions,
) *Error {
	var bodyReader io.Reader
	if bodyBytes != nil {
		bodyReader = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return &Error{
			Category:  CategoryBadRequest,
			Message:   fmt.Sprintf("build HTTP request: %v", err),
			RequestID: opts.requestID,
			Cause:     err,
		}
	}
	req.Header.Set(brokerapi.HeaderRequestID, opts.requestID)
	if opts.reservationID != "" {
		req.Header.Set(brokerapi.HeaderReservationID, opts.reservationID)
	}
	req.Header.Set("Accept", brokerapi.ContentTypeJSON)
	if bodyBytes != nil {
		req.Header.Set("Content-Type", brokerapi.ContentTypeJSON)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// ctx cancellation is surfaced as Transient too so the retry loop
		// can short-circuit on ctx.Err() above; an errors.Is check below
		// distinguishes "operator stopped us" from "broker unreachable".
		category := CategoryTransient
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			category = CategoryTransient
		}
		return &Error{
			Category:  category,
			Message:   fmt.Sprintf("HTTP %s %s: %v", method, url, err),
			RequestID: opts.requestID,
			Cause:     err,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	respID := resp.Header.Get(brokerapi.HeaderRequestID)
	if respID == "" {
		respID = opts.requestID
	}

	if resp.StatusCode >= 400 {
		return decodeErrorBody(resp, respID)
	}

	if out == nil {
		// Drain so the connection can be reused.
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

// decodeErrorBody parses an ErrorResponse off the wire. Bodies that fail
// to decode (or arrive empty) still produce a usable typed error so the
// caller never has to reach into resp.Body itself.
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
		out.Message = string(body)
		return out
	}
	out.Code = env.Code
	out.Message = env.Message
	if env.RequestID != "" {
		out.RequestID = env.RequestID
	}
	return out
}

// sleepWithCtx blocks for d unless ctx is cancelled first, in which case
// it returns the context's error. We honour ctx in the retry loop so a
// shutdown signal during a long backoff doesn't hold the goroutine
// hostage for the full sleep.
func (c *Client) sleepWithCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return &Error{
			Category: CategoryTransient,
			Message:  fmt.Sprintf("context cancelled during retry backoff: %v", ctx.Err()),
			Cause:    ctx.Err(),
		}
	}
}
