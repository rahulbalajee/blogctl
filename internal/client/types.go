package client

import (
	"time"
)

// Article is a draft's content: the API's article object.
type Article struct {
	FormatVersion int      `json:"formatVersion"`
	Title         string   `json:"title"`
	Summary       string   `json:"summary"`
	Body          string   `json:"body"`
	Tags          []string `json:"tags"`
}

// wire returns the article as it is sent: version 1 and a non-nil tag list,
// because the server rejects JSON nulls.
func (a Article) wire() Article {
	a.FormatVersion = 1
	if a.Tags == nil {
		a.Tags = []string{}
	}
	return a
}

// Post is a draft as the API returns it. The API never exposes another
// author's posts, so every Post belongs to the authenticated client's Author.
type Post struct {
	Slug                 string    `json:"slug"`
	AuthorName           string    `json:"authorName"`
	Draft                Article   `json:"draft"`
	Revision             int64     `json:"revision"`
	CreatedAt            time.Time `json:"createdAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
	HasPublishedSnapshot bool      `json:"hasPublishedSnapshot"`
}

// Page is one page of the client's posts. NextCursor is opaque.
type Page struct {
	Posts      []Post `json:"posts"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// UploadGrant is a short-lived storage upload form. URL and Fields are
// credentials in their own right (they include a signature and session
// token): use them once, never log or print them.
type UploadGrant struct {
	URL        string            `json:"url"`
	Fields     map[string]string `json:"fields"`
	PreviewURL string            `json:"previewUrl"`
}

// TokenInfo describes the current access token without revealing it.
type TokenInfo struct {
	Scopes    []string
	ExpiresAt time.Time
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

type errorResponse struct {
	Error string `json:"error"`
}
