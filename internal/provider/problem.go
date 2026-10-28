package provider

import (
	"encoding/json"
	"net/http"

	"github.com/ContainerSolutions/factory/internal/apiserver/gen"
)

// ProblemContentType is the media type of the API's error bodies (RFC 9457), which a Conn's
// webhook Handler answers its refusals with too.
const ProblemContentType = "application/problem+json"

// WriteProblem answers a refused webhook delivery with an RFC 9457 problem, in the API
// server's error shape (api/openapi.yaml's Problem): the status, its text as the title, code
// (one of internal/server's codes), detail (left out of a 5xx, as the server leaves it out),
// and the request id the server set on the response (X-Request-Id) as the instance.
func WriteProblem(w http.ResponseWriter, status int, code, detail string) {
	p := gen.Problem{Status: status, Title: http.StatusText(status), Code: &code}
	if detail != "" && status < http.StatusInternalServerError {
		p.Detail = &detail
	}
	if id := w.Header().Get("X-Request-Id"); id != "" {
		p.Instance = &id
	}
	w.Header().Set("Content-Type", ProblemContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p) // a write error means the client went away
}
