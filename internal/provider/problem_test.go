package provider_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ContainerSolutions/factory/internal/provider"
)

func TestWriteProblem(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		code      string
		detail    string
		requestID string
		want      map[string]any
	}{
		{"a 4xx with its detail and instance", http.StatusUnauthorized, "unauthenticated", "missing or invalid X-Hub-Signature-256", "req-1",
			map[string]any{"status": float64(401), "title": "Unauthorized", "code": "unauthenticated", "detail": "missing or invalid X-Hub-Signature-256", "instance": "req-1"}},
		{"no request id", http.StatusBadRequest, "invalid-request", "unreadable body", "",
			map[string]any{"status": float64(400), "title": "Bad Request", "code": "invalid-request", "detail": "unreadable body"}},
		{"a 5xx shows no detail", http.StatusServiceUnavailable, "unavailable", "store down", "req-2",
			map[string]any{"status": float64(503), "title": "Service Unavailable", "code": "unavailable", "instance": "req-2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if tt.requestID != "" {
				rec.Header().Set("X-Request-Id", tt.requestID)
			}
			provider.WriteProblem(rec, tt.status, tt.code, tt.detail)
			if rec.Code != tt.status || rec.Header().Get("Content-Type") != provider.ProblemContentType {
				t.Errorf("answer %d %q", rec.Code, rec.Header().Get("Content-Type"))
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("problem = %v, want %v", got, tt.want)
			}
		})
	}
}
