package server

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/labstack/echo/v4"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/store"
)

// registerOps mounts the ops routes on the ops Echo (recover and access log only, no auth): it
// must be served on a localhost listener, never the public address.
func (s *Server) registerOps() {
	s.ops.GET("/healthz", func(c echo.Context) error { return c.String(http.StatusOK, "ok\n") })
	s.ops.GET("/readyz", s.readyz)
	s.ops.GET("/metrics", s.metrics)
}

// StoreReadiness is a store that says whether it can serve: loaded, synced with the shared
// state within its maximum lag, and still writing (the object store). A store without it
// (memory, the file store) has nothing to sync: it is ready once it is loaded.
type StoreReadiness interface {
	Ready() error
}

// ready reports whether this instance is ready, the same on every instance, leader or not: the
// catalog is loaded and the store is ready (StoreReadiness). code is the problem code when it
// isn't.
func (s *Server) ready() (ok bool, code, why string) {
	if !s.catalogStatus().Ready || s.d.Catalog.Current() == nil {
		return false, CodeCatalogNotLoaded, "catalog not loaded"
	}
	if r, isR := s.d.Store.(StoreReadiness); isR {
		if err := r.Ready(); err != nil {
			if errors.Is(err, store.ErrReadOnly) {
				return false, CodeStoreReadOnly, err.Error()
			}
			return false, CodeUnavailable, err.Error()
		}
	}
	return true, "", ""
}

func (s *Server) readyz(c echo.Context) error {
	if ok, code, why := s.ready(); !ok {
		return problemf(http.StatusServiceUnavailable, code, "not ready: %s", why)
	}
	return c.String(http.StatusOK, "ready\n")
}

// metrics writes the Prometheus text exposition format (no client library: a few gauges and
// counters, computed on scrape).
func (s *Server) metrics(c echo.Context) error {
	ctx := c.Request().Context()
	var b strings.Builder
	gauge := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	counter := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	}

	var tasks []*v1alpha1.Task
	if _, err := s.d.Store.List(ctx, v1alpha1.KindTask, store.ListOptions{}, &tasks); err != nil {
		return err
	}
	byPhase := map[v1alpha1.TaskPhase]int{}
	for _, t := range tasks {
		p := v1alpha1.TaskPending
		if t.Status != nil {
			p = t.Status.Phase
		}
		byPhase[p]++
	}
	gauge("factory_tasks", "Tasks by phase.")
	for _, p := range v1alpha1.TaskPhases() {
		fmt.Fprintf(&b, "factory_tasks{phase=%q} %d\n", p, byPhase[p])
	}

	var agents []*v1alpha1.Agent
	if _, err := s.d.Store.List(ctx, v1alpha1.KindAgent, store.ListOptions{}, &agents); err != nil {
		return err
	}
	agentPhase := map[v1alpha1.AgentPhase]int{}
	for _, a := range agents {
		p := v1alpha1.AgentReady
		if a.Status != nil {
			p = a.Status.Phase
		}
		agentPhase[p]++
	}
	gauge("factory_agents", "Registered agents by phase.")
	for _, p := range v1alpha1.AgentPhases() {
		fmt.Fprintf(&b, "factory_agents{phase=%q} %d\n", p, agentPhase[p])
	}

	gauge("factory_sse_subscribers", "Open SSE streams.")
	fmt.Fprintf(&b, "factory_sse_subscribers %d\n", s.subscribers.Load())

	ready := 0
	if ok, _, _ := s.ready(); ok {
		ready = 1
	}
	gauge("factory_ready", "1 when the catalog and the store are loaded, the store has synced within its maximum lag and still writes (/readyz).")
	fmt.Fprintf(&b, "factory_ready %d\n", ready)

	counter("factory_controller_panics_total", "Reconcile calls that panicked, by controller.")
	for _, ctrl := range s.opts.Controllers {
		fmt.Fprintf(&b, "factory_controller_panics_total{controller=%q} %d\n", ctrl.Name(), ctrl.Panics())
	}

	counter("factory_events_dropped_total", "Provider events the intake filter dropped (no work item links their object and no rule could fire), by provider, type and reason.")
	if s.opts.Intake != nil {
		for _, d := range s.opts.Intake.Drops() {
			fmt.Fprintf(&b, "factory_events_dropped_total{provider=%q,type=%q,reason=%q} %d\n", d.Provider, d.Type, d.Reason, d.Count)
		}
	}

	counter("factory_http_requests_total", "HTTP requests by status code.")
	var codes []int
	s.requests.Range(func(k, _ any) bool {
		codes = append(codes, k.(int))
		return true
	})
	sort.Ints(codes)
	for _, code := range slices.Compact(codes) {
		v, _ := s.requests.Load(code)
		fmt.Fprintf(&b, "factory_http_requests_total{code=%q} %d\n", strconv.Itoa(code), v.(*atomic.Int64).Load())
	}
	return c.Blob(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(b.String()))
}
