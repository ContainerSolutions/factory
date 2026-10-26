package server

import (
	"context"
	"net/http"

	"github.com/compliance-framework/factory/internal/apiserver/gen"
)

// ReplayEvent implements gen.StrictServerInterface. A stub until the replay lands (group 1,
// PR 2).
func (s *Server) ReplayEvent(context.Context, gen.ReplayEventRequestObject) (gen.ReplayEventResponseObject, error) {
	return nil, problemf(http.StatusNotImplemented, CodeNotImplemented, "replaying an event is not implemented yet")
}
