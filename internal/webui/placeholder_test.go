//go:build !webui

package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedPlaceholderServes(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "make web") {
		t.Fatalf("embedded placeholder: status %d, body %q", rec.Code, rec.Body.String())
	}
}
