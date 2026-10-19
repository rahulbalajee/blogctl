package client

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Kind classifies a failure so callers can react without parsing messages.
type Kind int

const (
	// KindUnknown is any failure not covered below.
	KindUnknown Kind = iota
	// KindInvalid is a malformed or rejected input: a local check, or a 400,
	// 413, 415 or 422 from the API.
	KindInvalid
	// KindAuth means the credentials or token were rejected (401), or no
	// usable credentials were supplied.
	KindAuth
	// KindForbidden means authenticated but not allowed, such as a missing
	// scope (403 from the API).
	KindForbidden
	// KindNotFound covers missing posts and posts of other authors alike.
	KindNotFound
	// KindConflict is a stale revision or a slug that is already taken (409).
	KindConflict
	// KindRateLimited means a quota refused the request, even after the
	// bounded retries (429).
	KindRateLimited
	// KindUnavailable is a server, storage or network failure (5xx, timeouts,
	// refused connections). A write that failed this way was not applied.
	KindUnavailable
	// KindBlocked means the edge refused the request before the application:
	// an empty 403 when this machine's address is not on the allowlist, or an
	// empty 400 for a rejected path alias.
	KindBlocked
	// KindAmbiguous means a write may or may not have been applied and reading
	// the post back could not settle it.
	KindAmbiguous
)

// String names the kind as it appears in JSON error output.
func (k Kind) String() string {
	switch k {
	case KindInvalid:
		return "invalid"
	case KindAuth:
		return "auth"
	case KindForbidden:
		return "forbidden"
	case KindNotFound:
		return "not_found"
	case KindConflict:
		return "conflict"
	case KindRateLimited:
		return "rate_limited"
	case KindUnavailable:
		return "unavailable"
	case KindBlocked:
		return "blocked"
	case KindAmbiguous:
		return "ambiguous"
	default:
		return "unknown"
	}
}

// Error is a failed operation. It never contains credentials, tokens or
// upload form fields.
type Error struct {
	Kind Kind
	// Status is the HTTP status, or 0 when no response was received.
	Status int
	// Code is the API's JSON error code, if the response had one.
	Code string
	// Message explains the failure in plain words.
	Message string
	// RequestID is the X-Request-ID response header; it matches request_id
	// in the site's logs.
	RequestID string
	// RetryAfter is the server's requested wait on 429.
	RetryAfter time.Duration
	// CurrentRevision is the post's revision when a conflict was checked.
	CurrentRevision int64
	// Err is the underlying cause, if any.
	Err error
	// unsent marks a failure before the request left this machine, such as
	// a failed token exchange: a write that failed this way did not happen.
	unsent bool
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Message)
	if e.Status != 0 {
		fmt.Fprintf(&b, " (HTTP %d", e.Status)
		if e.Code != "" {
			fmt.Fprintf(&b, " %s", e.Code)
		}
		b.WriteString(")")
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " [request %s]", e.RequestID)
	}
	return b.String()
}

func (e *Error) Unwrap() error {
	return e.Err
}

// KindOf returns the kind of err, or KindUnknown if it is not an *Error.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindUnknown
}

func invalid(format string, args ...any) *Error {
	return &Error{Kind: KindInvalid, Message: fmt.Sprintf(format, args...)}
}
