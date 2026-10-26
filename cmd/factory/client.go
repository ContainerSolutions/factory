package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/server"
)

// cliClient is the API client of the CLI's admin commands (factory dispatch, events,
// workitem): srvURL must be an absolute http(s) URL and FACTORY_CLI_TOKEN set. On a usage
// error it says why on stderr, prefixed by name, and returns nil.
func cliClient(name, srvURL string, getenv func(string) string, stderr io.Writer) *apiclient.ClientWithResponses {
	token := getenv(envCLIToken)
	if token == "" {
		fmt.Fprintf(stderr, "%s: %s is not set\n", name, envCLIToken)
		return nil
	}
	base, err := url.Parse(srvURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		fmt.Fprintf(stderr, "%s: --server %q must be an absolute http(s) URL\n", name, srvURL)
		return nil
	}
	c, err := apiclient.NewClientWithResponses(strings.TrimRight(base.String(), "/")+server.BasePath,
		apiclient.WithHTTPClient(&http.Client{Timeout: dispatchTimeout}),
		apiclient.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
			r.Header.Set("Authorization", "Bearer "+token)
			return nil
		}))
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return nil
	}
	return c
}

// printJSON writes v to w as indented JSON.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
