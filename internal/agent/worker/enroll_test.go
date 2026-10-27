package worker_test

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ContainerSolutions/factory/internal/agent/worker"
)

// memEnrollment is a worker.Enrollment in memory: the saved refresh token, else the
// enrollment token.
type memEnrollment struct {
	mu                  sync.Mutex
	enroll, refresh     string
	saves, forgets      int
	savedBeforeAgentUse []bool // per save: no call used an agent token issued after it yet
	f                   *fakeDispatcher
}

func (m *memEnrollment) Token() (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refresh != "" {
		return m.refresh, true, nil
	}
	if m.enroll == "" {
		return "", false, errors.New("no enrollment token")
	}
	return m.enroll, false, nil
}

func (m *memEnrollment) Save(r string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh = r
	m.saves++
	// The registration that returned r is the last call so far: nothing used its token.
	calls := m.f.Calls()
	m.savedBeforeAgentUse = append(m.savedBeforeAgentUse, calls[len(calls)-1].Path == "/agents")
	return nil
}

func (m *memEnrollment) Forget() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh = ""
	m.forgets++
	return nil
}

func withEnrollment(e worker.Enrollment) harnessOpt {
	return func(o *worker.Options) { o.Enrollment, o.JoinToken, o.Owner = e, nil, nil }
}

// An enrolled agent registers with its enrollment token once, saves each refresh token before
// it uses the new agent token, and re-registers with the refresh token after a 401.
func TestEnrolledRegistration(t *testing.T) {
	f := newFakeDispatcher(t)
	f.enrollToken = "enr_enroll-secret"
	enr := &memEnrollment{enroll: f.enrollToken, f: f}
	h := startWorker(t, f, &stubRunner{}, withEnrollment(enr))
	f.waitFor("the first poll", func() bool { return len(f.find(http.MethodGet, "/lease")) == 1 })
	f.revoke()
	f.offer(testLease("1", nil))
	f.waitFor("a second registration", func() bool { return len(f.find(http.MethodPost, "/agents")) == 2 })
	f.waitFor("complete", func() bool { return len(f.find(http.MethodPost, "/complete")) == 1 })
	if err := h.stop(t); err != nil {
		t.Fatal(err)
	}
	regs := f.find(http.MethodPost, "/agents")
	if regs[0].Join != "" || regs[0].Owner != "" {
		t.Errorf("an enrolled registration sent the join or owner token")
	}
	enr.mu.Lock()
	defer enr.mu.Unlock()
	if enr.saves != 2 || enr.refresh != "arf_refresh-secret-2" || enr.forgets != 0 {
		t.Errorf("saves %d, refresh %q, forgets %d", enr.saves, enr.refresh, enr.forgets)
	}
	for i, ok := range enr.savedBeforeAgentUse {
		if !ok {
			t.Errorf("save %d came after the new agent token was used", i)
		}
	}
	h.assertNoSecretsInLogs(t, "enr_enroll-secret", "arf_refresh-secret")
}

// A refused refresh token is forgotten and fatal, saying to enroll again; so is a refused
// enrollment token. Neither reaches the error or the logs.
func TestEnrolledRegistrationRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		enr  *memEnrollment
		want string
	}{
		{"refresh", &memEnrollment{refresh: "arf_stale-secret"}, "refused the agent's refresh token"},
		{"enrollment", &memEnrollment{enroll: "enr_spent-secret"}, "refused the enrollment token"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeDispatcher(t)
			c.enr.f = f
			h := startWorker(t, f, &stubRunner{}, withEnrollment(c.enr))
			err := <-h.done
			h.done <- err
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "factory agent enroll") ||
				strings.Contains(err.Error(), "-secret") {
				t.Fatalf("Run = %v", err)
			}
			if n := len(f.Calls()); n != 1 {
				t.Errorf("%d calls, want 1", n)
			}
			if c.name == "refresh" && (c.enr.forgets != 1 || c.enr.refresh != "") {
				t.Errorf("the refused refresh token wasn't forgotten")
			}
			h.assertNoSecretsInLogs(t, "-secret")
		})
	}
}
