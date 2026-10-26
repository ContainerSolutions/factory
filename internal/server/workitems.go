package server

import (
	"context"
	"net/http"

	"github.com/compliance-framework/factory/internal/apiserver/gen"
)

// CloseWorkItem implements gen.StrictServerInterface. A stub until close lands (group 1,
// PR 3).
func (s *Server) CloseWorkItem(context.Context, gen.CloseWorkItemRequestObject) (gen.CloseWorkItemResponseObject, error) {
	return nil, problemf(http.StatusNotImplemented, CodeNotImplemented, "closing a work item is not implemented yet")
}

// DeleteWorkItem implements gen.StrictServerInterface. A stub until delete lands (group 1,
// PR 3).
func (s *Server) DeleteWorkItem(context.Context, gen.DeleteWorkItemRequestObject) (gen.DeleteWorkItemResponseObject, error) {
	return nil, problemf(http.StatusNotImplemented, CodeNotImplemented, "deleting a work item is not implemented yet")
}
