package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the production site.
const DefaultBaseURL = "https://rahulbalajee.com"

const (
	defaultTimeout      = 30 * time.Second
	defaultMaxRetries   = 2
	defaultMaxRetryWait = 60 * time.Second
	// Response bounds. The server escapes <, > and & in JSON, which can grow
	// an 80 KiB body sixfold; a list page holds up to 100 drafts.
	maxListResponse  = 64 << 20
	maxPostResponse  = 1 << 20
	maxSmallResponse = 64 << 10
)

// credentialPattern matches generated client IDs and secrets: 256-bit values
// as lowercase hex.
var credentialPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Config configures a Client. Only the credentials are required.
type Config struct {
	// BaseURL is the site origin, without a path. HTTPS is required except
	// for loopback test servers. Defaults to DefaultBaseURL.
	BaseURL string
	// ClientID and ClientSecret are the OAuth client credentials.
	ClientID     string
	ClientSecret string
	// UserAgent identifies the caller, for example "blogctl/1.2.3".
	UserAgent string
	// Timeout bounds each HTTP request. Defaults to 30 seconds.
	Timeout time.Duration
	// AllowIPv6 lets connections use IPv6. The edge allowlist holds IPv4
	// addresses, so the default is IPv4 only.
	AllowIPv6 bool
	// MaxRetries bounds retries of rate-limited requests and of failed reads.
	// Defaults to 2; a negative value disables retries.
	MaxRetries int
	// MaxRetryWait is the longest Retry-After the client waits out itself;
	// a longer one is returned to the caller. Defaults to 60 seconds.
	MaxRetryWait time.Duration

	// Transport, Now and Sleep replace the network and clock in tests.
	Transport http.RoundTripper
	Now       func() time.Time
	Sleep     func(context.Context, time.Duration) error
}

// Client is a Blog API client. It is safe for concurrent use.
type Client struct {
	base         *url.URL
	api          *http.Client
	upload       *http.Client
	tokens       *tokenSource
	userAgent    string
	maxRetries   int
	maxRetryWait time.Duration
	sleep        func(context.Context, time.Duration) error
	now          func() time.Time
}

// New validates cfg and returns a client. It makes no requests.
func New(cfg Config) (*Client, error) {
	base, err := parseBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if !credentialPattern.MatchString(cfg.ClientID) || !credentialPattern.MatchString(cfg.ClientSecret) {
		return nil, &Error{Kind: KindAuth, Message: "client ID and secret must each be 64 lowercase hexadecimal characters"}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = defaultMaxRetries
	} else if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	if cfg.MaxRetryWait <= 0 {
		cfg.MaxRetryWait = defaultMaxRetryWait
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepContext
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "blogctl"
	}
	apiTransport, uploadTransport := cfg.Transport, cfg.Transport
	if apiTransport == nil {
		apiTransport = newTransport(cfg.Timeout, !cfg.AllowIPv6)
		// Storage is not behind the allowlist, so uploads may use any family.
		uploadTransport = newTransport(cfg.Timeout, false)
	}
	c := &Client{
		base:         base,
		api:          newHTTPClient(apiTransport, cfg.Timeout),
		upload:       newHTTPClient(uploadTransport, 2*cfg.Timeout),
		userAgent:    cfg.UserAgent,
		maxRetries:   cfg.MaxRetries,
		maxRetryWait: cfg.MaxRetryWait,
		sleep:        cfg.Sleep,
		now:          cfg.Now,
	}
	c.tokens = &tokenSource{now: cfg.Now, exchange: func(ctx context.Context) (tokenResponse, error) {
		return c.exchange(ctx, cfg.ClientID, cfg.ClientSecret)
	}}
	return c, nil
}

// CheckBaseURL reports whether raw is an acceptable base URL, without
// building a client: the production origin or a loopback test server.
func CheckBaseURL(raw string) error {
	_, err := parseBaseURL(raw)
	return err
}

// parseBaseURL accepts an origin: HTTPS, or plain HTTP on a loopback host.
func parseBaseURL(raw string) (*url.URL, error) {
	if raw == "" {
		raw = DefaultBaseURL
	}
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, invalid("base URL must be an origin such as %s", DefaultBaseURL)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopbackHost(u.Hostname()) {
			return nil, invalid("plain HTTP is allowed only for loopback test servers; use HTTPS")
		}
	default:
		return nil, invalid("base URL must use HTTPS")
	}
	u.Path = ""
	// The credentials are sent to this origin, so only the site itself or a
	// local test server is accepted: a misled caller cannot redirect them.
	if u.String() != DefaultBaseURL && !loopbackHost(u.Hostname()) {
		return nil, invalid("base URL must be %s or a loopback test server", DefaultBaseURL)
	}
	return u, nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func newTransport(timeout time.Duration, ipv4Only bool) http.RoundTripper {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	network := "tcp"
	if ipv4Only {
		network = "tcp4"
	}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
}

// newHTTPClient refuses redirects: the response is returned as is and treated
// as a failure, so credentials never follow a redirect anywhere.
func newHTTPClient(transport http.RoundTripper, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type authMode int

const (
	authNone authMode = iota
	authBasic
	authBearer
)

type request struct {
	method      string
	path        string
	query       url.Values
	body        []byte
	contentType string
	auth        authMode
	// basic holds the encoded Basic credentials for the token endpoint.
	basic string
	// maxBody bounds the response body.
	maxBody int64
	// idempotent requests are retried after failures; writes are not.
	idempotent bool
}

type response struct {
	status     int
	header     http.Header
	body       []byte
	requestID  string
	retryAfter time.Duration
}

// send performs r with the client's retry rules: a 429 is retried after its
// Retry-After (it was refused before any work, so writes are safe too); a
// rejected bearer token is replaced once; failed reads are retried with
// backoff. Writes are never retried after an unclear failure.
func (c *Client) send(ctx context.Context, r request) (*response, error) {
	refreshed := false
	for attempt := 0; ; attempt++ {
		resp, err := c.once(ctx, r)
		if err != nil {
			// Only a network failure of this request is retried here; a failed
			// token exchange has already had its own retries.
			var e *Error
			retryable := errors.As(err, &e) && e.Kind == KindUnavailable && !e.unsent
			if retryable && r.idempotent && attempt < c.maxRetries && ctx.Err() == nil {
				if werr := c.wait(ctx, backoff(attempt)); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, err
		}
		switch {
		case resp.status == http.StatusUnauthorized && r.auth == authBearer && !refreshed:
			// A token can be rejected after a key rotation or clock skew;
			// replace it once. A second 401 is final.
			c.tokens.invalidate()
			refreshed = true
			attempt--
			continue
		case resp.status == http.StatusTooManyRequests && attempt < c.maxRetries:
			wait := resp.retryAfter
			if wait <= 0 {
				wait = backoff(attempt)
			}
			if wait > c.maxRetryWait {
				return resp, nil
			}
			if werr := c.wait(ctx, wait); werr != nil {
				return nil, werr
			}
			continue
		case resp.status >= 500 && r.idempotent && attempt < c.maxRetries:
			if werr := c.wait(ctx, backoff(attempt)); werr != nil {
				return nil, werr
			}
			continue
		}
		return resp, nil
	}
}

func backoff(attempt int) time.Duration {
	return time.Duration(500<<attempt) * time.Millisecond
}

func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if err := c.sleep(ctx, d); err != nil {
		return &Error{Kind: KindUnknown, Message: "interrupted while waiting to retry", Err: err}
	}
	return nil
}

func (c *Client) once(ctx context.Context, r request) (*response, error) {
	u := *c.base
	u.Path = r.path
	u.RawQuery = r.query.Encode()
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u.String(), body)
	if err != nil {
		return nil, &Error{Kind: KindUnknown, Message: "could not build the request", Err: err, unsent: true}
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	if r.body != nil {
		req.Header.Set("Content-Type", r.contentType)
		// CloudFront's origin access control signs the exact body bytes.
		sum := sha256.Sum256(r.body)
		req.Header.Set("x-amz-content-sha256", hex.EncodeToString(sum[:]))
	}
	switch r.auth {
	case authBasic:
		req.Header.Set("Authorization", "Basic "+r.basic)
	case authBearer:
		token, err := c.tokens.get(ctx)
		if err != nil {
			var e *Error
			if errors.As(err, &e) {
				unsent := *e
				unsent.unsent = true
				return nil, &unsent
			}
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := c.api.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, r.maxBody+1))
	if err != nil {
		return nil, transportError(ctx, err)
	}
	resp := &response{
		status:     res.StatusCode,
		header:     res.Header,
		body:       data,
		requestID:  res.Header.Get("X-Request-ID"),
		retryAfter: retryAfter(res.Header.Get("Retry-After"), c.now()),
	}
	if int64(len(data)) > r.maxBody {
		return nil, &Error{Kind: KindUnknown, Status: res.StatusCode, RequestID: resp.requestID, Message: "response exceeded the size limit"}
	}
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		return nil, &Error{Kind: KindUnknown, Status: res.StatusCode, RequestID: resp.requestID, Message: "refused a redirect; check the base URL"}
	}
	return resp, nil
}

// transportError describes a failure without echoing request details.
func transportError(ctx context.Context, err error) *Error {
	if ctx.Err() != nil {
		return &Error{Kind: KindUnknown, Message: "interrupted", Err: ctx.Err()}
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return &Error{Kind: KindUnavailable, Message: "the request timed out", Err: err}
	}
	return &Error{Kind: KindUnavailable, Message: "could not reach the site", Err: err}
}

// retryAfter parses seconds or an HTTP date.
func retryAfter(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

// apiCode returns the JSON error code, or "" for an empty or non-JSON body.
func (r *response) apiCode() string {
	var body errorResponse
	if json.Unmarshal(r.body, &body) != nil {
		return ""
	}
	return body.Error
}

// failure turns an unexpected response into an *Error. what names the
// target, such as `post "notes"`.
func (r *response) failure(what string) *Error {
	code := r.apiCode()
	e := &Error{Status: r.status, Code: code, RequestID: r.requestID, RetryAfter: r.retryAfter}
	switch {
	case code == "" && r.status == http.StatusForbidden:
		// The application always answers JSON; an empty refusal is the edge.
		e.Kind = KindBlocked
		e.Message = "the site's edge refused the request: this machine's public IPv4 address is probably not on the allowlist"
	case code == "" && r.status == http.StatusBadRequest:
		e.Kind = KindBlocked
		e.Message = "the site's edge rejected the request path"
	case r.status == http.StatusUnauthorized:
		e.Kind = KindAuth
		e.Message = "the access token was rejected"
		if code == "invalid_client" {
			e.Message = "the client credentials were rejected: wrong, revoked, expired or disabled client"
		}
	case r.status == http.StatusForbidden:
		e.Kind = KindForbidden
		e.Message = "this client is not allowed to do that"
		if code == "insufficient_scope" {
			e.Message = "this client lacks the scope for that operation"
		}
	case r.status == http.StatusNotFound:
		e.Kind = KindNotFound
		e.Message = what + " not found"
		if strings.HasPrefix(what, "post") {
			// The API answers alike for missing posts and other authors' posts.
			e.Message += " (it may belong to another author)"
		}
	case r.status == http.StatusConflict:
		e.Kind = KindConflict
		e.Message = what + " conflicts with its current state"
	case r.status == http.StatusTooManyRequests:
		e.Kind = KindRateLimited
		e.Message = "rate limited"
		if r.retryAfter > 0 {
			e.Message = fmt.Sprintf("rate limited; retry after %s", r.retryAfter.Round(time.Second))
		}
	case r.status == http.StatusBadRequest, r.status == http.StatusRequestEntityTooLarge,
		r.status == http.StatusUnsupportedMediaType, r.status == http.StatusUnprocessableEntity:
		e.Kind = KindInvalid
		e.Message = "the site rejected the request"
		switch code {
		case "invalid_article":
			e.Message = "the site rejected the article"
		case "invalid_image":
			e.Message = "the site rejected the image"
		case "invalid_scope":
			e.Message = "the requested scope is not assigned to this client"
		case "request_too_large":
			e.Message = "the request is too large"
		}
	case r.status >= 500:
		e.Kind = KindUnavailable
		e.Message = "the site is temporarily unavailable"
	default:
		e.Kind = KindUnknown
		e.Message = "unexpected response"
	}
	return e
}
