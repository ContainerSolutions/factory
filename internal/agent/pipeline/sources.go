package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apiclient"
)

// PromptSource gives a task's rendered prompt template and its context items
// (workspace-run.md §Sources). renderPrompt keeps the digest check, the size caps, the
// preamble, the fencing and the tickets; a source only fetches. Its errors fail the prompt
// step as they are, so each names its operation (APIPrompts: the request).
type PromptSource interface {
	// Prompt is the rendered template, byte-exact (its digest is checked against
	// spec.prompt.digest).
	Prompt(ctx context.Context, job *runner.Job) ([]byte, error)
	// Context is the non-repo context item i. gone is true when the item no longer exists
	// (the API's 404); c is then ignored.
	Context(ctx context.Context, job *runner.Job, i int) (c apiclient.TaskContext, gone bool, err error)
}

// APIPrompts is the default PromptSource: GET /tasks/{id}/prompt and GET
// /tasks/{id}/context/{i} through the job's dispatcher client.
type APIPrompts struct{}

var _ PromptSource = APIPrompts{}

// errNoClient is a job without a dispatcher client.
var errNoClient = errors.New("no dispatcher client")

// Prompt implements PromptSource.
func (APIPrompts) Prompt(ctx context.Context, job *runner.Job) ([]byte, error) {
	if job.API == nil {
		return nil, errNoClient
	}
	id := job.TaskID()
	res, err := job.API.GetTaskPromptWithResponse(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("GET /tasks/%s/prompt: %w", id, err)
	}
	if res.StatusCode() != http.StatusOK {
		return nil, apiError("GET /tasks/"+id+"/prompt", res.StatusCode(), res.Body)
	}
	return res.Body, nil
}

// Context implements PromptSource.
func (APIPrompts) Context(ctx context.Context, job *runner.Job, i int) (apiclient.TaskContext, bool, error) {
	if job.API == nil {
		return apiclient.TaskContext{}, false, errNoClient
	}
	id := job.TaskID()
	res, err := job.API.GetTaskContextWithResponse(ctx, id, i)
	if err != nil {
		return apiclient.TaskContext{}, false, fmt.Errorf("GET /tasks/%s/context/%d: %w", id, i, err)
	}
	switch {
	case res.StatusCode() == http.StatusNotFound:
		return apiclient.TaskContext{}, true, nil
	case res.StatusCode() != http.StatusOK || res.JSON200 == nil:
		return apiclient.TaskContext{}, false, apiError(fmt.Sprintf("GET /tasks/%s/context/%d", id, i), res.StatusCode(), res.Body)
	}
	return *res.JSON200, false, nil
}

// TranscriptStore keeps a run's transcript: gz is the redacted, capped, gzipped JSON Lines
// buildTranscript made. ref is the Outcome's TranscriptRef; "" when the store returns none
// (nothing was stored, or someone else uploads it). Its errors are logged as they are, so
// each names its operation (APITranscripts: the request).
type TranscriptStore interface {
	Store(ctx context.Context, job *runner.Job, gz []byte) (ref string, err error)
}

// APITranscripts is the default TranscriptStore: PUT /runs/{id}/transcript through the
// job's dispatcher client. A lease that names no run, or a job without a client, stores
// nothing and returns "".
type APITranscripts struct{}

var _ TranscriptStore = APITranscripts{}

// Store implements TranscriptStore.
func (APITranscripts) Store(ctx context.Context, job *runner.Job, gz []byte) (string, error) {
	runID := job.Lease.Lease.Run
	if runID == nil || *runID == "" || job.API == nil {
		return "", nil
	}
	res, err := job.API.UploadTranscriptWithBodyWithResponse(ctx, *runID, "application/gzip", bytes.NewReader(gz))
	if err != nil {
		return "", fmt.Errorf("PUT /runs/%s/transcript: %w", *runID, err)
	}
	if res.StatusCode() != http.StatusOK || res.JSON200 == nil {
		return "", apiError("PUT /runs/"+*runID+"/transcript", res.StatusCode(), res.Body)
	}
	return res.JSON200.TranscriptRef, nil
}
