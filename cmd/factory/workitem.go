package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ContainerSolutions/factory/internal/apiclient"
)

func workitemCommand() command {
	return command{
		name:    "workitem",
		summary: "close or delete a work item (workitem close|delete <id>)",
		run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			return runWorkitem(ctx, args, os.Getenv, stdout, stderr)
		},
	}
}

const workitemUsage = "usage: factory workitem close <id> --reason done|wont-do|duplicate [--note text] [--server URL]\n" +
	"       factory workitem delete <id> [--force] [--server URL]"

func runWorkitem(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "close" && args[0] != "delete") {
		fmt.Fprintln(stderr, workitemUsage)
		return exitUsage
	}
	sub := args[0]
	name := "factory workitem " + sub
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(stderr)
	srvURL := fl.String("server", firstNonEmpty(getenv(envServer), defaultServer), "the dispatcher's base URL (env "+envServer+")")
	reason := fl.String("reason", "", "close: why it is closed: done, wont-do or duplicate")
	note := fl.String("note", "", "close: a note for the record")
	force := fl.Bool("force", false, "delete: cancel the item's active tasks and delete even while one is leased")
	fl.Usage = func() {
		fmt.Fprintln(stderr, workitemUsage)
		fmt.Fprintln(stderr, "\nclose sets status.closed, cancels the item's active tasks and stops the rules for it;")
		fmt.Fprintln(stderr, "delete removes it with its tasks, runs, effects and events. Both use the CLI token from "+envCLIToken+".")
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
		fmt.Fprintf(stderr, "%s: unexpected arguments %v\n", name, extra)
		return exitUsage
	}
	if id == "" {
		fmt.Fprintf(stderr, "%s: the work item id is required\n", name)
		return exitUsage
	}
	if sub == "close" && !apiclient.CloseWorkItemRequestReason(*reason).Valid() {
		fmt.Fprintf(stderr, "%s: --reason must be done, wont-do or duplicate\n", name)
		return exitUsage
	}
	c := cliClient(name, *srvURL, getenv, stderr)
	if c == nil {
		return exitUsage
	}
	var (
		out  any
		resp string
	)
	if sub == "close" {
		body := apiclient.CloseWorkItemRequest{Reason: apiclient.CloseWorkItemRequestReason(*reason)}
		if *note != "" {
			body.Note = note
		}
		var res *apiclient.CloseWorkItemResponse
		if res, err = c.CloseWorkItemWithResponse(ctx, id, body); err == nil {
			if res.JSON200 != nil {
				out = res.JSON200.Status.Closed
			} else {
				resp = describeProblem(res.HTTPResponse, res.Body)
			}
		}
	} else {
		var res *apiclient.DeleteWorkItemResponse
		if res, err = c.DeleteWorkItemWithResponse(ctx, id, &apiclient.DeleteWorkItemParams{Force: force}); err == nil {
			if res.JSON200 != nil {
				out = res.JSON200
			} else {
				resp = describeProblem(res.HTTPResponse, res.Body)
			}
		}
	}
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	case resp != "":
		fmt.Fprintf(stderr, "%s: %s\n", name, resp)
		return 1
	}
	if err := printJSON(stdout, out); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	return 0
}
