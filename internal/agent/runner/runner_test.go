package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

const secret = "ghs_secret-token-value"

func TestCredentialsRedacted(t *testing.T) {
	c := NewCredentials(apiclient.GitCredential{Token: secret, Repo: "o/r", Host: "github.com", ExpiresAt: time.Unix(0, 0).UTC()})
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("cred", "c", c)
	j, err := json.Marshal(struct{ C *Credentials }{c})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s", c, c, c, c), c.String(), logs.String(), string(j)} {
		if strings.Contains(s, secret) || !strings.Contains(s, "REDACTED") {
			t.Errorf("leaks or isn't redacted: %s", s)
		}
	}
	c.Set(apiclient.GitCredential{Token: "new"})
	if c.Get().Token != "new" {
		t.Error("Set didn't swap the credential")
	}
}

type sink struct {
	progress []v1alpha1.ProgressEvent
	usage    []UsageDelta
}

func (s *sink) Progress(e v1alpha1.ProgressEvent) { s.progress = append(s.progress, e) }
func (s *sink) Usage(u UsageDelta)                { s.usage = append(s.usage, u) }

func job(id string) *Job {
	return &Job{Lease: apiclient.Lease{Task: v1alpha1.Task{Metadata: v1alpha1.TaskMeta{ID: id}}}}
}

func TestStub(t *testing.T) {
	s := &Stub{
		Outcome:  Outcome{Result: &v1alpha1.Result{Status: v1alpha1.ResultDone, Outcome: "triaged"}, TranscriptRef: "tr"},
		Progress: []v1alpha1.ProgressEvent{{Kind: "turn"}},
		Usage:    &UsageDelta{InputTokens: 3},
	}
	started := s.Started()
	var k sink
	out, err := s.Run(context.Background(), job("tsk_1"), &k)
	if err != nil || out.Result.Task != "tsk_1" || out.Result.Outcome != "triaged" || out.TranscriptRef != "tr" || out.Status != StatusDone {
		t.Fatalf("Run = %+v, %v", out, err)
	}
	if s.Outcome.Result.Task != "" {
		t.Error("the canned Result was changed")
	}
	if len(k.progress) != 1 || len(k.usage) != 1 || (<-started).TaskID() != "tsk_1" || len(s.Jobs()) != 1 {
		t.Errorf("sink %+v, jobs %d", k, len(s.Jobs()))
	}

	// Zero outcome: done with an empty Result; throttled: no Result.
	if out, _ := (&Stub{}).Run(context.Background(), job("t"), &k); out.Result == nil || out.Result.Status != v1alpha1.ResultDone {
		t.Errorf("zero stub = %+v", out)
	}
	if out, _ := (&Stub{Outcome: Outcome{Status: StatusThrottled}}).Run(context.Background(), job("t"), &k); out.Result != nil {
		t.Errorf("throttled stub = %+v", out)
	}
	boom := errors.New("boom")
	if _, err := (&Stub{Err: boom}).Run(context.Background(), job("t"), &k); !errors.Is(err, boom) {
		t.Errorf("err stub = %v", err)
	}

	// Hold: cancelled → failed{cancelled}.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err = (&Stub{Hold: make(chan struct{})}).Run(ctx, job("t"), &k)
	if err != nil || out.Status != StatusFailed || out.Reason != ReasonCancelled || out.Result != nil {
		t.Errorf("held stub after cancel = %+v, %v", out, err)
	}
	f := Func(func(context.Context, *Job, Sink) (Outcome, error) { return Outcome{Status: StatusNoop}, nil })
	if out, _ := f.Run(context.Background(), job("t"), &k); out.Status != StatusNoop {
		t.Errorf("Func = %+v", out)
	}
}
