package tokens

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ContainerSolutions/factory/internal/provider"
)

// GraphQLURL is the GraphQL endpoint of a REST API base: https://api.github.com/graphql for
// github.com, https://<host>/api/graphql for GHE (whose REST base is https://<host>/api/v3),
// else <base>/graphql.
func GraphQLURL(api *url.URL) *url.URL {
	u := *api
	if p, ok := strings.CutSuffix(strings.TrimRight(u.Path, "/"), "/api/v3"); ok {
		u.Path = p + "/api/graphql"
		return &u
	}
	return u.JoinPath("graphql")
}

// GraphQLError is a GraphQL response's first error (GitHub answers 200 with "errors").
// errors.Is maps its type: NOT_FOUND → provider.ErrNotFound, FORBIDDEN (a field or object the
// token's permissions don't reach) → ErrForbidden and provider.ErrForbidden; a RATE_LIMITED
// error comes wrapped in a *provider.RetryableError instead.
type GraphQLError struct {
	// Op is the operation that failed ("Observe").
	Op string
	// Type is GitHub's error type (NOT_FOUND, FORBIDDEN, RATE_LIMITED, …), when it has one.
	Type string
	// Message is GitHub's message, truncated.
	Message string
	// Count is how many errors the response had.
	Count int
}

func (e *GraphQLError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "github: graphql %s", e.Op)
	if e.Type != "" {
		fmt.Fprintf(&b, ": %s", e.Type)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if e.Count > 1 {
		fmt.Fprintf(&b, " (and %d more errors)", e.Count-1)
	}
	return b.String()
}

// Is maps the type to the package's and the provider's errors.
func (e *GraphQLError) Is(target error) bool {
	switch e.Type {
	case "NOT_FOUND":
		return target == provider.ErrNotFound
	case "FORBIDDEN":
		return target == ErrForbidden || target == provider.ErrForbidden
	}
	return false
}

// GraphQL posts one query (named op, for errors and logs) with an installation token from
// Token and decodes the response's data into out. HTTP errors map as Get's do. A response with
// errors is a *GraphQLError, even when it carries partial data: an observation built from
// partial data could miss what rules decide on. The token, the query's variables and the
// response are never logged.
func (m *Minter) GraphQL(ctx context.Context, token, op, query string, vars map[string]any, out any) error {
	if token == "" {
		return fmt.Errorf("github: graphql %s: no token: %w", op, provider.ErrInvalid)
	}
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	body := map[string]any{"query": query, "variables": vars}
	h, err := m.sendURL(ctx, m.plain, token, http.MethodPost, m.graphql, body, http.StatusOK, &resp, "graphql "+op)
	if err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		first := resp.Errors[0]
		msg := first.Message
		if len(msg) > maxMessage {
			msg = msg[:maxMessage] + "…"
		}
		gqlErr := &GraphQLError{Op: op, Type: first.Type, Message: msg, Count: len(resp.Errors)}
		if first.Type == "RATE_LIMITED" {
			after, _ := m.rateLimit(&http.Response{Header: h}, msg)
			return &provider.RetryableError{RetryAfter: after, Err: gqlErr}
		}
		return gqlErr
	}
	if len(resp.Data) == 0 || string(resp.Data) == "null" {
		return fmt.Errorf("github: graphql %s: no data in the response", op)
	}
	if err := json.Unmarshal(resp.Data, out); err != nil {
		return fmt.Errorf("github: graphql %s: decode data: %w", op, err)
	}
	return nil
}
