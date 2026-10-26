package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/apiserver/gen"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/scheduler"
	"github.com/compliance-framework/factory/internal/store"
)

// CodeTaskLeased is deleteWorkItem's 409 without force while one of the item's tasks is
// leased.
const CodeTaskLeased = "task-leased"

// maxCloseNote bounds closeWorkItem's note (CloseWorkItemRequest.note's maxLength).
const maxCloseNote = 1000

// CloseWorkItem implements gen.StrictServerInterface (api-server.md §Commands): it sets
// status.closed, cancels the item's active tasks and records a decision
// (controllers.CloseWorkItem).
func (s *Server) CloseWorkItem(ctx context.Context, req gen.CloseWorkItemRequestObject) (gen.CloseWorkItemResponseObject, error) {
	b := req.Body
	if b == nil || !b.Reason.Valid() {
		return nil, fieldError("reason", "done, wont-do or duplicate")
	}
	closed := v1alpha1.WorkItemClosed{Reason: v1alpha1.CloseReason(b.Reason), At: s.clk.Now(), By: principal(ctx).ActorID()}
	if b.Note != nil {
		if utf8.RuneCountInString(*b.Note) > maxCloseNote {
			return nil, fieldError("note", fmt.Sprintf("at most %d characters", maxCloseNote))
		}
		closed.Note = *b.Note
	}
	wi, cancelled, err := controllers.CloseWorkItem(ctx, s.d.Store, req.Id, closed)
	if errors.Is(err, store.ErrNotFound) {
		return nil, problemf(http.StatusNotFound, CodeNotFound, "no work item %q", req.Id)
	}
	if err != nil {
		return nil, err
	}
	s.log.InfoContext(ctx, "work item closed", "workItem", req.Id, "reason", closed.Reason, "actor", closed.By, "cancelled", cancelled)
	return gen.CloseWorkItem200JSONResponse(*wi), nil
}

// DeleteWorkItem implements gen.StrictServerInterface (api-server.md §Commands): it deletes the
// item and everything keyed to it (controllers.DeleteWorkItem), then the scheduler's leases of
// its tasks. A leased task refuses it (409 task-leased) unless force.
func (s *Server) DeleteWorkItem(ctx context.Context, req gen.DeleteWorkItemRequestObject) (gen.DeleteWorkItemResponseObject, error) {
	force := req.Params.Force != nil && *req.Params.Force
	del, err := controllers.DeleteWorkItem(ctx, s.d.Store, req.Id, force)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, problemf(http.StatusNotFound, CodeNotFound, "no work item %q", req.Id)
	case errors.Is(err, controllers.ErrTaskLeased):
		return nil, problemf(http.StatusConflict, CodeTaskLeased, "%v: cancel it first, or delete with force=true", err)
	case err != nil:
		return nil, err
	}
	for _, task := range del.Tasks {
		var leases []*scheduler.Lease
		if _, err := s.d.Store.List(ctx, scheduler.KindLease, store.ListOptions{
			LabelSelector: &v1alpha1.LabelSelector{MatchLabels: map[string]string{scheduler.LabelTask: task}},
		}, &leases); err != nil {
			return nil, fmt.Errorf("list leases of %s: %w", task, err)
		}
		for _, l := range leases {
			err := s.d.Store.Delete(ctx, store.Key{Kind: scheduler.KindLease, Name: l.Metadata.Name})
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("delete lease %s: %w", l.Metadata.Name, err)
			}
			if err == nil {
				del.Kinds[scheduler.KindLease]++
			}
		}
	}
	s.log.InfoContext(ctx, "work item deleted", "workItem", req.Id, "force", force, "deleted", del.Kinds, "actor", principal(ctx).ActorID())
	return gen.DeleteWorkItem200JSONResponse{WorkItem: req.Id, Deleted: del.Kinds}, nil
}
