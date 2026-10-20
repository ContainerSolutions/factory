package stack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/compliance-framework/factory/internal/agent/harness"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/clock"
)

// maxOutputTail is how much of make's output a failed Up keeps: written to a failure log
// under the state directory (failurelog.go), never logged above debug, since local-dev's
// output is the owner's, not the task's, and can hold credentials.
const maxOutputTail = 64 << 10

// ForPipeline is the run pipeline's Stack (pipeline.Stack): Up resolves the task's
// versions (inputs.train or inputs.verifyAgainst), brings the stack up under the task's
// compose project and waits for it; Down takes down the stack this ForPipeline brought up.
type ForPipeline struct {
	Manager *Manager
	// LocalDev is the local-dev checkout (config.LocalDev); absolute.
	LocalDev string
	// GitHub reads the train and the latest releases; default GitHub{} (anonymous).
	GitHub GitHubReader
	// Profile is the make profile; default ProfileShowcase.
	Profile string
	// WaitTimeout bounds the wait after make; default DefaultWaitTimeout.
	WaitTimeout time.Duration
	// Clock stamps the state; default clock.Real.
	Clock clock.Clock
	// Logger; default discard.
	Logger *slog.Logger

	mu      sync.Mutex
	project string
}

// Up implements pipeline.Stack.
func (p *ForPipeline) Up(ctx context.Context, _ harness.Workspace, t v1alpha1.Task) (map[string]string, error) {
	if p.Manager == nil {
		return nil, errors.New("no stack manager")
	}
	src, err := SourceFromInputs(t.Spec.Inputs)
	if err != nil {
		return nil, err
	}
	gh := p.GitHub
	if gh == nil {
		gh = GitHub{}
	}
	v, err := Resolve(ctx, src, gh)
	if err != nil {
		return nil, err
	}
	profile := p.Profile
	if profile == "" {
		profile = ProfileShowcase
	}
	clk := p.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	log := p.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := State{
		Project: ProjectName(t.Metadata.ID), Task: t.Metadata.ID, Profile: profile, LocalDev: p.LocalDev,
		Versions: v, Source: src.String(), StartedAt: clk.Now().UTC(),
	}
	out := &tail{n: maxOutputTail}
	m := p.Manager.WithOutput(out)
	log.Info("stack up", "project", s.Project, "source", s.Source, "api", v.API, "ui", v.UI, "agent", v.Agent)
	if err := m.Up(ctx, s); err != nil {
		return nil, p.upFailed(log, clk, s, out, err)
	}
	if err := m.Wait(ctx, p.WaitTimeout); err != nil {
		out.markFailure()
		if derr := m.Down(context.WithoutCancel(ctx), s.Project); derr != nil {
			err = errors.Join(err, fmt.Errorf("and taking it down: %w", derr))
		}
		return nil, p.upFailed(log, clk, s, out, err)
	}
	p.mu.Lock()
	p.project = s.Project
	p.mu.Unlock()
	return m.Env(s), nil
}

// Down implements pipeline.Stack.
func (p *ForPipeline) Down(ctx context.Context) error {
	p.mu.Lock()
	project := p.project
	p.project = ""
	p.mu.Unlock()
	if project == "" {
		return nil
	}
	return p.Manager.WithOutput(&tail{n: maxOutputTail}).Down(ctx, project)
}

// upFailed handles a failed Up or Wait: it writes make's output to a failure log, logs the
// failure at warn with only the output's last line (redacted), and returns err naming the
// log (a path on the agent's machine, never the output: the error becomes the Result's
// summary).
func (p *ForPipeline) upFailed(log *slog.Logger, clk clock.Clock, s State, out *tail, err error) error {
	target := upTarget(s.Profile)
	attrs := []any{"project", s.Project, "target", target, "err", err, "last", redactLine(out.failedLine())}
	path, lerr := writeFailureLog(p.Manager.StateDir(), s.Project, target, clk.Now(), err, out.String())
	if path != "" {
		attrs = append(attrs, "log", path)
	}
	if lerr != nil {
		attrs = append(attrs, "logErr", lerr)
	}
	log.Warn("stack up failed", attrs...)
	if path == "" {
		return err
	}
	return fmt.Errorf("%w (make output: %s, on the agent)", err, path)
}
