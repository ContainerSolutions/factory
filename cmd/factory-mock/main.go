// Command factory-mock serves the factory's HTTP contract (api/openapi.yaml) with canned
// responses from api/examples, for agent and web development before the real dispatcher
// exists. Each request is matched to its operation by method and path template and answered
// with that operation's example; unknown paths get 404 and a wrong method 405, both as
// application/problem+json. GET /api/v1/stream sends the example events and stays open.
//
// It uses plain net/http, not the generated Echo server, and checks nothing: no auth, no
// request validation.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/compliance-framework/factory/api"
	"github.com/compliance-framework/factory/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "factory-mock:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("factory-mock", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8081", "address to listen on")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version.String("factory-mock"))
		return nil
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	doc, err := api.Load(ctx)
	if err != nil {
		return err
	}
	ops, err := api.Operations(doc)
	if err != nil {
		return err
	}
	m, err := newMock(ops, log)
	if err != nil {
		return err
	}

	// Cancelled on shutdown, so open event streams end and Shutdown doesn't wait for them.
	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &http.Server{
		Addr:              *addr,
		Handler:           m,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}
	srv.RegisterOnShutdown(cancel)
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.Info("serving the factory API contract", "addr", ln.Addr().String(), "operations", len(ops))

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
