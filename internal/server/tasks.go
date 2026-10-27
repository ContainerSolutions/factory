package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
	"github.com/ContainerSolutions/factory/internal/apiserver/gen"
	"github.com/ContainerSolutions/factory/internal/auth"
	"github.com/ContainerSolutions/factory/internal/controllers"
	"github.com/ContainerSolutions/factory/internal/provider"
	"github.com/ContainerSolutions/factory/internal/store"
)

// maxCAS bounds compare-and-swap retries.
const maxCAS = 10

// trustTrusted is a context item the factory trusts (its content isn't fenced).
const trustTrusted = "trusted"

// TranscriptRefPrefix starts a transcript reference (uploadTranscript's transcriptRef).
const TranscriptRefPrefix = "blob://transcripts/"

func (s *Server) getTask(ctx context.Context, id string) (*v1alpha1.Task, error) {
	var t v1alpha1.Task
	if err := s.d.Store.Get(ctx, store.Key{Kind: v1alpha1.KindTask, Name: id}, &t); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, problemf(http.StatusNotFound, CodeNotFound, "no task %q", id)
		}
		return nil, fmt.Errorf("get task %s: %w", id, err)
	}
	return &t, nil
}

// holds reports whether agent holds t's active lease (Leased, Running or Cancelling).
func holds(t *v1alpha1.Task, agent string) bool {
	if t.Status == nil || t.Status.Agent == nil || *t.Status.Agent != agent {
		return false
	}
	switch t.Status.Phase {
	case v1alpha1.TaskLeased, v1alpha1.TaskRunning, v1alpha1.TaskCancelling:
		return true
	}
	return false
}

// leasedTask returns the task the calling agent holds the active lease of: another agent's
// (or no one's) task, and for a run token another task than its lease's, is 403
// not-lease-holder.
func (s *Server) leasedTask(ctx context.Context, id string) (*v1alpha1.Task, error) {
	agent, task, err := leaseCaller(ctx)
	if err != nil {
		return nil, err
	}
	if task != "" && task != id {
		return nil, problemf(http.StatusForbidden, CodeNotLeaseHolder, "the run token is for another task")
	}
	t, err := s.getTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if !holds(t, agent) {
		return nil, problemf(http.StatusForbidden, CodeNotLeaseHolder, "the agent doesn't hold the lease of task %s", id)
	}
	return t, nil
}

// GetTask implements gen.StrictServerInterface.
func (s *Server) GetTask(ctx context.Context, req gen.GetTaskRequestObject) (gen.GetTaskResponseObject, error) {
	var (
		t   *v1alpha1.Task
		err error
	)
	if principal(ctx).Kind == auth.KindAgent {
		t, err = s.leasedTask(ctx, req.Id)
	} else {
		t, err = s.getTask(ctx, req.Id)
	}
	if err != nil {
		return nil, err
	}
	return gen.GetTask200JSONResponse(*t), nil
}

// GetTaskPrompt implements gen.StrictServerInterface.
func (s *Server) GetTaskPrompt(ctx context.Context, req gen.GetTaskPromptRequestObject) (gen.GetTaskPromptResponseObject, error) {
	if _, err := s.leasedTask(ctx, req.Id); err != nil {
		return nil, err
	}
	var p controllers.TaskPrompt
	if err := s.d.Store.Get(ctx, store.Key{Kind: controllers.KindTaskPrompt, Name: req.Id}, &p); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, problemf(http.StatusNotFound, CodeNotFound, "task %s has no rendered prompt", req.Id)
		}
		return nil, fmt.Errorf("get prompt of %s: %w", req.Id, err)
	}
	resp := gen.GetTaskPrompt200TextResponse{Body: p.Text}
	if p.Digest != "" {
		d := p.Digest
		resp.Headers.Digest = &d
	}
	return resp, nil
}

// MediaTypeSchemaJSON is getTaskOutputSchema's media type.
const MediaTypeSchemaJSON = "application/schema+json"

// GetTaskOutputSchema implements gen.StrictServerInterface: the output schema the task's
// Result is validated against on complete (controllers.ResultSpec: the live catalog's), as
// the catalog holds it. An agent may read only the task it holds the lease of, and a run token
// only its lease's task; UI and CLI any task (getTask's rule).
func (s *Server) GetTaskOutputSchema(ctx context.Context, req gen.GetTaskOutputSchemaRequestObject) (gen.GetTaskOutputSchemaResponseObject, error) {
	var (
		t   *v1alpha1.Task
		err error
	)
	if k := principal(ctx).Kind; k == auth.KindAgent || k == auth.KindRun {
		t, err = s.leasedTask(ctx, req.Id)
	} else {
		t, err = s.getTask(ctx, req.Id)
	}
	if err != nil {
		return nil, err
	}
	ts := controllers.ResultSpec(s.d.Catalog.Current(), t)
	if ts == nil || ts.OutputSchemaDocument == nil {
		return nil, problemf(http.StatusNotFound, CodeNotFound, "task %s's TaskSpec %q declares no output schema", req.Id, t.Spec.TaskSpec.Name)
	}
	return rawSchema(ts.OutputSchemaDocument), nil
}

// rawSchema writes a schema document's bytes unchanged (the generated response would
// re-encode it as a map, reordering its keys).
type rawSchema []byte

func (r rawSchema) VisitGetTaskOutputSchemaResponse(w http.ResponseWriter) error {
	w.Header().Set(echo.HeaderContentType, MediaTypeSchemaJSON)
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(r)
	return err
}

// ContentReader is implemented by provider connections that can read an object's untrusted
// text (title, body, comments) for a task's context items. provider.Conn has no such method
// yet (Observe carries no untrusted text), so a connection without it yields reference-only
// items.
type ContentReader interface {
	ReadContent(ctx context.Context, ref v1alpha1.ObjectRef) (Content, error)
}

// Content is an object's text, as a ContentReader returns it.
type Content struct {
	MediaType string
	Text      string
	// Author is who wrote it (login), for the fence's label.
	Author string
}

// GetTaskContext implements gen.StrictServerInterface: context item n, with untrusted content
// fenced and labelled.
func (s *Server) GetTaskContext(ctx context.Context, req gen.GetTaskContextRequestObject) (gen.GetTaskContextResponseObject, error) {
	t, err := s.leasedTask(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if req.N < 0 || req.N >= len(t.Spec.Context) {
		return nil, problemf(http.StatusNotFound, CodeNotFound, "task %s has no context item %d", req.Id, req.N)
	}
	item := t.Spec.Context[req.N]
	trust := item.Trust
	if trust == "" {
		trust = controllers.TrustUntrusted
	}
	out := gen.TaskContext{Index: req.N, Kind: item.Kind, Ref: item.Ref, Trust: trust}
	if item.Provider != "" {
		p := item.Provider
		out.Provider = &p
	}
	if conn, ok := s.d.Conns.Conn(item.Provider); ok && item.Kind != "repo" {
		if cr, ok := conn.(ContentReader); ok {
			c, err := cr.ReadContent(ctx, v1alpha1.ObjectRef{Provider: item.Provider, Kind: item.Kind, Ref: item.Ref})
			switch {
			case errors.Is(err, provider.ErrNotFound):
				return nil, problemf(http.StatusNotFound, CodeNotFound, "context item %d is gone", req.N)
			case err != nil:
				return nil, fmt.Errorf("read context %d of %s: %w", req.N, req.Id, err)
			}
			out.Content = c.Text
			if trust != trustTrusted {
				out.Content = fence(item.Provider+":"+item.Ref, c.Author, c.Text)
			}
			if c.MediaType != "" {
				mt := c.MediaType
				out.MediaType = &mt
			}
		}
	}
	return gen.GetTaskContext200JSONResponse(out), nil
}

// fence wraps untrusted text in a code fence longer than any backtick run inside it, labelled
// with its source (the agent re-wraps it in <untrusted>): the text can't close the fence.
func fence(source, author, text string) string {
	n, run := 4, 0
	for _, r := range text {
		if r == '`' {
			run++
			n = max(n, run+1)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", n)
	label := fmt.Sprintf("untrusted source=%q", source)
	if author != "" {
		label += fmt.Sprintf(" author=%q", author)
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return f + label + "\n" + text + f + "\n"
}

// runTask returns the run and its task.
func (s *Server) runTask(ctx context.Context, runID string) (*v1alpha1.Run, *v1alpha1.Task, error) {
	var run v1alpha1.Run
	if err := s.d.Store.Get(ctx, store.Key{Kind: v1alpha1.KindRun, Name: runID}, &run); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, problemf(http.StatusNotFound, CodeNotFound, "no run %q", runID)
		}
		return nil, nil, fmt.Errorf("get run %s: %w", runID, err)
	}
	t, err := s.getTask(ctx, run.Spec.Task)
	if err != nil {
		return nil, nil, err
	}
	return &run, t, nil
}

// heldRun checks that the calling agent holds the lease of the run's task and that the run is
// its current one (agent and attempt). A run token takes only its own run: the open run of
// its lease's task.
func (s *Server) heldRun(ctx context.Context, runID string) (*v1alpha1.Run, error) {
	agent, task, err := leaseCaller(ctx)
	if err != nil {
		return nil, err
	}
	run, t, err := s.runTask(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !holds(t, agent) || run.Spec.Agent != agent {
		return nil, problemf(http.StatusForbidden, CodeNotLeaseHolder, "the agent doesn't hold the lease of run %s", runID)
	}
	if task != "" {
		open, err := s.openRun(ctx, task)
		if err != nil {
			return nil, err
		}
		if run.Spec.Task != task || open == nil || open.Metadata.Name != run.Metadata.Name {
			return nil, problemf(http.StatusForbidden, CodeNotLeaseHolder, "the run token is for another run")
		}
	}
	return run, nil
}

// UploadTranscript implements gen.StrictServerInterface.
func (s *Server) UploadTranscript(ctx context.Context, req gen.UploadTranscriptRequestObject) (gen.UploadTranscriptResponseObject, error) {
	run, err := s.heldRun(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if r := requestFrom(ctx); r != nil {
		if ct := r.Header.Get(echo.HeaderContentType); ct != "" && !strings.HasPrefix(ct, "application/gzip") {
			return nil, problemf(http.StatusUnsupportedMediaType, "unsupported-media-type", "the transcript must be application/gzip")
		}
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, s.opts.TranscriptLimit+1))
	if err != nil {
		var he *echo.HTTPError
		if errors.As(err, &he) {
			return nil, he // the body limit
		}
		return nil, problemf(http.StatusBadRequest, CodeInvalid, "unreadable body")
	}
	if int64(len(data)) > s.opts.TranscriptLimit {
		return nil, problemf(http.StatusRequestEntityTooLarge, CodeTooLarge, "the transcript is over %d bytes", s.opts.TranscriptLimit)
	}
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return nil, problemf(http.StatusBadRequest, CodeInvalid, "the transcript is not gzip-compressed")
	}
	if err := s.d.Transcripts.PutTranscript(ctx, run.Metadata.Name, data); err != nil {
		return nil, err
	}
	ref := TranscriptRefPrefix + run.Metadata.Name + ".jsonl.gz"
	if err := s.setTranscriptRef(ctx, run.Metadata.Name, ref); err != nil {
		return nil, err
	}
	return gen.UploadTranscript200JSONResponse{TranscriptRef: ref, Bytes: int64(len(data))}, nil
}

// setTranscriptRef records the transcript on the run.
func (s *Server) setTranscriptRef(ctx context.Context, runID, ref string) error {
	return s.updateRun(ctx, runID, func(r *v1alpha1.Run) bool {
		if r.Status == nil {
			r.Status = &v1alpha1.RunStatus{}
		}
		if r.Status.TranscriptRef == ref {
			return false
		}
		r.Status.TranscriptRef = ref
		return true
	})
}

// updateRun applies mutate to the stored run with compare-and-swap.
func (s *Server) updateRun(ctx context.Context, runID string, mutate func(*v1alpha1.Run) bool) error {
	for range maxCAS {
		var r v1alpha1.Run
		if err := s.d.Store.Get(ctx, store.Key{Kind: v1alpha1.KindRun, Name: runID}, &r); err != nil {
			return fmt.Errorf("get run %s: %w", runID, err)
		}
		if !mutate(&r) {
			return nil
		}
		err := s.d.Store.Update(ctx, &r)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("update run %s: %w", runID, err)
		}
		return nil
	}
	return fmt.Errorf("update run %s: %w", runID, store.ErrConflict)
}

// GetTranscript implements gen.StrictServerInterface: a UI reader, or the agent holding the
// run's lease.
func (s *Server) GetTranscript(ctx context.Context, req gen.GetTranscriptRequestObject) (gen.GetTranscriptResponseObject, error) {
	if principal(ctx).Kind == auth.KindAgent {
		if _, err := s.heldRun(ctx, req.Id); err != nil {
			return nil, err
		}
	} else if _, _, err := s.runTask(ctx, req.Id); err != nil {
		return nil, err
	}
	data, err := s.d.Transcripts.GetTranscript(ctx, req.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, problemf(http.StatusNotFound, CodeNotFound, "run %s has no transcript", req.Id)
	}
	if err != nil {
		return nil, err
	}
	return gen.GetTranscript200ApplicationgzipResponse{Body: bytes.NewReader(data), ContentLength: int64(len(data))}, nil
}
