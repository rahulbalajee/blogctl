// Package blogtest is a fake Blog API, OAuth token endpoint and storage upload
// endpoint for blogctl's tests. It checks every request the way the real edge
// and API would: exact body hashes, Basic credentials only at the token
// endpoint, a valid bearer token only on /api/v1, and no site credentials at
// all on the storage upload.
package blogtest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rahulbalajee/blogctl/internal/client"
)

// Credentials the fake accepts.
const (
	ClientID     = "1111111111111111111111111111111111111111111111111111111111111111"
	ClientSecret = "2222222222222222222222222222222222222222222222222222222222222222"
)

// Upload is one request the storage endpoint received.
type Upload struct {
	Header http.Header
	Fields []string
	// LastPart is the name of the final form part; storage needs the file last.
	LastPart    string
	FileName    string
	FileSize    int
	ContentType string
}

// Server is the fake. Override, when set, sees every request first and
// handles it by returning true.
type Server struct {
	*httptest.Server
	t testing.TB

	mu        sync.Mutex
	Override  func(w http.ResponseWriter, r *http.Request, body []byte) bool
	Scope     string
	ExpiresIn int
	// Exchanges counts token requests; APIRequests counts /api/v1 requests.
	Exchanges   int
	APIRequests int
	Tokens      []string
	Posts       map[string]client.Post
	Uploads     []Upload
	JWKSBlocked bool
	// Seen collects every request header and body, to scan for leaks.
	Seen []string
}

// New starts a fake on a loopback port; it closes when the test ends.
func New(t testing.TB) *Server {
	s := &Server{t: t, Scope: "drafts:read drafts:write media:write", ExpiresIn: 600, Posts: map[string]client.Post{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Lock and Unlock guard the fields tests read while the server runs.
func (s *Server) Lock()   { s.mu.Lock() }
func (s *Server) Unlock() { s.mu.Unlock() }

// SetOverride replaces the request hook; nil removes it.
func (s *Server) SetOverride(fn func(w http.ResponseWriter, r *http.Request, body []byte) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Override = fn
}

// SetJWKSBlocked makes the key set answer like an edge block.
func (s *Server) SetJWKSBlocked(blocked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.JWKSBlocked = blocked
}

// Count returns the token exchanges and API requests so far.
func (s *Server) Count() (exchanges, api int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Exchanges, s.APIRequests
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.Seen = append(s.Seen, fmt.Sprint(r.Header), string(body))
	override := s.Override
	s.mu.Unlock()
	if override != nil && override(w, r, body) {
		return
	}
	if len(body) > 0 {
		// CloudFront's origin access control refuses a missing or wrong hash
		// with an empty 403.
		sum := sha256.Sum256(body)
		if r.Header.Get("x-amz-content-sha256") != hex.EncodeToString(sum[:]) && !strings.HasPrefix(r.URL.Path, "/s3/") {
			s.t.Errorf("%s %s: x-amz-content-sha256 does not match the body", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
	}
	switch {
	case r.URL.Path == "/.well-known/jwks.json":
		s.mu.Lock()
		blocked := s.JWKSBlocked
		s.mu.Unlock()
		if blocked {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": []any{}})
	case r.URL.Path == "/oauth/token":
		s.token(w, r, body)
	case strings.HasPrefix(r.URL.Path, "/api/v1/"):
		s.api(w, r, body)
	case strings.HasPrefix(r.URL.Path, "/s3/"):
		s.upload(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

func (s *Server) token(w http.ResponseWriter, r *http.Request, body []byte) {
	s.mu.Lock()
	s.Exchanges++
	s.mu.Unlock()
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		s.t.Errorf("token request: %s with %q", r.Method, r.Header.Get("Content-Type"))
	}
	if form, err := url.ParseQuery(string(body)); err != nil || form.Get("grant_type") != "client_credentials" {
		s.t.Errorf("token request body %q", body)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(ClientID)+":"+url.QueryEscape(ClientSecret)))
	if r.Header.Get("Authorization") != want {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	s.mu.Lock()
	issued := fmt.Sprintf("tok%d.%s", len(s.Tokens)+1, strings.Repeat("x", 40))
	s.Tokens = append(s.Tokens, issued)
	scope, expires := s.Scope, s.ExpiresIn
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"access_token": issued, "token_type": "Bearer", "expires_in": expires, "scope": scope})
}

func (s *Server) api(w http.ResponseWriter, r *http.Request, body []byte) {
	s.mu.Lock()
	s.APIRequests++
	valid := false
	for _, t := range s.Tokens {
		valid = valid || r.Header.Get("Authorization") == "Bearer "+t
	}
	s.mu.Unlock()
	if !valid {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	if len(body) > 0 && r.Header.Get("Content-Type") != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "unsupported_media_type"})
		return
	}
	slug := strings.TrimPrefix(r.URL.Path, "/api/v1/posts/")
	switch {
	case r.URL.Path == "/api/v1/posts" && r.Method == http.MethodGet:
		s.mu.Lock()
		page := client.Page{Posts: []client.Post{}}
		slugs := make([]string, 0, len(s.Posts))
		for slug := range s.Posts {
			slugs = append(slugs, slug)
		}
		sort.Strings(slugs)
		for _, slug := range slugs {
			page.Posts = append(page.Posts, s.Posts[slug])
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, page)
	case r.URL.Path == "/api/v1/posts" && r.Method == http.MethodPost:
		var input struct {
			Slug    string         `json:"slug"`
			Article client.Article `json:"article"`
		}
		if !strictJSON(body, &input) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, taken := s.Posts[input.Slug]; taken {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "conflict"})
			return
		}
		post := newPost(input.Slug, input.Article, 1)
		s.Posts[input.Slug] = post
		writeJSON(w, http.StatusCreated, post)
	case r.URL.Path == "/api/v1/media/sign" && r.Method == http.MethodPost:
		var input struct {
			Slug        string `json:"slug"`
			ContentType string `json:"contentType"`
			Size        int    `json:"size"`
		}
		if !strictJSON(body, &input) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		name := strings.Repeat("a", 64) + ".png"
		writeJSON(w, http.StatusOK, client.UploadGrant{
			URL:        s.URL + "/s3/upload",
			Fields:     map[string]string{"key": "draft/" + input.Slug + "/" + name, "policy": "signed-policy", "x-amz-signature": "signature"},
			PreviewURL: "/api/admin/media/" + input.Slug + "/" + name,
		})
	case slug != r.URL.Path && r.Method == http.MethodGet:
		s.mu.Lock()
		post, ok := s.Posts[slug]
		s.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		writeJSON(w, http.StatusOK, post)
	case slug != r.URL.Path && r.Method == http.MethodPut:
		var input struct {
			Revision int64          `json:"revision"`
			Article  client.Article `json:"article"`
		}
		if !strictJSON(body, &input) || input.Revision < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		post, ok := s.Posts[slug]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		if post.Revision != input.Revision {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "conflict"})
			return
		}
		post = newPost(slug, input.Article, input.Revision+1)
		s.Posts[slug] = post
		writeJSON(w, http.StatusOK, post)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	record := Upload{Header: r.Header.Clone()}
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		s.t.Errorf("upload content type: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// The body was already read for the leak scan; parse it from Seen.
	s.mu.Lock()
	raw := s.Seen[len(s.Seen)-1]
	s.mu.Unlock()
	reader := multipart.NewReader(strings.NewReader(raw), params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.t.Errorf("upload form: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(part)
		record.LastPart = part.FormName()
		if part.FileName() != "" {
			record.FileName, record.FileSize, record.ContentType = part.FileName(), len(data), part.Header.Get("Content-Type")
		} else {
			record.Fields = append(record.Fields, part.FormName()+"="+string(data))
		}
	}
	s.mu.Lock()
	s.Uploads = append(s.Uploads, record)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// Put stores a post directly, as if created earlier.
func (s *Server) Put(post client.Post) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Posts[post.Slug] = post
}

func newPost(slug string, a client.Article, revision int64) client.Post {
	tags := make([]string, 0, len(a.Tags))
	for _, tag := range a.Tags {
		tags = append(tags, strings.TrimSpace(tag))
	}
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	return client.Post{
		Slug:       slug,
		AuthorName: "Robot",
		Draft: client.Article{
			FormatVersion: 1,
			Title:         strings.TrimSpace(a.Title),
			Summary:       strings.TrimSpace(a.Summary),
			Body:          a.Body,
			Tags:          tags,
		},
		Revision:  revision,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// strictJSON mirrors the API's decoder: no unknown fields and no nulls.
func strictJSON(body []byte, v any) bool {
	if strings.Contains(string(body), "null") {
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(v) == nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", "fake-request-id")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
