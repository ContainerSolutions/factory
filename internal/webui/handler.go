// Package webui serves the Factory UI embedded in the dispatcher binary.
package webui

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Handler serves the embedded UI as a single-page app: existing files are
// served as-is (hashed assets under /assets/ are cached as immutable), and any
// other path that isn't under /api/ falls back to index.html so client-side
// routes work. index.html is never cached.
func Handler() http.Handler {
	sub, err := fs.Sub(assets, root)
	if err != nil {
		// Unreachable while root matches the embed directive; fail requests, not the process.
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "ui not available", http.StatusInternalServerError)
		})
	}
	return newHandler(sub)
}

func newHandler(files fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name != "" && name != "index.html" {
			if st, err := fs.Stat(files, name); err == nil && !st.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		serveIndex(w, r, files)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, files fs.FS) {
	body, err := fs.ReadFile(files, "index.html")
	if err != nil {
		http.Error(w, "ui not available", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}
