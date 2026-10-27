package pipeline

import (
	"context"
	"fmt"
	"net/http"

	"github.com/ContainerSolutions/factory/internal/agent/runbundle"
	"github.com/ContainerSolutions/factory/internal/agent/runner"
)

// maxSchema bounds a served output schema: runbundle's bound, which Write and Read apply too.
const maxSchema = runbundle.MaxSchemaBytes

// SchemaSource gives a task's spec-specific output schema (the TaskSpec's output.schema, a
// JSON Schema document). A nil document means the TaskSpec declares none: the Result is then
// checked against the v1alpha1 Result schema and the task's outcomes only, as the dispatcher
// checks it on complete.
type SchemaSource interface {
	OutputSchema(ctx context.Context, job *runner.Job) ([]byte, error)
}

// APISchemas is the default SchemaSource: GET /tasks/{id}/output-schema (getTaskOutputSchema)
// through the job's dispatcher client, the schema the dispatcher validates the Result with on
// complete. Only a 404 means no schema; any other failure fails the run, since a Result
// checked without the schema could be refused on complete without its repair turn.
type APISchemas struct{}

// OutputSchema implements SchemaSource.
func (APISchemas) OutputSchema(ctx context.Context, job *runner.Job) ([]byte, error) {
	if job.API == nil {
		return nil, errNoClient
	}
	id := job.TaskID()
	op := "GET /tasks/" + id + "/output-schema"
	res, err := job.API.GetTaskOutputSchemaWithResponse(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	switch res.StatusCode() {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, apiError(op, res.StatusCode(), res.Body)
	}
	if len(res.Body) > maxSchema {
		return nil, fmt.Errorf("%s: the schema is over %d bytes", op, maxSchema)
	}
	return res.Body, nil
}
