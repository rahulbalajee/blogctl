// Package rules holds the Blog API's documented input limits, so blogctl can
// reject obvious mistakes before spending a request. The site stays
// authoritative; these values come from its published contract
// (docs/api/blog-api.md and blog-api.openapi.json in rahulbalajee.com), and
// rules_test.go checks them against the copy in testdata. When the site
// changes a limit, refresh that copy with `make sync-contract`.
package rules

import "regexp"

// MaxSlugLength bounds post slugs, which are lowercase letters and digits in
// hyphen-separated groups.
const MaxSlugLength = 96

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// ValidSlug reports whether slug is a well-formed post slug.
func ValidSlug(slug string) bool {
	return len(slug) <= MaxSlugLength && slugPattern.MatchString(slug)
}

// Article limits, in bytes after trimming.
const (
	MinTitleLength   = 3
	MaxTitleLength   = 160
	MinSummaryLength = 10
	MaxSummaryLength = 300
	MaxBodyBytes     = 80 << 10
	MaxTags          = 5
	MaxTagLength     = 32
	MaxArticleImages = 8
)

// Image limits for uploads.
const (
	MaxImageSize      = 3 << 20
	MaxImageDimension = 4000
)

// ImageExtension reports whether contentType is an accepted upload type and
// the extension the site stores it under.
func ImageExtension(contentType string) (string, bool) {
	switch contentType {
	case "image/png":
		return "png", true
	case "image/jpeg":
		return "jpg", true
	default:
		return "", false
	}
}
