package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/apiclient"
)

// The dispatcher's API base path and the registration credential headers (contracts.md §HTTP
// API).
const (
	BasePath         = "/api/v1"
	HeaderJoinToken  = "X-Factory-Join-Token"
	HeaderOwnerToken = "X-Factory-Owner-Token"
)

// APIBase is the API base URL of a dispatcher.
func APIBase(dispatcher string) string { return strings.TrimRight(dispatcher, "/") + BasePath }

// APIError is a dispatcher answer other than the operation's success. It carries the
// problem's code and text, never a request's credentials.
type APIError struct {
	Op     string
	Status int
	Code   string
	Title  string
	Detail string
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d", e.Op, e.Status)
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	if t := firstNonEmpty(e.Detail, e.Title); t != "" {
		b.WriteString(": " + t)
	}
	return b.String()
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// apiError builds an APIError from a response and its body (an application/problem+json
// Problem when the dispatcher sent one).
func apiError(op string, res *http.Response, body []byte) error {
	if res == nil {
		return fmt.Errorf("%s: no response", op)
	}
	e := &APIError{Op: op, Status: res.StatusCode}
	var p apiclient.Problem
	if json.Unmarshal(body, &p) == nil {
		e.Title = p.Title
		if p.Code != nil {
			e.Code = *p.Code
		}
		if p.Detail != nil {
			e.Detail = *p.Detail
		}
	}
	return e
}

func statusOf(err error) int {
	var e *APIError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// isUnauthorized: the agent token was refused (expired or revoked).
func isUnauthorized(err error) bool { return statusOf(err) == http.StatusUnauthorized }

// isGone: on a lease route, the lease isn't ours any more (404 / 409, and 403
// not-lease-holder after a re-lease).
func isGone(err error) bool {
	switch statusOf(err) {
	case http.StatusNotFound, http.StatusConflict, http.StatusForbidden:
		return true
	}
	return false
}

// isTransient: worth retrying (network errors, timeouts, 5xx, 408, 429).
func isTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var pe *permanentError
	if errors.As(err, &pe) {
		return false
	}
	s := statusOf(err)
	return s == 0 || s >= 500 || s == http.StatusRequestTimeout || s == http.StatusTooManyRequests
}

// permanentError is a failure retrying won't fix (no join token, the owner refused the
// login).
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// callTimeout bounds every dispatcher call but the long poll.
const callTimeout = 30 * time.Second

// backoff is exponential backoff without jitter (a handful of agents per pool).
type backoff struct{ min, max, cur time.Duration }

func (b *backoff) next() time.Duration {
	if b.cur == 0 {
		b.cur = b.min
	} else {
		b.cur = min(2*b.cur, b.max)
	}
	return b.cur
}

func (b *backoff) reset() { b.cur = 0 }
