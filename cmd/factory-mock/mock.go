package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/api"
)

// keepAlive is how often an open event stream gets a comment, as the real server does.
const keepAlive = 15 * time.Second

// route is one operation with its canned response, ready to serve.
type route struct {
	op       api.Operation
	segments []string // path template split on "/"; "{x}" matches any one segment
	params   int
	body     []byte   // non-SSE example body; nil when the response has none
	frames   [][]byte // encoded SSE frames
}

// mock answers each request with the example of the operation it matches.
type mock struct {
	routes []route
	log    *slog.Logger
}

// newMock prepares a route per operation, reading and rendering every example up front so a
// broken example fails at startup rather than on a request.
func newMock(ops []api.Operation, log *slog.Logger) (*mock, error) {
	m := &mock{log: log}
	for _, op := range ops {
		r := route{op: op, segments: strings.Split(op.Path, "/")}
		for _, s := range r.segments {
			if isParam(s) {
				r.params++
			}
		}
		switch {
		case !op.HasBody():
		case op.ContentType == api.MediaTypeSSE:
			frames, err := op.ExampleFrames()
			if err != nil {
				return nil, err
			}
			for _, f := range frames {
				b, err := f.Encode()
				if err != nil {
					return nil, fmt.Errorf("%s: %w", op.ID, err)
				}
				r.frames = append(r.frames, b)
			}
		default:
			b, err := op.ExampleBody()
			if err != nil {
				return nil, err
			}
			r.body = b
		}
		m.routes = append(m.routes, r)
	}
	return m, nil
}

func isParam(segment string) bool {
	return strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}")
}

// match reports whether the path fits the template.
func (r *route) match(segments []string) bool {
	if len(segments) != len(r.segments) {
		return false
	}
	for i, s := range r.segments {
		if isParam(s) {
			if segments[i] == "" {
				return false
			}
			continue
		}
		if s != segments[i] {
			return false
		}
	}
	return true
}

// find returns the most specific route for the method and path (fewest parameters), and the
// methods allowed on the path when none matches the method.
func (m *mock) find(method, path string) (*route, []string) {
	segments := strings.Split(path, "/")
	var best *route
	var allowed []string
	for i := range m.routes {
		r := &m.routes[i]
		if !r.match(segments) {
			continue
		}
		if r.op.Method != method {
			allowed = append(allowed, r.op.Method)
			continue
		}
		if best == nil || r.params < best.params {
			best = r
		}
	}
	slices.Sort(allowed)
	return best, slices.Compact(allowed)
}

func (m *mock) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r, allowed := m.find(req.Method, req.URL.Path)
	switch {
	case r != nil:
		m.log.Info("request", "method", req.Method, "path", req.URL.Path, "operation", r.op.ID, "status", r.op.Status)
		m.serve(w, req, r)
	case len(allowed) > 0:
		m.log.Info("request", "method", req.Method, "path", req.URL.Path, "status", http.StatusMethodNotAllowed)
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		problem(w, http.StatusMethodNotAllowed, "method-not-allowed", fmt.Sprintf("%s is not allowed on %s", req.Method, req.URL.Path))
	default:
		m.log.Info("request", "method", req.Method, "path", req.URL.Path, "status", http.StatusNotFound)
		problem(w, http.StatusNotFound, "not-found", fmt.Sprintf("no operation at %s", req.URL.Path))
	}
}

func (m *mock) serve(w http.ResponseWriter, req *http.Request, r *route) {
	if r.op.ContentType == api.MediaTypeSSE {
		m.stream(w, req, r)
		return
	}
	if r.op.Status >= 300 && r.op.Status < 400 {
		w.Header().Set("Location", "/")
	}
	if !r.op.HasBody() {
		w.WriteHeader(r.op.Status)
		return
	}
	w.Header().Set("Content-Type", r.op.ContentType)
	w.WriteHeader(r.op.Status)
	if _, err := w.Write(r.body); err != nil {
		m.log.Debug("write response", "operation", r.op.ID, "err", err)
	}
}

// stream sends the example events, then keeps the connection open (with keep-alive comments)
// until the client goes away.
func (m *mock) stream(w http.ResponseWriter, req *http.Request, r *route) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		problem(w, http.StatusInternalServerError, "streaming-unsupported", "the connection can't stream")
		return
	}
	w.Header().Set("Content-Type", api.MediaTypeSSE)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(r.op.Status)
	for _, f := range r.frames {
		if _, err := w.Write(f); err != nil {
			return
		}
	}
	flusher.Flush()
	tick := time.NewTicker(keepAlive)
	defer tick.Stop()
	for {
		select {
		case <-req.Context().Done():
			return
		case <-tick.C:
			if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// problemBody is an RFC 9457 problem, the shape of every error in the contract.
type problemBody struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func problem(w http.ResponseWriter, status int, code, detail string) {
	b, err := json.Marshal(problemBody{Type: "about:blank", Title: http.StatusText(status), Status: status, Code: code, Detail: detail})
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}
