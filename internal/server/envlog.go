package server

import (
	"context"
	"net/http"

	"github.com/compliance-framework/factory/internal/apiserver/gen"
)

// UploadEnvironmentLog implements gen.StrictServerInterface. A stub until the environment log
// lands (group 1, PR 5).
func (s *Server) UploadEnvironmentLog(context.Context, gen.UploadEnvironmentLogRequestObject) (gen.UploadEnvironmentLogResponseObject, error) {
	return nil, problemf(http.StatusNotImplemented, CodeNotImplemented, "the environment log is not implemented yet")
}

// GetEnvironmentLog implements gen.StrictServerInterface. A stub until the environment log
// lands (group 1, PR 5).
func (s *Server) GetEnvironmentLog(context.Context, gen.GetEnvironmentLogRequestObject) (gen.GetEnvironmentLogResponseObject, error) {
	return nil, problemf(http.StatusNotImplemented, CodeNotImplemented, "the environment log is not implemented yet")
}
