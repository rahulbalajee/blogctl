// Package client talks to the rahulbalajee.com Blog API (docs/api/blog-api.md)
// on behalf of an OAuth machine client: client-credentials tokens kept in
// memory, the draft operations and image uploads.
//
// It speaks only the documented wire contract and imports no server code; the
// rules package mirrors the site's slug, size and image limits so local checks
// match the server's. The server stays authoritative.
//
// Transport rules from the API contract are enforced here rather than left to
// callers: HTTPS (plain HTTP only for loopback test servers), IPv4 unless told
// otherwise because the edge allowlist is IPv4, no redirects, exact body hashes
// for CloudFront, timeouts and bounded response sizes. Basic credentials go
// only to the token endpoint, the bearer token only to /api/v1, and nothing
// from the site goes to the storage upload form. Writes are never replayed
// blindly: an unclear outcome is settled by reading the post back.
package client
