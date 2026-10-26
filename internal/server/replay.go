package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/compliance-framework/factory/internal/apiserver/gen"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/store"
)

// ReplayEvent implements gen.StrictServerInterface (api-server.md §Commands): the rule
// controller evaluates the stored event again with its own id and applies what differs from
// its earlier decisions; the response holds the decisions it recorded (each Replay).
func (s *Server) ReplayEvent(ctx context.Context, req gen.ReplayEventRequestObject) (gen.ReplayEventResponseObject, error) {
	if s.d.Replayer == nil {
		return nil, problemf(http.StatusServiceUnavailable, CodeUnavailable, "event replay is not available")
	}
	if _, err := s.catalog(); err != nil {
		return nil, err
	}
	res, err := s.d.Replayer.Replay(ctx, req.Id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, problemf(http.StatusNotFound, CodeNotFound, "no event %q", req.Id)
	case errors.Is(err, controllers.ErrNoCatalog):
		return nil, problemf(http.StatusServiceUnavailable, CodeCatalogNotLoaded, "the catalog is not loaded yet")
	case err != nil:
		return nil, err
	}
	s.log.InfoContext(ctx, "event replayed", "event", req.Id, "workItem", res.WorkItem,
		"decisions", len(res.Decisions), "actor", principal(ctx).ActorID())
	out := gen.ReplayEvent200JSONResponse{Event: req.Id, Decisions: res.Decisions}
	if out.Decisions == nil {
		out.Decisions = []gen.Decision{}
	}
	if res.WorkItem != "" {
		wi := res.WorkItem
		out.WorkItem = &wi
	}
	return out, nil
}
