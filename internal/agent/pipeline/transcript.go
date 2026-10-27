package pipeline

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/ContainerSolutions/factory/internal/agent/runner"
)

// Redacted replaces every secret in a transcript, a progress summary or an error.
const Redacted = "[REDACTED]"

// secretPatterns are token shapes redacted wherever they appear (run-pipeline.md §Steps
// 11): GitHub tokens (ghs_, gho_, ghp_, ghu_, ghr_, github_pat_), API keys (sk-…), private
// key blocks and Authorization header values.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bgh[opsur]_[A-Za-z0-9_]{16,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{16,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
}

// authHeaderRE is an Authorization header's value (its scheme and credential), as written in
// a log, a curl command or an escaped JSON string.
var authHeaderRE = regexp.MustCompile(`(?i)((?:proxy-)?authorization["']?\s*[:=]\s*["']?)(?:(?:basic|bearer|token|digest)\s+)?[^\s"'\\,;]+`)

// redactor removes secrets: the run's own (the git tokens, read-only and refreshed ones
// included, and the agent token), any extra ones (the supervisor's model logins) and anything
// shaped like one.
type redactor struct {
	job   *runner.Job
	extra []string
}

func newRedactor(job *runner.Job) *redactor { return &redactor{job: job} }

// secrets are the run's current secrets.
func (r *redactor) secrets() []string {
	var out []string
	for _, t := range r.extra {
		if len(t) >= 4 {
			out = append(out, t)
		}
	}
	if r.job == nil {
		return out
	}
	if r.job.Git != nil {
		if t := r.job.Git.Get().Token; len(t) >= 4 {
			out = append(out, t)
		}
		for _, c := range r.job.Git.Reads() {
			if len(c.Token) >= 4 {
				out = append(out, c.Token)
			}
		}
	}
	if r.job.AgentToken != nil {
		if t := r.job.AgentToken(); len(t) >= 4 {
			out = append(out, t)
		}
	}
	if t := r.job.Lease.RunToken; t != nil && len(*t) >= 4 {
		out = append(out, *t)
	}
	return out
}

// RedactShapes redacts s by the token shapes alone, as a run that holds no token of its own
// (a VM workspace's) does.
func RedactShapes(s string) string { return newRedactor(nil).String(s) }

// Redact redacts s as the run's redactor does: job's secrets (its git tokens, its agent token,
// read now, and the lease's run token), extra (secrets the job doesn't hold, such as the supervisor's model
// logins) and every token shape. The supervisor redacts what a VM workspace reports with it.
func Redact(job *runner.Job, s string, extra ...string) string {
	return (&redactor{job: job, extra: extra}).String(s)
}

// String redacts s.
func (r *redactor) String(s string) string {
	for _, sec := range r.secrets() {
		s = strings.ReplaceAll(s, sec, Redacted)
	}
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, Redacted)
	}
	return authHeaderRE.ReplaceAllString(s, "${1}"+Redacted)
}

// Error redacts err's text: the result's Error() is the redacted text, and it unwraps to
// err, so errors.Is and errors.As still match. nil stays nil.
func (r *redactor) Error(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{msg: r.String(err.Error()), err: err}
}

// redactedError is an error whose text was redacted; it wraps the original.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// factoryLine is a transcript line the pipeline itself writes.
func factoryLine(event string, data map[string]any) []byte {
	m := map[string]any{"type": "factory." + event}
	for k, v := range data {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return append(b, '\n')
}

// gzip framing overhead allowance: the header, the trailer and a sync flush.
const gzipSlack = 64

// buildTranscript joins the parts' lines into gzip-compressed JSON Lines of at most limit
// bytes. Every line is redacted; a line that isn't JSON is kept as {"text": line}. When the
// cap is reached the remaining lines are dropped and a factory.transcript-truncated line
// says how many.
func buildTranscript(parts [][]byte, red *redactor, limit int64) ([]byte, error) {
	var lines [][]byte
	for _, p := range parts {
		sc := bufio.NewScanner(bytes.NewReader(p))
		sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			s := red.String(string(line))
			var b []byte
			if json.Valid([]byte(s)) {
				b = []byte(s)
			} else {
				b, _ = json.Marshal(map[string]string{"text": s})
			}
			lines = append(lines, append(b, '\n'))
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("transcript: %w", err)
		}
	}
	marker := func(dropped int) []byte {
		return factoryLine("transcript-truncated", map[string]any{"droppedLines": dropped, "limitBytes": limit})
	}
	reserve := int64(len(marker(len(lines)))) + 2*gzipSlack
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	pending := int64(0) // bytes written since the last flush, compressed size unknown
	dropped := 0
	for i, l := range lines {
		n := int64(len(l)) + gzipSlack
		if int64(buf.Len())+pending+n+reserve > limit {
			if err := zw.Flush(); err != nil {
				return nil, err
			}
			pending = 0
			if int64(buf.Len())+n+reserve > limit {
				dropped = len(lines) - i
				break
			}
		}
		if _, err := zw.Write(l); err != nil {
			return nil, err
		}
		pending += int64(len(l))
	}
	if dropped > 0 {
		if _, err := zw.Write(marker(dropped)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > limit {
		return nil, fmt.Errorf("transcript: %d bytes over the %d-byte cap", buf.Len(), limit)
	}
	return buf.Bytes(), nil
}

// StoreTranscript is the host's transcript step for a run that happened elsewhere (a VM
// workspace's, whose transcript the supervisor holds as untrusted data): parts' lines are
// redacted with job's secrets and extra, capped at TranscriptLimit as buildTranscript does,
// and handed to the TranscriptStore (APITranscripts: PUT /runs/{id}/transcript). It returns
// the reference.
func (p *Pipeline) StoreTranscript(ctx context.Context, job *runner.Job, parts [][]byte, extra ...string) (string, error) {
	body, err := buildTranscript(parts, &redactor{job: job, extra: extra}, p.o.TranscriptLimit)
	if err != nil {
		return "", err
	}
	return p.o.Transcripts.Store(ctx, job, body)
}

// storeTranscript builds the run's transcript and hands it to the TranscriptStore
// (APITranscripts: PUT /runs/{id}/transcript), returning its reference.
func (r *run) storeTranscript(ctx context.Context) (string, error) {
	body, err := buildTranscript(r.parts, r.red, r.p.o.TranscriptLimit)
	if err != nil {
		return "", err
	}
	ref, err := r.p.o.Transcripts.Store(ctx, r.job, body)
	if err != nil {
		return "", err
	}
	if ref != "" {
		r.log.Info("transcript uploaded", "bytes", len(body))
	}
	return ref, nil
}
