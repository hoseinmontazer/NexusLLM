// Package runtime — per-provider HTTP transport.
//
// Design goals:
//   - Each provider endpoint owns exactly one *http.Client, built once and
//     reused for the lifetime of the endpoint in the registry.
//   - Clients are never shared across endpoints with different configurations.
//   - No global environment variables (HTTP_PROXY, HTTPS_PROXY, ALL_PROXY)
//     are consulted. All proxy settings come exclusively from DB configuration.
//   - Changing one provider's proxy has zero effect on any other provider.
//   - The transport abstraction is extensible: future features (rotating proxy
//     pools, geo-routing, mTLS, service mesh, IP whitelisting) can be added
//     by wrapping the RoundTripper without touching provider code.
package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/http2"
)

// ─────────────────────────────────────────────────────────────────────────────
// ProviderTransportConfig
// ─────────────────────────────────────────────────────────────────────────────

// ProviderTransportConfig holds every tunable property of the HTTP transport
// used to reach a cloud provider endpoint.
//
// It is stored per-endpoint in model_endpoints (migration 046) and loaded by
// the Registry at startup and on every Reload().
//
// Zero values are safe: BuildProviderClient() applies production-grade
// defaults for every zero field. Operators only need to set fields they want
// to override.
type ProviderTransportConfig struct {
	// ── Proxy ────────────────────────────────────────────────────────────
	// ProxyURL is the outbound proxy for this provider only.
	// Supported schemes: http, https, socks5.
	// Credentials may be embedded: http://user:pass@proxy.corp:3128
	// Empty = direct connection. HTTP_PROXY / HTTPS_PROXY env vars are
	// never consulted — isolation between providers is guaranteed.
	ProxyURL string

	// ── TLS ──────────────────────────────────────────────────────────────
	// TLSInsecureSkipVerify disables server certificate verification.
	// Use only in controlled corporate environments where a MITM proxy
	// re-signs TLS certificates. Never set true against public provider APIs.
	TLSInsecureSkipVerify bool

	// TLSRootCAPEM is a PEM-encoded root CA bundle appended to system roots.
	// Used when a corporate proxy presents a certificate signed by an
	// internal CA that is not in the OS trust store.
	TLSRootCAPEM string

	// ── Timeouts ─────────────────────────────────────────────────────────
	// ConnectTimeoutSeconds is the TCP dial + TLS handshake timeout.
	// Default: 10 s.
	ConnectTimeoutSeconds int

	// ReadTimeoutSeconds caps the time to receive a complete non-streaming
	// response body. 0 = unlimited (recommended for streaming endpoints;
	// the caller's context.WithTimeout controls the deadline instead).
	// Default: 0.
	ReadTimeoutSeconds int

	// IdleConnTimeoutSeconds controls how long an idle keep-alive connection
	// stays in the pool before being closed.
	// Default: 90 s.
	IdleConnTimeoutSeconds int

	// ResponseHeaderTimeoutSeconds is the maximum time to wait for the
	// server to send response headers after the full request is sent.
	// 0 disables the timeout (not recommended for non-streaming calls).
	// Default: 30 s.
	ResponseHeaderTimeoutSeconds int

	// ── Connection pool ───────────────────────────────────────────────────
	// MaxIdleConnsPerHost is the maximum idle keep-alive connections in the
	// pool per target host.
	// Default: 32.
	MaxIdleConnsPerHost int

	// MaxConnsPerHost limits total open connections (idle + active) per host.
	// 0 = unlimited.
	MaxConnsPerHost int

	// ── Protocol ─────────────────────────────────────────────────────────
	// DisableHTTP2 prevents HTTP/2 negotiation via ALPN.
	// Set true only if the provider has known HTTP/2 compatibility issues.
	// Default: false (HTTP/2 enabled when server advertises it).
	DisableHTTP2 bool
}

// DefaultProviderTransportConfig returns safe production defaults.
// These are the values used when no per-endpoint override is stored in the DB.
func DefaultProviderTransportConfig() ProviderTransportConfig {
	return ProviderTransportConfig{
		ConnectTimeoutSeconds:        10,
		ReadTimeoutSeconds:           0, // streaming-safe; use context deadline
		IdleConnTimeoutSeconds:       90,
		ResponseHeaderTimeoutSeconds: 30,
		MaxIdleConnsPerHost:          32,
		MaxConnsPerHost:              0, // unlimited
		DisableHTTP2:                 false,
	}
}

// withDefaults returns a copy of cfg with all zero fields replaced by
// production-grade defaults. Non-zero fields in cfg are preserved.
func (cfg ProviderTransportConfig) withDefaults() ProviderTransportConfig {
	d := DefaultProviderTransportConfig()
	if cfg.ConnectTimeoutSeconds <= 0 {
		cfg.ConnectTimeoutSeconds = d.ConnectTimeoutSeconds
	}
	if cfg.IdleConnTimeoutSeconds <= 0 {
		cfg.IdleConnTimeoutSeconds = d.IdleConnTimeoutSeconds
	}
	if cfg.MaxIdleConnsPerHost <= 0 {
		cfg.MaxIdleConnsPerHost = d.MaxIdleConnsPerHost
	}
	// ResponseHeaderTimeout: 0 means "use default (30 s)"; negative means disabled.
	// We use a sentinel of -1 to mean "disabled" at the DB layer.
	if cfg.ResponseHeaderTimeoutSeconds == 0 {
		cfg.ResponseHeaderTimeoutSeconds = d.ResponseHeaderTimeoutSeconds
	} else if cfg.ResponseHeaderTimeoutSeconds < 0 {
		cfg.ResponseHeaderTimeoutSeconds = 0 // explicitly disabled
	}
	return cfg
}

// ─────────────────────────────────────────────────────────────────────────────
// BuildProviderClient
// ─────────────────────────────────────────────────────────────────────────────

// BuildProviderClient constructs a production-grade *http.Client from cfg.
//
// Guarantees:
//   - Never uses http.DefaultTransport or http.DefaultClient.
//   - Never reads HTTP_PROXY, HTTPS_PROXY, or ALL_PROXY env variables.
//     When ProxyURL is empty the transport Proxy func is explicitly nil so
//     the standard library cannot fall back to env vars.
//   - TCP keep-alives are always enabled (30 s interval).
//   - Connection pooling is always enabled.
//   - HTTP/2 is configured via golang.org/x/net/http2.ConfigureTransport
//     unless DisableHTTP2 is true.
//   - The client's Timeout field is zero (no global deadline). This is correct
//     for providers because streaming responses can run indefinitely and must
//     not be killed by a client-level wall-clock timer. Per-request deadlines
//     are enforced via context at the call site.
//
// Returns an error only for invalid configuration (bad proxy URL, unparseable
// CA PEM). All other fields have safe defaults.
func BuildProviderClient(cfg ProviderTransportConfig) (*http.Client, error) {
	cfg = cfg.withDefaults()

	// ── TLS ──────────────────────────────────────────────────────────────
	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.TLSInsecureSkipVerify, //nolint:gosec // explicit operator opt-in
		MinVersion:         tls.VersionTLS12,
	}
	if cfg.TLSRootCAPEM != "" {
		pool, err := buildCertPool(cfg.TLSRootCAPEM)
		if err != nil {
			return nil, fmt.Errorf("provider transport: invalid TLS root CA PEM: %w", err)
		}
		tlsCfg.RootCAs = pool
	}

	// ── Proxy ─────────────────────────────────────────────────────────────
	// Explicitly nil when no proxy is configured so the net/http internals
	// cannot fall back to environment variables (ProxyFromEnvironment).
	var proxyFunc func(*http.Request) (*url.URL, error)
	if cfg.ProxyURL != "" {
		parsed, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("provider transport: invalid proxy URL %q: %w", cfg.ProxyURL, err)
		}
		switch parsed.Scheme {
		case "http", "https", "socks5":
			// valid
		default:
			return nil, fmt.Errorf("provider transport: unsupported proxy scheme %q — use http, https, or socks5", parsed.Scheme)
		}
		proxyFunc = http.ProxyURL(parsed)
	}

	// ── Dialer ────────────────────────────────────────────────────────────
	dialer := &net.Dialer{
		Timeout:   time.Duration(cfg.ConnectTimeoutSeconds) * time.Second,
		KeepAlive: 30 * time.Second, // always on — prevents silent connection drops
	}

	// ── Transport ─────────────────────────────────────────────────────────
	t := &http.Transport{
		DialContext:         dialer.DialContext,
		Proxy:               proxyFunc, // nil = direct, never reads env vars
		TLSClientConfig:     tlsCfg,
		MaxIdleConnsPerHost: cfg.MaxIdleConnsPerHost,
		MaxConnsPerHost:     cfg.MaxConnsPerHost,
		IdleConnTimeout:     time.Duration(cfg.IdleConnTimeoutSeconds) * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   !cfg.DisableHTTP2,
		// ResponseHeaderTimeout set below after conditional
	}
	if cfg.ResponseHeaderTimeoutSeconds > 0 {
		t.ResponseHeaderTimeout = time.Duration(cfg.ResponseHeaderTimeoutSeconds) * time.Second
	}

	// Upgrade to HTTP/2. ConfigureTransport installs the h2 RoundTripper and
	// enables connection multiplexing when the server negotiates ALPN "h2".
	if !cfg.DisableHTTP2 {
		_ = http2.ConfigureTransport(t)
	}

	// ── Client ────────────────────────────────────────────────────────────
	// Timeout = 0: no global wall-clock limit. Streaming responses must not
	// be killed by the client. Use context.WithTimeout at the call site.
	var rt http.RoundTripper = t
	if cfg.ReadTimeoutSeconds > 0 {
		rt = &readTimeoutTransport{
			wrapped:     t,
			readTimeout: time.Duration(cfg.ReadTimeoutSeconds) * time.Second,
		}
	}

	return &http.Client{Transport: rt}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// readTimeoutTransport
// ─────────────────────────────────────────────────────────────────────────────

// readTimeoutTransport is an http.RoundTripper that applies a read deadline to
// non-streaming responses while leaving SSE streams unrestricted.
//
// Streaming responses (Accept: text/event-stream) are forwarded without
// modification so that long-running inference streams are never killed.
// Non-streaming responses get a context deadline equal to readTimeout,
// unless the request context already has a tighter deadline.
type readTimeoutTransport struct {
	wrapped     http.RoundTripper
	readTimeout time.Duration
}

func (rt *readTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Streaming requests: pass through without adding a deadline.
	if req.Header.Get("Accept") == "text/event-stream" {
		return rt.wrapped.RoundTrip(req)
	}

	// Non-streaming: add a deadline if the context does not already have one
	// that is tighter than readTimeout.
	ctx := req.Context()
	var cancel context.CancelFunc
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > rt.readTimeout {
		ctx, cancel = context.WithTimeout(ctx, rt.readTimeout)
		req = req.WithContext(ctx)
	}

	resp, err := rt.wrapped.RoundTrip(req)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}

	if cancel != nil && resp != nil && resp.Body != nil {
		resp.Body = &readTimeoutBody{ReadCloser: resp.Body, cancel: cancel}
	}
	return resp, nil
}

type readTimeoutBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *readTimeoutBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// proxyErrorClassifier — maps connection errors to observability labels
// ─────────────────────────────────────────────────────────────────────────────

// ClassifyProviderError returns a short string label suitable for use as a
// Prometheus label value. It identifies whether an error is proxy-related or
// a general connection failure so dashboards can separate the two.
func ClassifyProviderError(err error) string {
	if err == nil {
		return "none"
	}
	msg := err.Error()
	switch {
	case contains(msg, "proxyconnect"), contains(msg, "proxy"), contains(msg, "CONNECT"):
		return "proxy_error"
	case contains(msg, "connection refused"), contains(msg, "no such host"),
		contains(msg, "i/o timeout"), contains(msg, "dial"):
		return "connection_failure"
	case contains(msg, "context deadline exceeded"), contains(msg, "context canceled"):
		return "timeout"
	case contains(msg, "tls"), contains(msg, "x509"), contains(msg, "certificate"):
		return "tls_error"
	default:
		return "request_error"
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}

// ─────────────────────────────────────────────────────────────────────────────
// ValidateProxyURL — public helper used by admin handlers
// ─────────────────────────────────────────────────────────────────────────────

// ValidateProxyURL parses and validates a proxy URL string.
//
// Rules:
//   - Empty string is valid (means "no proxy — direct connection").
//   - Supported schemes: http, https, socks5.
//   - Credentials are allowed (user:pass@host:port).
//   - Returns a descriptive error for unsupported schemes or unparseable URLs.
//
// This is intentionally a standalone function (not a method) so admin HTTP
// handlers can call it before writing to the database, giving operators a
// clear error message rather than a silent misconfiguration.
func ValidateProxyURL(raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid proxy URL %q: %w", raw, err)
	}
	if parsed.Host == "" {
		return fmt.Errorf("invalid proxy URL %q: missing host", raw)
	}
	switch parsed.Scheme {
	case "http", "https", "socks5":
		// valid
	case "":
		return fmt.Errorf("invalid proxy URL %q: missing scheme (use http://, https://, or socks5://)", raw)
	default:
		return fmt.Errorf("invalid proxy URL %q: unsupported scheme %q — use http, https, or socks5", raw, parsed.Scheme)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// buildCertPool parses pemData and returns a certificate pool that includes
// both the system root CAs and the certificates in pemData.
func buildCertPool(pemData string) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool() // Windows fallback
	}
	if !pool.AppendCertsFromPEM([]byte(pemData)) {
		return nil, fmt.Errorf("no valid PEM certificates found in TLSRootCAPEM")
	}
	return pool, nil
}
