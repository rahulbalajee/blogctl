package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// renewBefore replaces a token this long before it expires, so a request
// never carries a token that lapses in flight.
const renewBefore = 60 * time.Second

// tokenSource keeps one access token in memory and exchanges a new one when
// it is missing or about to expire. Concurrent callers share one exchange.
// Tokens are never written to disk.
type tokenSource struct {
	mu       sync.Mutex
	token    string
	scopes   []string
	expires  time.Time
	margin   time.Duration
	now      func() time.Time
	exchange func(context.Context) (tokenResponse, error)
}

func (t *tokenSource) get(ctx context.Context) (string, error) {
	info, err := t.current(ctx)
	if err != nil {
		return "", err
	}
	return info.token, nil
}

type tokenState struct {
	token   string
	scopes  []string
	expires time.Time
}

func (t *tokenSource) current(ctx context.Context) (tokenState, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && t.now().Before(t.expires.Add(-t.margin)) {
		return tokenState{token: t.token, scopes: t.scopes, expires: t.expires}, nil
	}
	issued, err := t.exchange(ctx)
	if err != nil {
		t.token = ""
		return tokenState{}, err
	}
	lifetime := time.Duration(issued.ExpiresIn) * time.Second
	t.token = issued.AccessToken
	t.scopes = strings.Fields(issued.Scope)
	t.expires = t.now().Add(lifetime)
	// Renew a minute early, or halfway through a token shorter than that.
	t.margin = min(renewBefore, lifetime/2)
	return tokenState{token: t.token, scopes: t.scopes, expires: t.expires}, nil
}

func (t *tokenSource) invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token = ""
}

// formEncode applies application/x-www-form-urlencoded escaping, which RFC
// 6749 requires for client credentials inside Basic authentication.
func formEncode(value string) string {
	return url.QueryEscape(value)
}

// exchange trades the client credentials for an access token. The Basic
// credentials go only to the token endpoint.
func (c *Client) exchange(ctx context.Context, id, secret string) (tokenResponse, error) {
	body := []byte("grant_type=client_credentials")
	resp, err := c.send(ctx, request{
		method:      http.MethodPost,
		path:        "/oauth/token",
		body:        body,
		contentType: "application/x-www-form-urlencoded",
		auth:        authBasic,
		basic:       base64.StdEncoding.EncodeToString([]byte(formEncode(id) + ":" + formEncode(secret))),
		maxBody:     maxSmallResponse,
		idempotent:  true,
	})
	if err != nil {
		return tokenResponse{}, err
	}
	if resp.status != http.StatusOK {
		return tokenResponse{}, resp.failure("token request")
	}
	var issued tokenResponse
	if err := json.Unmarshal(resp.body, &issued); err != nil || issued.AccessToken == "" ||
		!strings.EqualFold(issued.TokenType, "Bearer") || issued.ExpiresIn <= 0 ||
		strings.ContainsAny(issued.AccessToken, " \t\r\n") {
		return tokenResponse{}, &Error{Kind: KindUnknown, Status: resp.status, RequestID: resp.requestID, Message: "the token response was malformed"}
	}
	return issued, nil
}

// Check confirms this machine can reach the API and the credentials work,
// without performing any draft operation. It first asks for the public key
// set, which needs no credentials, so an edge block is reported before the
// secret is ever sent.
func (c *Client) Check(ctx context.Context) (TokenInfo, error) {
	resp, err := c.send(ctx, request{
		method:     http.MethodGet,
		path:       "/.well-known/jwks.json",
		maxBody:    maxSmallResponse,
		idempotent: true,
	})
	if err != nil {
		return TokenInfo{}, err
	}
	if resp.status == http.StatusNotFound {
		return TokenInfo{}, &Error{Kind: KindNotFound, Status: resp.status, RequestID: resp.requestID, Message: "the site does not serve the OAuth API (it is switched off or not deployed)"}
	}
	if resp.status != http.StatusOK {
		return TokenInfo{}, resp.failure("key set")
	}
	state, err := c.tokens.current(ctx)
	if err != nil {
		return TokenInfo{}, err
	}
	return TokenInfo{Scopes: state.scopes, ExpiresAt: state.expires}, nil
}
