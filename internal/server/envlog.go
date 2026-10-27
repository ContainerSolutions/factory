package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/ContainerSolutions/factory/internal/apiserver/gen"
	"github.com/ContainerSolutions/factory/internal/store"
)

// EnvironmentLogRefPrefix starts an environment log reference (uploadEnvironmentLog's
// environmentLogRef, Run.status.environmentLogRef).
const EnvironmentLogRefPrefix = "blob://environment-logs/"

// UploadEnvironmentLog implements gen.StrictServerInterface: the agent holding the run's lease
// stores its workspace's environment log (plain text, redacted by the agent) beside the
// transcript, under the transcripts' caps and retention, and the run records its reference.
func (s *Server) UploadEnvironmentLog(ctx context.Context, req gen.UploadEnvironmentLogRequestObject) (gen.UploadEnvironmentLogResponseObject, error) {
	run, err := s.heldRun(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if r := requestFrom(ctx); r != nil {
		if ct := r.Header.Get(echo.HeaderContentType); ct != "" && !strings.HasPrefix(ct, "text/plain") {
			return nil, problemf(http.StatusUnsupportedMediaType, "unsupported-media-type", "the environment log must be text/plain")
		}
	}
	if req.Body == nil {
		return nil, problemf(http.StatusBadRequest, CodeInvalid, "no environment log")
	}
	data := []byte(*req.Body)
	if int64(len(data)) > s.opts.TranscriptLimit {
		return nil, problemf(http.StatusRequestEntityTooLarge, CodeTooLarge, "the environment log is over %d bytes", s.opts.TranscriptLimit)
	}
	if err := s.d.Transcripts.PutTranscript(ctx, store.EnvironmentLogBlob(run.Metadata.Name), data); err != nil {
		return nil, err
	}
	ref := EnvironmentLogRefPrefix + run.Metadata.Name + ".log"
	err = s.updateRun(ctx, run.Metadata.Name, func(r *gen.Run) bool {
		if r.Status == nil {
			r.Status = &gen.RunStatus{}
		}
		if r.Status.EnvironmentLogRef == ref {
			return false
		}
		r.Status.EnvironmentLogRef = ref
		return true
	})
	if err != nil {
		return nil, err
	}
	return gen.UploadEnvironmentLog200JSONResponse{EnvironmentLogRef: ref, Bytes: int64(len(data))}, nil
}

// GetEnvironmentLog implements gen.StrictServerInterface: the stored environment log, as
// uploaded, for a UI reader or the CLI.
func (s *Server) GetEnvironmentLog(ctx context.Context, req gen.GetEnvironmentLogRequestObject) (gen.GetEnvironmentLogResponseObject, error) {
	if _, _, err := s.runTask(ctx, req.Id); err != nil {
		return nil, err
	}
	data, err := s.d.Transcripts.GetTranscript(ctx, store.EnvironmentLogBlob(req.Id))
	if errors.Is(err, store.ErrNotFound) {
		return nil, problemf(http.StatusNotFound, CodeNotFound, "run %s has no environment log", req.Id)
	}
	if err != nil {
		return nil, err
	}
	return gen.GetEnvironmentLog200TextResponse(data), nil
}
