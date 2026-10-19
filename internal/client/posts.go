package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/rahulbalajee/blogctl/internal/rules"
)

// maxRequestBody is the API's JSON request limit.
const maxRequestBody = 100 << 10

// MaxListLimit is the largest page the API serves.
const MaxListLimit = 100

// ListPosts returns one page of the client's posts, newest first. limit is 1
// to MaxListLimit; cursor is "" for the first page.
func (c *Client) ListPosts(ctx context.Context, limit int, cursor string) (Page, error) {
	if limit < 1 || limit > MaxListLimit {
		return Page{}, invalid("limit must be between 1 and %d", MaxListLimit)
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	resp, err := c.send(ctx, request{
		method:     http.MethodGet,
		path:       "/api/v1/posts",
		query:      query,
		auth:       authBearer,
		maxBody:    maxListResponse,
		idempotent: true,
	})
	if err != nil {
		return Page{}, err
	}
	if resp.status != http.StatusOK {
		return Page{}, resp.failure("post list")
	}
	var page Page
	if err := decodeResponse(resp, &page); err != nil {
		return Page{}, err
	}
	return page, nil
}

// GetPost returns one of the client's posts.
func (c *Client) GetPost(ctx context.Context, slug string) (Post, error) {
	if !rules.ValidSlug(slug) {
		return Post{}, invalidSlug(slug)
	}
	resp, err := c.send(ctx, request{
		method:     http.MethodGet,
		path:       "/api/v1/posts/" + slug,
		auth:       authBearer,
		maxBody:    maxPostResponse,
		idempotent: true,
	})
	if err != nil {
		return Post{}, err
	}
	if resp.status != http.StatusOK {
		return Post{}, resp.failure(fmt.Sprintf("post %q", slug))
	}
	var post Post
	if err := decodeResponse(resp, &post); err != nil {
		return Post{}, err
	}
	return post, nil
}

// Result reports how a write ended. Reconciled is true when the response was
// lost or refused but reading the post back showed the write had been
// applied, for example by an earlier run of the same command.
type Result struct {
	Post       Post
	Reconciled bool
}

// CreatePost creates a draft. If the slug is already taken, or the outcome is
// unclear, the post is read back: it counts as created only if it is this
// client's, still at revision 1, with exactly the same content. The request is
// never replayed and no other slug is tried.
func (c *Client) CreatePost(ctx context.Context, slug string, article Article) (Result, error) {
	if !rules.ValidSlug(slug) {
		return Result{}, invalidSlug(slug)
	}
	article = article.wire()
	if err := CheckArticle(article); err != nil {
		return Result{}, err
	}
	body, err := encodeRequest(struct {
		Slug    string  `json:"slug"`
		Article Article `json:"article"`
	}{slug, article})
	if err != nil {
		return Result{}, err
	}
	resp, sendErr := c.send(ctx, request{
		method:      http.MethodPost,
		path:        "/api/v1/posts",
		body:        body,
		contentType: "application/json",
		auth:        authBearer,
		maxBody:     maxPostResponse,
	})
	what := fmt.Sprintf("post %q", slug)
	failure, settled := writeOutcome(ctx, resp, sendErr, http.StatusCreated, what)
	if settled != nil {
		return Result{Post: *settled}, nil
	}
	if failure.unsent || failure.Kind == KindAmbiguous || failure.Status != 0 && failure.Status != http.StatusConflict && !unclear(failure) {
		return Result{}, failure
	}
	existed := failure.Status == http.StatusConflict
	post, err := c.GetPost(ctx, slug)
	switch {
	case err == nil && post.Revision == 1 && sameContent(post.Draft, article):
		return Result{Post: post, Reconciled: true}, nil
	case err == nil && existed:
		failure.Kind = KindConflict
		failure.CurrentRevision = post.Revision
		failure.Message = fmt.Sprintf("slug %q already holds a different version of this client's post (revision %d); get it and update instead", slug, post.Revision)
	case err == nil && post.Revision == 1 && sameHeadline(post.Draft, article):
		// Probably this request, with the body changed by sanitizing.
		failure.Kind = KindAmbiguous
		failure.CurrentRevision = 1
		failure.Message = fmt.Sprintf("post %q now exists with this title, summary and tags but a different body (the site may have sanitized it); check with get", slug)
	case err == nil:
		failure.Kind = KindConflict
		failure.CurrentRevision = post.Revision
		failure.Message = fmt.Sprintf("slug %q holds a different post of this client (revision %d)", slug, post.Revision)
	case KindOf(err) == KindNotFound && existed:
		failure.Message = fmt.Sprintf("slug %q is taken by another author's post or a deleted one; choose another", slug)
	case KindOf(err) == KindNotFound && failure.Kind == KindUnavailable:
		failure.Message = fmt.Sprintf("post %q was not created; it is safe to run the command again", slug)
	default:
		failure.Kind = KindAmbiguous
		failure.Message = fmt.Sprintf("post %q may or may not have been created and could not be read back; check with get before retrying", slug)
	}
	return Result{}, failure
}

// UpdatePost replaces the draft at the expected revision. A 409 is reported
// as a conflict with the current revision; it is never treated as success.
// An unclear outcome is settled by reading the post back: applied only if it
// is now at revision+1 with exactly the same content.
func (c *Client) UpdatePost(ctx context.Context, slug string, revision int64, article Article) (Result, error) {
	if !rules.ValidSlug(slug) {
		return Result{}, invalidSlug(slug)
	}
	if revision < 1 {
		return Result{}, invalid("revision must be a positive number from get")
	}
	article = article.wire()
	if err := CheckArticle(article); err != nil {
		return Result{}, err
	}
	body, err := encodeRequest(struct {
		Revision int64   `json:"revision"`
		Article  Article `json:"article"`
	}{revision, article})
	if err != nil {
		return Result{}, err
	}
	resp, sendErr := c.send(ctx, request{
		method:      http.MethodPut,
		path:        "/api/v1/posts/" + slug,
		body:        body,
		contentType: "application/json",
		auth:        authBearer,
		maxBody:     maxPostResponse,
	})
	what := fmt.Sprintf("post %q", slug)
	failure, settled := writeOutcome(ctx, resp, sendErr, http.StatusOK, what)
	if settled != nil {
		return Result{Post: *settled}, nil
	}
	if failure.Status == http.StatusConflict {
		if post, err := c.GetPost(ctx, slug); err == nil {
			failure.CurrentRevision = post.Revision
			failure.Message = fmt.Sprintf("post %q changed: it is at revision %d, not %d; get it again and merge before updating", slug, post.Revision, revision)
		}
		return Result{}, failure
	}
	if failure.unsent || failure.Kind == KindAmbiguous || failure.Status != 0 && !unclear(failure) {
		return Result{}, failure
	}
	post, err := c.GetPost(ctx, slug)
	switch {
	case err == nil && post.Revision == revision+1 && sameContent(post.Draft, article):
		return Result{Post: post, Reconciled: true}, nil
	case err == nil && post.Revision == revision+1 && sameHeadline(post.Draft, article):
		failure.Kind = KindAmbiguous
		failure.CurrentRevision = post.Revision
		failure.Message = fmt.Sprintf("post %q is at revision %d with this title, summary and tags but a different body (the site may have sanitized it); check with get", slug, post.Revision)
	case err == nil && post.Revision == revision && failure.Kind == KindUnavailable:
		failure.Message = fmt.Sprintf("post %q was not updated; it is safe to run the command again", slug)
	case err == nil:
		failure.Kind = KindConflict
		failure.CurrentRevision = post.Revision
		failure.Message = fmt.Sprintf("post %q is at revision %d after an unclear update; get it and compare before retrying", slug, post.Revision)
	default:
		failure.Kind = KindAmbiguous
		failure.Message = fmt.Sprintf("post %q may or may not have been updated and could not be read back; check with get before retrying", slug)
	}
	return Result{}, failure
}

// writeOutcome sorts a write's response. It returns the saved post when the
// write plainly succeeded; otherwise the failure, which callers read back
// when unclear. An interrupted write cannot be read back and is ambiguous.
func writeOutcome(ctx context.Context, resp *response, sendErr error, success int, what string) (*Error, *Post) {
	if sendErr == nil && resp.status == success {
		var post Post
		if err := decodeResponse(resp, &post); err == nil {
			return nil, &post
		}
		// Applied, but the response is unreadable: settle it by reading back.
		return &Error{Kind: KindUnknown, Status: resp.status, RequestID: resp.requestID, Message: what + " was saved but the response was unreadable"}, nil
	}
	if ctx.Err() != nil {
		return &Error{Kind: KindAmbiguous, Message: "interrupted: " + what + " may or may not have been saved; check with get before retrying", Err: ctx.Err()}, nil
	}
	if sendErr != nil {
		var e *Error
		if errors.As(sendErr, &e) {
			return e, nil
		}
		return &Error{Kind: KindUnknown, Message: "the request failed", Err: sendErr}, nil
	}
	return resp.failure(what), nil
}

// unclear reports whether a write may have been applied despite failing: no
// response arrived, the site failed, or a success response was unusable.
func unclear(e *Error) bool {
	return e.Kind == KindUnavailable || e.Status >= 200 && e.Status < 300
}

// CheckArticle applies the server's article limits locally, so obvious
// mistakes fail without spending a mutation attempt. The server still
// sanitizes and validates the stored result.
func CheckArticle(a Article) error {
	title, summary := strings.TrimSpace(a.Title), strings.TrimSpace(a.Summary)
	switch {
	case len(title) < rules.MinTitleLength || len(title) > rules.MaxTitleLength:
		return invalid("title must be %d to %d bytes after trimming", rules.MinTitleLength, rules.MaxTitleLength)
	case len(summary) < rules.MinSummaryLength || len(summary) > rules.MaxSummaryLength:
		return invalid("summary must be %d to %d bytes after trimming", rules.MinSummaryLength, rules.MaxSummaryLength)
	case strings.TrimSpace(a.Body) == "" || len(a.Body) > rules.MaxBodyBytes:
		return invalid("body must be non-empty HTML of at most %d bytes", rules.MaxBodyBytes)
	case len(a.Tags) > rules.MaxTags:
		return invalid("at most %d tags are allowed", rules.MaxTags)
	}
	for _, tag := range a.Tags {
		if tag = strings.TrimSpace(tag); tag == "" || len(tag) > rules.MaxTagLength {
			return invalid("each tag must be 1 to %d bytes after trimming", rules.MaxTagLength)
		}
	}
	return nil
}

// sameContent reports whether the saved draft is exactly what was sent:
// sameHeadline plus an identical body.
func sameContent(saved, sent Article) bool {
	return sameHeadline(saved, sent) && saved.Body == sent.Body
}

// sameHeadline compares what the server stores without rewriting: trimmed
// title, summary and tags. Sanitizing can change the body's markup, so a
// headline match alone only suggests the same write.
func sameHeadline(saved, sent Article) bool {
	trim := func(tags []string) []string {
		out := make([]string, len(tags))
		for i, tag := range tags {
			out[i] = strings.TrimSpace(tag)
		}
		return out
	}
	return saved.Title == strings.TrimSpace(sent.Title) &&
		saved.Summary == strings.TrimSpace(sent.Summary) &&
		slices.Equal(trim(saved.Tags), trim(sent.Tags))
}

func invalidSlug(slug string) *Error {
	return invalid("slug %q must be lowercase letters and digits separated by single hyphens, at most %d bytes", slug, rules.MaxSlugLength)
}

// encodeRequest encodes v without HTML escaping: escaping turns each <, > and
// & of an article body into six bytes, which would push valid articles past
// the request limit.
func encodeRequest(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, &Error{Kind: KindUnknown, Message: "could not encode the request", Err: err}
	}
	body := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(body) > maxRequestBody {
		return nil, invalid("the request is %d bytes; the API accepts at most %d", len(body), maxRequestBody)
	}
	return body, nil
}

func decodeResponse(resp *response, v any) error {
	if err := json.Unmarshal(resp.body, v); err != nil {
		return &Error{Kind: KindUnknown, Status: resp.status, RequestID: resp.requestID, Message: "the response was not the expected JSON", Err: err}
	}
	return nil
}
