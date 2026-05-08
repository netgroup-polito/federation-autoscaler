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
	"errors"
	"fmt"
	"net/http"

	brokerapi "github.com/netgroup-polito/federation-autoscaler/internal/broker/api"
)

// Category groups HTTP failures into stable, machine-readable buckets so
// callers can branch without parsing strings. The mapping below is the
// only place the HTTP status / ErrorCode → category translation lives —
// keep the wire-side enum in internal/broker/api/types.go and this
// switch in lock-step.
type Category int

const (
	// CategoryUnknown is the zero value; never returned by classify.
	CategoryUnknown Category = iota

	// CategoryTransient covers 5xx server errors and underlying network /
	// timeout failures — anything the request layer should retry with
	// exponential backoff before surfacing.
	CategoryTransient

	// CategoryBadRequest covers 400 — the request is malformed or
	// violates wire-level invariants. Retrying is pointless.
	CategoryBadRequest

	// CategoryUnauthenticated covers 401 — mTLS handshake or CN-derived
	// identity rejection. Retrying with the same credentials is pointless.
	CategoryUnauthenticated

	// CategoryForbidden covers 403 — the cluster identity is known but
	// not authorised for the requested operation.
	CategoryForbidden

	// CategoryNotFound covers 404 — typically a stale instruction id or
	// a missing reservation.
	CategoryNotFound

	// CategoryConflict covers 409 — idempotency-key reuse with a
	// different payload, or a precondition that another writer already
	// claimed.
	CategoryConflict

	// CategoryPreconditionFailed covers 412 — typically a missing
	// heartbeat prerequisite on POST /api/v1/reservations.
	CategoryPreconditionFailed

	// CategoryTooManyRequests covers 429 — per-cluster rate limiting.
	// Retrying after Retry-After is appropriate; the request layer does
	// not retry these automatically because the right backoff is
	// caller-policy.
	CategoryTooManyRequests
)

// Error is the typed shape returned by the request layer for every non-2xx
// HTTP response. Network errors are wrapped in an Error with
// Category=CategoryTransient and Status=0 so callers have a single error
// type to switch on.
type Error struct {
	Category  Category
	Code      brokerapi.ErrorCode
	Message   string
	Status    int    // HTTP status code; 0 for pre-response failures.
	RequestID string // mirrors the X-Request-Id header.

	// Cause carries the underlying network or decode error when one
	// exists; nil for clean HTTP-layer rejections.
	Cause error
}

// Error formats the typed error in a way that is unambiguous in logs and
// stable enough for tests to assert against substrings.
func (e *Error) Error() string {
	switch {
	case e == nil:
		return ""
	case e.Status == 0:
		return fmt.Sprintf("agent client: transient: %v", e.Cause)
	case e.Code == "":
		return fmt.Sprintf("agent client: HTTP %d: %s", e.Status, e.Message)
	default:
		return fmt.Sprintf("agent client: HTTP %d %s: %s",
			e.Status, e.Code, e.Message)
	}
}

// Unwrap exposes the underlying Cause so errors.Is / errors.As traverse
// past the categorisation envelope.
func (e *Error) Unwrap() error { return e.Cause }

// Predicate helpers. They use errors.As under the hood so they work on
// wrapped errors too — callers should prefer these over direct field
// access when classifying errors several layers deep.

func IsTransient(err error) bool          { return is(err, CategoryTransient) }
func IsBadRequest(err error) bool         { return is(err, CategoryBadRequest) }
func IsUnauthenticated(err error) bool    { return is(err, CategoryUnauthenticated) }
func IsForbidden(err error) bool          { return is(err, CategoryForbidden) }
func IsNotFound(err error) bool           { return is(err, CategoryNotFound) }
func IsConflict(err error) bool           { return is(err, CategoryConflict) }
func IsPreconditionFailed(err error) bool { return is(err, CategoryPreconditionFailed) }
func IsTooManyRequests(err error) bool    { return is(err, CategoryTooManyRequests) }

func is(err error, want Category) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Category == want
}

// classify maps an HTTP status code to a Category. It is the single place
// the broker's contract (docs/design.md §7.0) is decoded into the
// agent's typed error model. Unrecognised statuses fall through to
// CategoryTransient on 5xx and are otherwise treated as best-effort
// CategoryBadRequest, matching the broker's "fail-fast on garbage" stance.
func classify(status int) Category {
	switch {
	case status >= 500:
		return CategoryTransient
	case status == http.StatusBadRequest:
		return CategoryBadRequest
	case status == http.StatusUnauthorized:
		return CategoryUnauthenticated
	case status == http.StatusForbidden:
		return CategoryForbidden
	case status == http.StatusNotFound:
		return CategoryNotFound
	case status == http.StatusConflict:
		return CategoryConflict
	case status == http.StatusPreconditionFailed:
		return CategoryPreconditionFailed
	case status == http.StatusTooManyRequests:
		return CategoryTooManyRequests
	default:
		// Other 4xx fall through here (e.g. 405, 415). They are protocol
		// bugs from the agent's side and should not be retried.
		return CategoryBadRequest
	}
}
