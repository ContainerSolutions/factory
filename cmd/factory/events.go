package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

func eventsCommand() command {
	return command{
		name:    "events",
		summary: "replay a stored event through the rules (events replay <id>)",
		run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			return runEvents(ctx, args, os.Getenv, stdout, stderr)
		},
	}
}

func runEvents(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "replay" {
		fmt.Fprintln(stderr, "usage: factory events replay <event id> [--server URL]")
		return exitUsage
	}
	fl := flag.NewFlagSet("factory events replay", flag.ContinueOnError)
	fl.SetOutput(stderr)
	srvURL := fl.String("server", firstNonEmpty(getenv(envServer), defaultServer), "the dispatcher's base URL (env "+envServer+")")
	fl.Usage = func() {
		fmt.Fprintln(stderr, "usage: factory events replay <event id> [--server URL]")
		fmt.Fprintln(stderr, "\nPosts to <server>/api/v1/events/<id>/replay with the CLI token from "+envCLIToken+": the stored")
		fmt.Fprintln(stderr, "event is evaluated again against the live catalog, with its own id, so only the rules that act")
		fmt.Fprintln(stderr, "now and didn't then change anything. Prints the decisions the replay recorded, as JSON.")
		fl.PrintDefaults()
	}
	id, extra, err := parseIDArgs(fl, args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if len(extra) > 0 {
		fmt.Fprintf(stderr, "factory events replay: unexpected arguments %v\n", extra)
		return exitUsage
	}
	if id == "" {
		fmt.Fprintln(stderr, "factory events replay: the event id is required")
		return exitUsage
	}
	c := cliClient("factory events replay", *srvURL, getenv, stderr)
	if c == nil {
		return exitUsage
	}
	res, err := c.ReplayEventWithResponse(ctx, id)
	if err != nil {
		fmt.Fprintf(stderr, "factory events replay: %v\n", err)
		return 1
	}
	if res.JSON200 == nil {
		fmt.Fprintf(stderr, "factory events replay: %s\n", describeProblem(res.HTTPResponse, res.Body))
		return 1
	}
	if err := printJSON(stdout, res.JSON200); err != nil {
		fmt.Fprintf(stderr, "factory events replay: %v\n", err)
		return 1
	}
	return 0
}
