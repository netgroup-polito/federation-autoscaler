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
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Default knobs for the request-layer retry / timeout policy. Substep 7a
// keeps these conservative: the poller in 7c is what drives the actual
// call cadence, so the per-call timeout has to be shorter than the poll
// interval (5 s) but long enough to cover a Broker reconcile under load.
const (
	DefaultRequestTimeout    = 10 * time.Second
	DefaultMaxRetries        = 3
	DefaultInitialBackoff    = 100 * time.Millisecond
	DefaultBackoffMultiplier = 2.0
	DefaultMaxBackoff        = 2 * time.Second
)

// Options bundles the construction-time settings of a Client. Empty
// values fall back to the Default* constants above.
type Options struct {
	// BrokerURL is the base URL of the Broker REST API, e.g.
	// "https://broker.example.com:8443". Path is ignored; only scheme +
	// host + port are used. Required.
	BrokerURL string

	// TLS configures the mTLS handshake. Required — the Broker rejects
	// plaintext traffic.
	TLS TLSConfig

	// Logger is the structured logger every request logs through.
	// Defaults to controller-runtime's logger named "agent-client".
	Logger logr.Logger

	// RequestTimeout caps a single HTTP attempt (not the whole
	// retry chain). Defaults to DefaultRequestTimeout.
	RequestTimeout time.Duration

	// MaxRetries is the maximum number of additional attempts after the
	// first. MaxRetries=3 means up to 4 attempts total. Zero or unset
	// falls back to DefaultMaxRetries.
	MaxRetries int

	// InitialBackoff is the wait before the first retry. Defaults to
	// DefaultInitialBackoff.
	InitialBackoff time.Duration

	// BackoffMultiplier is the geometric factor applied to the backoff
	// after every failed attempt. Defaults to DefaultBackoffMultiplier.
	BackoffMultiplier float64

	// MaxBackoff caps individual sleep durations between retries.
	// Defaults to DefaultMaxBackoff.
	MaxBackoff time.Duration

	// Transport overrides the underlying http.RoundTripper. Tests inject
	// httptest-aware transports here. Production code should leave this
	// nil so a real *http.Transport is built from TLS.
	Transport http.RoundTripper
}

// Client is the typed Broker HTTP client embedded by both the consumer-
// and provider-role agents. It is safe for concurrent use, is cheap to
// construct (a single tls.Config + http.Client), and owns no goroutines —
// the poller in 7c drives request cadence.
type Client struct {
	baseURL *url.URL
	http    *http.Client
	log     logr.Logger

	maxRetries        int
	initialBackoff    time.Duration
	backoffMultiplier float64
	maxBackoff        time.Duration
}

// New validates opts, builds the mTLS transport, and returns a ready-to-use
// Client. It performs no network I/O.
func New(opts Options) (*Client, error) {
	if opts.BrokerURL == "" {
		return nil, fmt.Errorf("client: BrokerURL is required")
	}
	parsed, err := url.Parse(opts.BrokerURL)
	if err != nil {
		return nil, fmt.Errorf("client: parse BrokerURL %q: %w", opts.BrokerURL, err)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("client: BrokerURL must use https (got scheme %q)", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("client: BrokerURL %q has no host", opts.BrokerURL)
	}
	// Strip path/query/fragment: the client only uses the origin.
	parsed = &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}

	transport := opts.Transport
	if transport == nil {
		tlsCfg, err := opts.TLS.BuildClientTLSConfig()
		if err != nil {
			return nil, err
		}
		transport = newHTTPTransport(tlsCfg)
	}

	logger := opts.Logger
	if logger.GetSink() == nil {
		logger = log.Log.WithName("agent-client")
	}

	c := &Client{
		baseURL:           parsed,
		http:              &http.Client{Transport: transport, Timeout: orDefault(opts.RequestTimeout, DefaultRequestTimeout)},
		log:               logger,
		maxRetries:        orDefaultInt(opts.MaxRetries, DefaultMaxRetries),
		initialBackoff:    orDefault(opts.InitialBackoff, DefaultInitialBackoff),
		backoffMultiplier: opts.BackoffMultiplier,
		maxBackoff:        orDefault(opts.MaxBackoff, DefaultMaxBackoff),
	}
	if c.backoffMultiplier <= 0 {
		c.backoffMultiplier = DefaultBackoffMultiplier
	}
	return c, nil
}

// BaseURL returns a copy of the parsed origin URL the client dials. Used
// by tests and by future endpoint methods that need to construct paths.
func (c *Client) BaseURL() *url.URL { u := *c.baseURL; return &u }

// resolve returns the absolute URL for a Broker-relative path.
// Path values may include a leading "/" or not; both forms work.
func (c *Client) resolve(p string) string {
	u := *c.baseURL
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u.Path = p
	return u.String()
}

// newHTTPTransport returns the production *http.Transport. Defined as a
// package-level variable so tests can replace it; the real implementation
// just wraps the default transport with the mTLS config and disables
// HTTP/2 to match the Broker's CVE-2023-44487 mitigation.
var newHTTPTransport = func(tlsCfg *tls.Config) http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = tlsCfg
	// HTTP/2 is intentionally disabled on the Broker side
	// (cmd/broker/main.go); matching here keeps the negotiated protocol
	// predictable and avoids subtle h2 framing bugs on retry.
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(authority string, c *tls.Conn) http.RoundTripper{}
	return t
}

func orDefault(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func orDefaultInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
