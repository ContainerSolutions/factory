package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":       {Data: []byte("<html>index</html>")},
		"assets/app-1.js":  {Data: []byte("console.log(1)")},
		"favicon.ico":      {Data: []byte("ico")},
		"assets/nested/.k": {Data: []byte("")},
	}
}

func TestHandler(t *testing.T) {
	h := newHandler(testFS())
	tests := []struct {
		name         string
		path         string
		wantStatus   int
		wantBody     string
		wantCache    string
		wantTypeHTML bool
	}{
		{name: "root serves index", path: "/", wantStatus: 200, wantBody: "index", wantCache: "no-cache", wantTypeHTML: true},
		{name: "client route falls back to index", path: "/workitems/wi_1", wantStatus: 200, wantBody: "index", wantCache: "no-cache", wantTypeHTML: true},
		{name: "explicit index.html", path: "/index.html", wantStatus: 200, wantBody: "index", wantCache: "no-cache", wantTypeHTML: true},
		{name: "hashed asset is immutable", path: "/assets/app-1.js", wantStatus: 200, wantBody: "console.log(1)", wantCache: "public, max-age=31536000, immutable"},
		{name: "other file served without immutable cache", path: "/favicon.ico", wantStatus: 200, wantBody: "ico", wantCache: ""},
		{name: "directory falls back to index", path: "/assets/nested", wantStatus: 200, wantBody: "index", wantCache: "no-cache", wantTypeHTML: true},
		{name: "traversal is cleaned", path: "/../index.html", wantStatus: 200, wantBody: "index", wantCache: "no-cache", wantTypeHTML: true},
		{name: "api root is not the UI", path: "/api", wantStatus: 404},
		{name: "api paths are not the UI", path: "/api/v1/agents", wantStatus: 404},
		{name: "api reached through dot segments is not the UI", path: "/./api/v1/agents", wantStatus: 404},
		{name: "hooks root is not the UI", path: "/hooks", wantStatus: 404},
		{name: "unknown hook path is not the UI", path: "/hooks/github-publik", wantStatus: 404},
		{name: "path merely starting with api is UI", path: "/apiary", wantStatus: 200, wantBody: "index", wantCache: "no-cache", wantTypeHTML: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus != 200 {
				return
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tt.wantBody)
			}
			if got := rec.Header().Get("Cache-Control"); got != tt.wantCache {
				t.Errorf("Cache-Control = %q, want %q", got, tt.wantCache)
			}
			if tt.wantTypeHTML && !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
				t.Errorf("Content-Type = %q, want text/html", rec.Header().Get("Content-Type"))
			}
		})
	}
}

func TestHandlerHeadOmitsBody(t *testing.T) {
	rec := httptest.NewRecorder()
	newHandler(testFS()).ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/", nil))
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("HEAD / = %d with %d body bytes, want 200 and empty body", rec.Code, rec.Body.Len())
	}
}

func TestHandlerMissingIndex(t *testing.T) {
	rec := httptest.NewRecorder()
	newHandler(fstest.MapFS{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestHandlerRejectsNonReadMethods(t *testing.T) {
	h := newHandler(testFS())
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantAllow  string
	}{
		{name: "POST to a client route", method: http.MethodPost, path: "/workitems/wi_1", wantStatus: http.StatusMethodNotAllowed, wantAllow: "GET, HEAD"},
		{name: "POST to root", method: http.MethodPost, path: "/", wantStatus: http.StatusMethodNotAllowed, wantAllow: "GET, HEAD"},
		{name: "PUT to an asset", method: http.MethodPut, path: "/assets/app-1.js", wantStatus: http.StatusMethodNotAllowed, wantAllow: "GET, HEAD"},
		{name: "POST to a mistyped webhook path", method: http.MethodPost, path: "/hooks/github-publik", wantStatus: http.StatusNotFound},
		{name: "POST to an api path", method: http.MethodPost, path: "/api/v1/commands", wantStatus: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Allow"); got != tt.wantAllow {
				t.Errorf("Allow = %q, want %q", got, tt.wantAllow)
			}
			if strings.Contains(rec.Body.String(), "index") {
				t.Errorf("body served the UI: %q", rec.Body.String())
			}
		})
	}
}
