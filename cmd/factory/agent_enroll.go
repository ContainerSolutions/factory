package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

// runAgentEnroll is factory agent enroll: it mints an enrollment (createEnrollment) and prints
// its one-time token alone on stdout, the enrollment on stderr. The token is shown only once.
func runAgentEnroll(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	const name = "factory agent enroll"
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(stderr)
	srvURL := fl.String("server", firstNonEmpty(getenv(envServer), defaultServer), "the dispatcher's base URL (env "+envServer+")")
	pool := fl.String("pool", "", "the Pool the agent joins (required)")
	owner := fl.String("owner", "", "the GitHub login of the owner who lends the agent (required)")
	host := fl.String("name", "", "the agent host's name: the agent is agt_<owner>-<name> (required)")
	ttl := fl.String("ttl", "", "how long the token may be used (default 7d, at most 30d)")
	fl.Usage = func() {
		fmt.Fprintln(stderr, "usage: factory agent enroll --pool <pool> --owner <login> --name <name> [--ttl 7d] [--server URL]")
		fmt.Fprintln(stderr, "\nMints a one-time enrollment token for an agent host and prints it once. Put it in a file")
		fmt.Fprintln(stderr, "on the host (0600) and point the agent's enrollmentTokenFile at it. Uses "+envCLIToken+".")
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	switch {
	case fl.NArg() > 0:
		fmt.Fprintf(stderr, "%s: unexpected arguments %v\n", name, fl.Args())
		return exitUsage
	case *pool == "" || *owner == "" || *host == "":
		fmt.Fprintf(stderr, "%s: --pool, --owner and --name are required\n", name)
		return exitUsage
	}
	req := apiclient.EnrollmentRequest{Pool: *pool, Owner: *owner, Name: *host}
	if *ttl != "" {
		d := v1alpha1.Duration(*ttl)
		if _, err := d.Parse(); err != nil {
			fmt.Fprintf(stderr, "%s: --ttl: %v\n", name, err)
			return exitUsage
		}
		req.Ttl = &d
	}
	c := cliClient(name, *srvURL, getenv, stderr)
	if c == nil {
		return exitUsage
	}
	res, err := c.CreateEnrollmentWithResponse(ctx, req)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	case res.JSON201 == nil:
		fmt.Fprintf(stderr, "%s: %s\n", name, describeProblem(res.HTTPResponse, res.Body))
		return 1
	}
	e := res.JSON201.Enrollment
	fmt.Fprintf(stderr, "%s: enrollment %s for agt_%s-%s in pool %s, usable until %s; the token below is shown only once\n",
		name, e.Id, e.Owner, e.Name, e.Pool, e.ExpiresAt.Format(time.RFC3339))
	fmt.Fprintln(stdout, res.JSON201.Token)
	return 0
}
