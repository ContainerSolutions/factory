// Package probe works out what this machine can offer the dispatcher (worker.md §Startup step
// 2): a HarnessOffer per configured harness, the labels whose prerequisites pass, and the base
// tooling every run needs. It never calls the dispatcher.
//
// Harness detection proper (versions, auth status) is each adapter's Probe (AGT-S1-T03/T04,
// harness-adapters.md); until an adapter is registered, Configured reports what the local
// config says. Label checks (local-stack.md §Labels and probes) plug in as LabelCheckers.
package probe

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strings"

	"github.com/compliance-framework/factory/internal/agent/config"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Prober probes one harness: harness-adapters.md's Adapter.Probe.
type Prober interface {
	Probe(ctx context.Context, cfg config.Harness) (v1alpha1.HarnessOffer, error)
}

// Configured is the default Prober: the offer is the local config's models, efforts and
// default, with the harness's subscription login assumed (the PoC runs both harnesses on
// their owners' subscriptions, 10-poc.md). The adapters replace it with real detection.
type Configured struct{ Name string }

// Probe implements Prober.
func (c Configured) Probe(_ context.Context, h config.Harness) (v1alpha1.HarnessOffer, error) {
	return v1alpha1.HarnessOffer{
		Name: c.Name, Auth: "subscription",
		Models: slices.Clone(h.Models), DefaultModel: h.Default, ReasoningEffort: slices.Clone(h.Efforts),
	}, nil
}

// LabelChecker verifies a label's prerequisites; a label is advertised only when it passes.
type LabelChecker interface {
	Check(ctx context.Context, cfg *config.Config) error
}

// LabelFunc adapts a function to LabelChecker.
type LabelFunc func(ctx context.Context, cfg *config.Config) error

// Check calls f.
func (f LabelFunc) Check(ctx context.Context, cfg *config.Config) error { return f(ctx, cfg) }

// Tooling checks the base tooling the worker refuses to start without.
type Tooling interface {
	Check(ctx context.Context) []Tool
}

// Tool is one base-tooling requirement and whether it is met.
type Tool struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Problem string `json:"problem,omitempty"`
}

// Options say how to probe. Nil maps and a nil Tooling use the defaults.
type Options struct {
	// Harnesses maps a harness name to its Prober; a configured harness without one is
	// probed with Configured.
	Harnesses map[string]Prober
	// Labels maps a label to its checker; a configured label without one is advertised as is
	// (its probe isn't built yet).
	Labels map[string]LabelChecker
	// Tooling checks the base tooling; nil skips the check.
	Tooling Tooling
}

// Dropped is a configured harness or label that isn't advertised, and why.
type Dropped struct {
	Kind   string `json:"kind"` // harness or label
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Report is what a probe found.
type Report struct {
	Capabilities v1alpha1.AgentCapabilities `json:"capabilities"`
	Dropped      []Dropped                  `json:"dropped,omitempty"`
	Tooling      []Tool                     `json:"tooling,omitempty"`
}

// Ready reports whether the worker may start: at least one harness offered and every base
// tool present; otherwise the reasons.
func (r Report) Ready() (bool, []string) {
	var why []string
	if len(r.Capabilities.Harnesses) == 0 {
		why = append(why, "no harness passed its probe")
	}
	for _, t := range r.Tooling {
		if !t.OK {
			why = append(why, fmt.Sprintf("base tool %s: %s", t.Name, t.Problem))
		}
	}
	return len(why) == 0, why
}

// Run probes every configured harness and label and the base tooling. Harnesses are offered
// in name order; labels keep their configured order.
func Run(ctx context.Context, cfg *config.Config, o Options) Report {
	var rep Report
	names := make([]string, 0, len(cfg.Harnesses))
	for n := range cfg.Harnesses {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := o.Harnesses[n]
		if p == nil {
			p = Configured{Name: n}
		}
		off, err := p.Probe(ctx, cfg.Harnesses[n])
		if err != nil {
			rep.Dropped = append(rep.Dropped, Dropped{Kind: "harness", Name: n, Reason: err.Error()})
			continue
		}
		off.Name = n
		rep.Capabilities.Harnesses = append(rep.Capabilities.Harnesses, off)
	}
	for _, l := range cfg.Labels {
		if c := o.Labels[l]; c != nil {
			if err := c.Check(ctx, cfg); err != nil {
				rep.Dropped = append(rep.Dropped, Dropped{Kind: "label", Name: l, Reason: err.Error()})
				continue
			}
		}
		rep.Capabilities.Labels = append(rep.Capabilities.Labels, l)
	}
	if o.Tooling != nil {
		rep.Tooling = o.Tooling.Check(ctx)
	}
	return rep
}

// Commands runs programs for BaseTooling; tests fake it.
type Commands interface {
	LookPath(name string) (string, error)
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecCommands runs real programs.
type ExecCommands struct{}

// LookPath is exec.LookPath.
func (ExecCommands) LookPath(name string) (string, error) { return exec.LookPath(name) }

// Output runs name with args and returns its standard output.
func (ExecCommands) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &out
	err := cmd.Run()
	return out.Bytes(), err
}

// BaseTools are the programs every run needs (worker.md §Startup step 2).
func BaseTools() []string { return []string{"git", "gh", "jq", "python3"} }

// GhExtensions are the gh extensions every run needs.
func GhExtensions() []string { return []string{"gh-pr-review", "gh-stack"} }

// BaseTooling checks BaseTools on PATH and the GhExtensions in `gh extension list` (which
// needs no gh login).
type BaseTooling struct{ Commands Commands }

// Check implements Tooling.
func (b BaseTooling) Check(ctx context.Context) []Tool {
	cmds := b.Commands
	if cmds == nil {
		cmds = ExecCommands{}
	}
	var out []Tool
	ghOK := false
	for _, name := range BaseTools() {
		t := Tool{Name: name, OK: true}
		if _, err := cmds.LookPath(name); err != nil {
			t.OK, t.Problem = false, "not found on PATH"
		}
		ghOK = ghOK || (name == "gh" && t.OK)
		out = append(out, t)
	}
	var list string
	listErr := ""
	if ghOK {
		b, err := cmds.Output(ctx, "gh", "extension", "list")
		if err != nil {
			listErr = fmt.Sprintf("gh extension list failed: %v", err)
		}
		list = string(b)
	} else {
		listErr = "needs gh"
	}
	for _, ext := range GhExtensions() {
		t := Tool{Name: ext, OK: listErr == "" && strings.Contains(list, ext)}
		switch {
		case listErr != "":
			t.Problem = listErr
		case !t.OK:
			t.Problem = "not installed (gh extension install …/" + ext + ")"
		}
		out = append(out, t)
	}
	return out
}
