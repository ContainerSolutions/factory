package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ContainerSolutions/factory/internal/apiclient"
)

// runAgentEnrollments is factory agent enrollments [list|delete <id>]: list prints every
// enrollment as JSON (never a token); delete revokes one.
func runAgentEnrollments(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	sub := "list"
	if len(args) > 0 && (args[0] == "list" || args[0] == "delete") {
		sub, args = args[0], args[1:]
	}
	name := "factory agent enrollments " + sub
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(stderr)
	srvURL := fl.String("server", firstNonEmpty(getenv(envServer), defaultServer), "the dispatcher's base URL (env "+envServer+")")
	fl.Usage = func() {
		fmt.Fprintln(stderr, agentUsage)
		fl.PrintDefaults()
	}
	id, extra, err := parseIDArgs(fl, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	switch {
	case sub == "list" && id != "", len(extra) > 0:
		fmt.Fprintf(stderr, "%s: unexpected arguments\n", name)
		return exitUsage
	case sub == "delete" && id == "":
		fmt.Fprintf(stderr, "%s: the enrollment id is required\n", name)
		return exitUsage
	}
	c := cliClient(name, *srvURL, getenv, stderr)
	if c == nil {
		return exitUsage
	}
	if sub == "delete" {
		res, err := c.DeleteEnrollmentWithResponse(ctx, id)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "%s: %v\n", name, err)
			return 1
		case res.StatusCode() != 204:
			fmt.Fprintf(stderr, "%s: %s\n", name, describeProblem(res.HTTPResponse, res.Body))
			return 1
		}
		fmt.Fprintf(stderr, "%s: revoked %s\n", name, id)
		return 0
	}
	items := []apiclient.Enrollment{}
	params := &apiclient.ListEnrollmentsParams{}
	for {
		res, err := c.ListEnrollmentsWithResponse(ctx, params)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "%s: %v\n", name, err)
			return 1
		case res.JSON200 == nil:
			fmt.Fprintf(stderr, "%s: %s\n", name, describeProblem(res.HTTPResponse, res.Body))
			return 1
		}
		items = append(items, res.JSON200.Items...)
		if res.JSON200.NextCursor == nil || *res.JSON200.NextCursor == "" {
			break
		}
		params.Cursor = res.JSON200.NextCursor
	}
	if err := printJSON(stdout, items); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	return 0
}
