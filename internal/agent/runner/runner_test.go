package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ContainerSolutions/factory/internal/apiclient"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

const secret = "ghs_secret-token-value"

func TestCredentialsRedacted(t *testing.T) {
	c := NewCredentials(apiclient.GitCredential{Token: secret, Repo: "o/r", Host: "github.com", ExpiresAt: time.Unix(0, 0).UTC()},
		apiclient.GitCredential{Token: secret + "-read", Repo: "o/private"})
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
	if !strings.Contains(c.String(), "o/private") {
		t.Errorf("String doesn't name the read repos: %s", c)
	}
	c.Set(apiclient.GitCredential{Token: "new"})
	if c.Get().Token != "new" {
		t.Error("Set didn't swap the credential")
	}
	reads := c.Reads()
	reads[0].Token = "mutated"
	if c.Reads()[0].Token != secret+"-read" {
		t.Error("Reads returned the held slice")
	}
	c.SetReads([]apiclient.GitCredential{{Token: "new-read", Repo: "o/private"}})
	if r := c.Reads(); len(r) != 1 || r[0].Token != "new-read" {
		t.Errorf("SetReads didn't swap the read credentials: %v", r)
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

func TestFunc(t *testing.T) {
	var k sink
	f := Func(func(context.Context, *Job, Sink) (Outcome, error) { return Outcome{Status: StatusNoop}, nil })
	if out, _ := f.Run(context.Background(), job("t"), &k); out.Status != StatusNoop {
		t.Errorf("Func = %+v", out)
	}
}

func TestStatusesReasons(t *testing.T) {
	for name, set := range map[string][]string{"Statuses": Statuses(), "Reasons": Reasons()} {
		seen := map[string]bool{}
		for _, v := range set {
			if v == "" || seen[v] {
				t.Errorf("%s has %q empty or twice", name, v)
			}
			seen[v] = true
		}
	}
	if s := Statuses(); len(s) != 5 || s[4] != StatusThrottled {
		t.Errorf("Statuses() = %v", s)
	}
	if r := Reasons(); len(r) != 5 || r[3] != ReasonEnvironmentError || r[4] != ReasonCancelled {
		t.Errorf("Reasons() = %v", r)
	}
}
