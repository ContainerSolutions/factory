package pipeline

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/compliance-framework/factory/internal/agent/runner"
)

// MaxEnvironmentLog caps the environment log a VM workspace's run uploads (the VM caps it at
// envrun.MaxLog; past this cap it is cut on a line and ends with EnvironmentLogTruncated).
const MaxEnvironmentLog = 10 << 20

// EnvironmentLogTruncated is the line that ends a cut environment log.
const EnvironmentLogTruncated = "[truncated]\n"

// StoreEnvironmentLog uploads a workspace's environment log (out/environment.log, untrusted)
// as the transcript is: redacted again with the job's secrets, extra (the supervisor's model
// logins) and every token shape, and capped at MaxEnvironmentLog, through the job's dispatcher
// client (PUT /runs/{id}/environment-log). A lease that names no run, or a job without a
// client, uploads nothing. Its errors name the request.
func (p *Pipeline) StoreEnvironmentLog(ctx context.Context, job *runner.Job, log []byte, extra ...string) error {
	runID := job.Lease.Lease.Run
	if runID == nil || *runID == "" || job.API == nil || len(log) == 0 {
		return nil
	}
	body := capEnvironmentLog((&redactor{job: job, extra: extra}).String(string(log)))
	res, err := job.API.UploadEnvironmentLogWithTextBodyWithResponse(ctx, *runID, body)
	if err != nil {
		return fmt.Errorf("PUT /runs/%s/environment-log: %w", *runID, err)
	}
	if res.StatusCode() != http.StatusOK || res.JSON200 == nil {
		return apiError("PUT /runs/"+*runID+"/environment-log", res.StatusCode(), res.Body)
	}
	return nil
}

// capEnvironmentLog cuts s after its last whole line within MaxEnvironmentLog, then adds
// EnvironmentLogTruncated.
func capEnvironmentLog(s string) string {
	if len(s) <= MaxEnvironmentLog {
		return s
	}
	cut := s[:MaxEnvironmentLog-len(EnvironmentLogTruncated)]
	if i := strings.LastIndexByte(cut, '\n'); i >= 0 {
		cut = cut[:i+1]
	} else {
		cut += "\n"
	}
	return cut + EnvironmentLogTruncated
}
