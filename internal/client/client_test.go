package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rahulbalajee/blogctl/internal/blogtest"
	"github.com/rahulbalajee/blogctl/internal/client"
)

type sleeper struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *sleeper) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waits = append(s.waits, d)
	return nil
}

func newClient(t *testing.T, srv *blogtest.Server, edit func(*client.Config)) (*client.Client, *sleeper) {
	t.Helper()
	sleeps := &sleeper{}
	cfg := client.Config{
		BaseURL:      srv.URL,
		ClientID:     blogtest.ClientID,
		ClientSecret: blogtest.ClientSecret,
		Sleep:        sleeps.sleep,
	}
	if edit != nil {
		edit(&cfg)
	}
	c, err := client.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, sleeps
}

func article(title string) client.Article {
	return client.Article{Title: title, Summary: "A summary long enough for the API.", Body: "<p>Body</p>", Tags: []string{" go "}}
}

func kind(t *testing.T, err error, want client.Kind) *client.Error {
	t.Helper()
	var e *client.Error
	if !errors.As(err, &e) || e.Kind != want {
		t.Fatalf("error = %v (kind %v), want kind %v", err, client.KindOf(err), want)
	}
	return e
}

// The token exchange is form-encoded Basic with a body hash, it is shared by
// later calls, and API calls carry only the bearer token.
func TestTokenIsExchangedOnceAndSentOnlyAsBearer(t *testing.T) {
	srv := blogtest.New(t)
	c, _ := newClient(t, srv, nil)
	ctx := context.Background()
	if _, err := c.CreatePost(ctx, "notes", article("Notes")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetPost(ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListPosts(ctx, 10, ""); err != nil {
		t.Fatal(err)
	}
	if exchanges, api := srv.Count(); exchanges != 1 || api != 3 {
		t.Fatalf("exchanges=%d api=%d, want 1 and 3", exchanges, api)
	}
}

func TestTokenRenewsBeforeExpiry(t *testing.T) {
	srv := blogtest.New(t)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	c, _ := newClient(t, srv, func(cfg *client.Config) {
		cfg.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	})
	ctx := context.Background()
	if _, err := c.ListPosts(ctx, 1, ""); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(539 * time.Second)
	mu.Unlock()
	if _, err := c.ListPosts(ctx, 1, ""); err != nil {
		t.Fatal(err)
	}
	if exchanges, _ := srv.Count(); exchanges != 1 {
		t.Fatalf("renewed %d times inside the token's life", exchanges-1)
	}
	mu.Lock()
	now = now.Add(2 * time.Second) // within a minute of expiry
	mu.Unlock()
	if _, err := c.ListPosts(ctx, 1, ""); err != nil {
		t.Fatal(err)
	}
	if exchanges, _ := srv.Count(); exchanges != 2 {
		t.Fatalf("exchanges = %d, want a renewal near expiry", exchanges)
	}
}

// A rejected token is replaced once; a second rejection is final.
func TestUnauthorizedReExchangesOnceThenStops(t *testing.T) {
	srv := blogtest.New(t)
	srv.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			srv.Lock()
			srv.APIRequests++
			srv.Unlock()
			jsonError(w, http.StatusUnauthorized, "invalid_token")
			return true
		}
		return false
	})
	c, _ := newClient(t, srv, nil)
	_, err := c.ListPosts(context.Background(), 1, "")
	kind(t, err, client.KindAuth)
	if exchanges, api := srv.Count(); exchanges != 2 || api != 2 {
		t.Fatalf("exchanges=%d api=%d, want exactly one retry", exchanges, api)
	}
}

func TestForbiddenNeverRefreshes(t *testing.T) {
	srv := blogtest.New(t)
	srv.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path == "/api/v1/media/sign" {
			jsonError(w, http.StatusForbidden, "insufficient_scope")
			return true
		}
		return false
	})
	c, _ := newClient(t, srv, nil)
	_, err := c.SignUpload(context.Background(), "notes", "image/png", 10)
	kind(t, err, client.KindForbidden)
	if exchanges, _ := srv.Count(); exchanges != 1 {
		t.Fatalf("exchanges = %d; a 403 must not trigger a new token", exchanges)
	}
}

func TestRateLimitWaitsForRetryAfterWithinCap(t *testing.T) {
	srv := blogtest.New(t)
	var calls atomic.Int32
	srv.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path == "/api/v1/posts" && calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			jsonError(w, http.StatusTooManyRequests, "rate_limited")
			return true
		}
		return false
	})
	c, sleeps := newClient(t, srv, nil)
	if _, err := c.ListPosts(context.Background(), 1, ""); err != nil {
		t.Fatal(err)
	}
	if len(sleeps.waits) != 1 || sleeps.waits[0] != 2*time.Second {
		t.Fatalf("waits = %v, want one 2s wait", sleeps.waits)
	}

	srv.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path == "/api/v1/posts" {
			w.Header().Set("Retry-After", "120")
			jsonError(w, http.StatusTooManyRequests, "rate_limited")
			return true
		}
		return false
	})
	sleeps.waits = nil
	_, err := c.ListPosts(context.Background(), 1, "")
	if e := kind(t, err, client.KindRateLimited); e.RetryAfter != 120*time.Second || len(sleeps.waits) != 0 {
		t.Fatalf("long Retry-After: %+v, waits %v; want it returned, not waited", e, sleeps.waits)
	}
}

func TestRedirectsAreRefused(t *testing.T) {
	srv := blogtest.New(t)
	var followed atomic.Bool
	srv.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		switch r.URL.Path {
		case "/oauth/token":
			http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
			return true
		case "/elsewhere":
			followed.Store(true)
		}
		return false
	})
	c, _ := newClient(t, srv, nil)
	_, err := c.ListPosts(context.Background(), 1, "")
	if err == nil || followed.Load() || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect: err=%v followed=%v", err, followed.Load())
	}
}

func TestRequestsTimeOut(t *testing.T) {
	srv := blogtest.New(t)
	srv.SetOverride(func(_ http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path == "/api/v1/posts" {
			time.Sleep(300 * time.Millisecond)
		}
		return false
	})
	c, _ := newClient(t, srv, func(cfg *client.Config) {
		cfg.Timeout = 100 * time.Millisecond
		cfg.MaxRetries = -1
	})
	_, err := c.ListPosts(context.Background(), 1, "")
	if e := kind(t, err, client.KindUnavailable); !strings.Contains(e.Message, "timed out") {
		t.Fatalf("timeout message = %q", e.Message)
	}
}

// An edge block is found before the secret is sent anywhere.
func TestCheckReportsEdgeBlockBeforeSendingTheSecret(t *testing.T) {
	srv := blogtest.New(t)
	srv.SetJWKSBlocked(true)
	c, _ := newClient(t, srv, nil)
	_, err := c.Check(context.Background())
	kind(t, err, client.KindBlocked)
	if exchanges, _ := srv.Count(); exchanges != 0 {
		t.Fatal("the secret was sent although the edge refused the key set")
	}

	srv.SetJWKSBlocked(false)
	info, err := c.Check(context.Background())
	if err != nil || strings.Join(info.Scopes, " ") != "drafts:read drafts:write media:write" || info.ExpiresAt.IsZero() {
		t.Fatalf("check = %+v, %v", info, err)
	}
}

func TestDisabledClientIsAnAuthError(t *testing.T) {
	srv := blogtest.New(t)
	srv.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path == "/oauth/token" {
			jsonError(w, http.StatusUnauthorized, "invalid_client")
			return true
		}
		return false
	})
	c, _ := newClient(t, srv, nil)
	_, err := c.GetPost(context.Background(), "notes")
	if e := kind(t, err, client.KindAuth); !strings.Contains(e.Message, "client credentials were rejected") {
		t.Fatalf("message = %q", e.Message)
	}
}

// dropResponse lets the server apply a write, then loses its response.
func dropResponse(srv *blogtest.Server, method, path string) {
	srv.SetOverride(func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method != method || r.URL.Path != path {
			return false
		}
		srv.SetOverride(nil)
		recorder := &discard{header: http.Header{}}
		r.Body = readCloser{bytes.NewReader(body)}
		srv.Config.Handler.ServeHTTP(recorder, r)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
		return true
	})
}

func TestCreateWithLostResponseIsReconciled(t *testing.T) {
	srv := blogtest.New(t)
	c, _ := newClient(t, srv, nil)
	ctx := context.Background()
	if _, err := c.ListPosts(ctx, 1, ""); err != nil { // obtain a token first
		t.Fatal(err)
	}
	dropResponse(srv, http.MethodPost, "/api/v1/posts")
	result, err := c.CreatePost(ctx, "notes", article("Notes"))
	if err != nil || !result.Reconciled || result.Post.Revision != 1 {
		t.Fatalf("lost create = %+v, %v", result, err)
	}
	// Running the same command again finds the same post rather than failing.
	again, err := c.CreatePost(ctx, "notes", article("Notes"))
	if err != nil || !again.Reconciled {
		t.Fatalf("rerun = %+v, %v", again, err)
	}
	// A different article under the same slug is a conflict.
	_, err = c.CreatePost(ctx, "notes", article("Something else"))
	if e := kind(t, err, client.KindConflict); e.CurrentRevision != 1 {
		t.Fatalf("conflict = %+v", e)
	}
}

func TestCreateConflictWithAnUnreadablePost(t *testing.T) {
	srv := blogtest.New(t)
	srv.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/posts" {
			jsonError(w, http.StatusConflict, "conflict")
			return true
		}
		return false
	})
	c, _ := newClient(t, srv, nil)
	_, err := c.CreatePost(context.Background(), "taken", article("Notes"))
	if e := kind(t, err, client.KindConflict); !strings.Contains(e.Message, "another author") {
		t.Fatalf("message = %q", e.Message)
	}
}

// A 409 on update is never success: it reports the current revision.
func TestUpdateConflictReportsCurrentRevision(t *testing.T) {
	srv := blogtest.New(t)
	c, _ := newClient(t, srv, nil)
	ctx := context.Background()
	created, err := c.CreatePost(ctx, "notes", article("Notes"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdatePost(ctx, "notes", created.Post.Revision, article("Second")); err != nil {
		t.Fatal(err)
	}
	_, err = c.UpdatePost(ctx, "notes", created.Post.Revision, article("Stale edit"))
	if e := kind(t, err, client.KindConflict); e.CurrentRevision != 2 {
		t.Fatalf("conflict = %+v", e)
	}
	if post, _ := c.GetPost(ctx, "notes"); post.Draft.Title != "Second" {
		t.Fatalf("stale edit overwrote the post: %q", post.Draft.Title)
	}
}

func TestUpdateWithLostResponseIsReconciled(t *testing.T) {
	srv := blogtest.New(t)
	c, _ := newClient(t, srv, nil)
	ctx := context.Background()
	created, err := c.CreatePost(ctx, "notes", article("Notes"))
	if err != nil {
		t.Fatal(err)
	}
	dropResponse(srv, http.MethodPut, "/api/v1/posts/notes")
	result, err := c.UpdatePost(ctx, "notes", created.Post.Revision, article("Second"))
	if err != nil || !result.Reconciled || result.Post.Revision != 2 {
		t.Fatalf("lost update = %+v, %v", result, err)
	}
}

// The upload goes to storage with the grant's fields, the file last, and none
// of the site's credentials.
func TestUploadSendsNoSiteCredentials(t *testing.T) {
	srv := blogtest.New(t)
	c, _ := newClient(t, srv, nil)
	image, err := c.UploadImage(context.Background(), "notes", "diagram.png", pngBytes(t, 20, 10), "A diagram")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(image.HTML, `<img src="/api/admin/media/notes/`) || !strings.HasSuffix(image.HTML, `alt="A diagram">`) {
		t.Fatalf("html = %s", image.HTML)
	}
	srv.Lock()
	defer srv.Unlock()
	if len(srv.Uploads) != 1 {
		t.Fatalf("uploads = %d", len(srv.Uploads))
	}
	upload := srv.Uploads[0]
	if upload.Header.Get("Authorization") != "" || upload.Header.Get("Cookie") != "" {
		t.Fatal("site credentials were sent to storage")
	}
	for _, token := range srv.Tokens {
		for name, values := range upload.Header {
			if strings.Contains(strings.Join(values, " "), token) {
				t.Fatalf("token sent to storage in %s", name)
			}
		}
	}
	if upload.LastPart != "file" || upload.FileName != "diagram.png" || upload.ContentType != "image/png" || len(upload.Fields) != 3 {
		t.Fatalf("upload form = %+v", upload)
	}
}

func TestResponsesAreBounded(t *testing.T) {
	srv := blogtest.New(t)
	srv.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path == "/api/v1/posts/huge" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(bytes.Repeat([]byte(" "), 2<<20))
			return true
		}
		return false
	})
	c, _ := newClient(t, srv, nil)
	_, err := c.GetPost(context.Background(), "huge")
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized response: %v", err)
	}
}

func TestBaseURLMustBeHTTPSOrLoopback(t *testing.T) {
	for raw, ok := range map[string]bool{
		"":                          true,
		"https://rahulbalajee.com":  true,
		"https://rahulbalajee.com/": true,
		"http://127.0.0.1:8080":     true,
		"http://localhost:8080":     true,
		"http://rahulbalajee.com":   false,
		"https://example.com/api":   false,
		"https://attacker.example":  false,
		"https://rahulbalajee.co":   false,
		"https://[::1]:8443":        true,
		"https://user@example.com":  false,
		"ftp://example.com":         false,
	} {
		_, err := client.New(client.Config{BaseURL: raw, ClientID: blogtest.ClientID, ClientSecret: blogtest.ClientSecret})
		if (err == nil) != ok {
			t.Errorf("base URL %q: err=%v, want ok=%v", raw, err, ok)
		}
	}
	if _, err := client.New(client.Config{ClientID: "short", ClientSecret: blogtest.ClientSecret}); client.KindOf(err) != client.KindAuth {
		t.Errorf("malformed client ID accepted: %v", err)
	}
}

func TestLocalArticleAndImageChecks(t *testing.T) {
	good := article("Notes")
	if err := client.CheckArticle(good); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*client.Article){
		"short title":   func(a *client.Article) { a.Title = " ab " },
		"short summary": func(a *client.Article) { a.Summary = "too short" },
		"empty body":    func(a *client.Article) { a.Body = "  " },
		"six tags":      func(a *client.Article) { a.Tags = []string{"a", "b", "c", "d", "e", "f"} },
		"blank tag":     func(a *client.Article) { a.Tags = []string{" "} },
	} {
		a := good
		edit(&a)
		if client.KindOf(client.CheckArticle(a)) != client.KindInvalid {
			t.Errorf("%s accepted", name)
		}
	}
	if typ, err := client.CheckImage(pngBytes(t, 10, 10)); err != nil || typ != "image/png" {
		t.Fatalf("png = %q, %v", typ, err)
	}
	if _, err := client.CheckImage(pngBytes(t, 4001, 1)); client.KindOf(err) != client.KindInvalid {
		t.Error("oversized image accepted")
	}
	if _, err := client.CheckImage([]byte("GIF89a not a png")); client.KindOf(err) != client.KindInvalid {
		t.Error("gif accepted")
	}
}

// A nil tag list is sent as [], because the API rejects null.
func TestNilTagsAreSentAsAnEmptyList(t *testing.T) {
	srv := blogtest.New(t)
	c, _ := newClient(t, srv, nil)
	a := article("Notes")
	a.Tags = nil
	if _, err := c.CreatePost(context.Background(), "notes", a); err != nil {
		t.Fatal(err)
	}
	srv.Lock()
	defer srv.Unlock()
	for _, seen := range srv.Seen {
		if strings.Contains(seen, `"tags":null`) {
			t.Fatal("tags sent as null")
		}
	}
}

// The client's types decode the site's real responses, copied from its HTTP
// golden files into testdata/server, with no unknown fields.
func TestClientTypesDecodeServerGoldens(t *testing.T) {
	cases := map[string]any{
		"get.json":        &client.Post{},
		"create.json":     &client.Post{},
		"update.json":     &client.Post{},
		"list-one.json":   &client.Page{},
		"list-empty.json": &client.Page{},
		"media-sign.json": &client.UploadGrant{},
	}
	for name, target := range cases {
		raw, err := os.ReadFile(filepath.Join("testdata", "server", name))
		if err != nil {
			t.Fatal(err)
		}
		var golden struct {
			Body json.RawMessage `json:"body"`
		}
		if err := json.Unmarshal(raw, &golden); err != nil {
			t.Fatal(err)
		}
		body := strings.ReplaceAll(string(golden.Body), `"<timestamp>"`, `"2026-10-19T09:00:00Z"`)
		decoder := json.NewDecoder(strings.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func pngBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jsonError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

type discard struct {
	header http.Header
}

func (d *discard) Header() http.Header         { return d.header }
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }
func (d *discard) WriteHeader(int)             {}

type readCloser struct {
	*bytes.Reader
}

func (readCloser) Close() error { return nil }

// Rerunning create with a changed body must not pass as reconciled: the new
// body was not saved.
func TestCreateRerunWithChangedBodyIsAConflict(t *testing.T) {
	srv := blogtest.New(t)
	c, _ := newClient(t, srv, nil)
	ctx := context.Background()
	if _, err := c.CreatePost(ctx, "notes", article("Notes")); err != nil {
		t.Fatal(err)
	}
	changed := article("Notes")
	changed.Body = "<p>A different body</p>"
	_, err := c.CreatePost(ctx, "notes", changed)
	if e := kind(t, err, client.KindConflict); e.CurrentRevision != 1 {
		t.Fatalf("changed rerun = %+v", e)
	}
}

// A lost create whose saved body differs (as sanitizing can do) cannot be
// called applied with certainty.
func TestLostCreateWithRewrittenBodyIsAmbiguous(t *testing.T) {
	srv := blogtest.New(t)
	c, _ := newClient(t, srv, nil)
	ctx := context.Background()
	if _, err := c.ListPosts(ctx, 1, ""); err != nil {
		t.Fatal(err)
	}
	srv.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/posts" {
			return false
		}
		srv.SetOverride(nil)
		post := article("Notes")
		post.Body = "<p>Sanitized</p>"
		srv.Put(client.Post{Slug: "notes", Draft: post, Revision: 1})
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
		return true
	})
	_, err := c.CreatePost(ctx, "notes", article("Notes"))
	kind(t, err, client.KindAmbiguous)
}

// A write that never left the machine (its token exchange failed) is not an
// unclear outcome.
func TestWriteBlockedByTokenFailureIsNotAmbiguous(t *testing.T) {
	srv := blogtest.New(t)
	srv.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path == "/oauth/token" {
			jsonError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
			return true
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			t.Errorf("%s %s sent without a token", r.Method, r.URL.Path)
		}
		return false
	})
	c, _ := newClient(t, srv, nil)
	_, err := c.CreatePost(context.Background(), "notes", article("Notes"))
	kind(t, err, client.KindUnavailable)
}

func TestUnreadableSuccessIsReconciled(t *testing.T) {
	srv := blogtest.New(t)
	c, _ := newClient(t, srv, nil)
	ctx := context.Background()
	if _, err := c.ListPosts(ctx, 1, ""); err != nil {
		t.Fatal(err)
	}
	srv.SetOverride(func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/posts" {
			return false
		}
		srv.SetOverride(nil)
		r.Body = readCloser{bytes.NewReader(body)}
		srv.Config.Handler.ServeHTTP(&discard{header: http.Header{}}, r)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("not json"))
		return true
	})
	result, err := c.CreatePost(ctx, "notes", article("Notes"))
	if err != nil || !result.Reconciled {
		t.Fatalf("unreadable 201 = %+v, %v", result, err)
	}
}

func TestInterruptedWriteIsAmbiguous(t *testing.T) {
	srv := blogtest.New(t)
	c, _ := newClient(t, srv, nil)
	if _, err := c.ListPosts(context.Background(), 1, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv.SetOverride(func(_ http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/posts" {
			cancel()
			time.Sleep(50 * time.Millisecond)
		}
		return false
	})
	_, err := c.CreatePost(ctx, "notes", article("Notes"))
	kind(t, err, client.KindAmbiguous)
}

func TestCheckExplainsAnAPIThatIsSwitchedOff(t *testing.T) {
	srv := blogtest.New(t)
	srv.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path == "/.well-known/jwks.json" {
			jsonError(w, http.StatusNotFound, "not_found")
			return true
		}
		return false
	})
	c, _ := newClient(t, srv, nil)
	_, err := c.Check(context.Background())
	if e := kind(t, err, client.KindNotFound); !strings.Contains(e.Message, "does not serve the OAuth API") {
		t.Fatalf("message = %q", e.Message)
	}
}

func TestEmptyEdge400NamesThePath(t *testing.T) {
	srv := blogtest.New(t)
	srv.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path == "/.well-known/jwks.json" {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		return false
	})
	c, _ := newClient(t, srv, nil)
	_, err := c.Check(context.Background())
	if e := kind(t, err, client.KindBlocked); !strings.Contains(e.Message, "request path") {
		t.Fatalf("message = %q", e.Message)
	}
}

// A failed token exchange has its own retries; the read that needed the token
// must not repeat it. One exchange for a wrong secret, three at most when the
// token endpoint is rate limited.
func TestTokenFailuresAreNotRetriedTwice(t *testing.T) {
	srv := blogtest.New(t)
	c, err := client.New(client.Config{BaseURL: srv.URL, ClientID: blogtest.ClientID, ClientSecret: strings.Repeat("9", 64), Sleep: (&sleeper{}).sleep})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListPosts(context.Background(), 1, "")
	kind(t, err, client.KindAuth)
	if exchanges, _ := srv.Count(); exchanges != 1 {
		t.Fatalf("wrong secret: %d exchanges, want 1", exchanges)
	}

	limited := blogtest.New(t)
	limited.SetOverride(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path == "/oauth/token" {
			limited.Lock()
			limited.Exchanges++
			limited.Unlock()
			w.Header().Set("Retry-After", "1")
			jsonError(w, http.StatusTooManyRequests, "temporarily_unavailable")
			return true
		}
		return false
	})
	c2, sleeps := newClient(t, limited, nil)
	_, err = c2.ListPosts(context.Background(), 1, "")
	kind(t, err, client.KindRateLimited)
	if exchanges, _ := limited.Count(); exchanges != 3 || len(sleeps.waits) != 2 {
		t.Fatalf("rate-limited token endpoint: %d exchanges and %d waits, want 3 and 2", exchanges, len(sleeps.waits))
	}
}

// Markup-heavy articles within the limits must not be inflated past the
// request limit by HTML escaping.
func TestMarkupHeavyArticleFitsTheRequestLimit(t *testing.T) {
	srv := blogtest.New(t)
	c, _ := newClient(t, srv, nil)
	a := article("Markup heavy")
	a.Body = strings.Repeat("<p>&</p>", 61400/8)
	if _, err := c.CreatePost(context.Background(), "markup", a); err != nil {
		t.Fatalf("61 KB body refused: %v", err)
	}
	srv.Lock()
	defer srv.Unlock()
	// JSON's escaped form of "<": a backslash, then u003c.
	escaped := string(rune(92)) + "u003c"
	for _, seen := range srv.Seen {
		if strings.Contains(seen, escaped) {
			t.Fatal("request body was HTML-escaped")
		}
	}
}
