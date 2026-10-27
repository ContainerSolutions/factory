// Package probe works out what this machine can offer the dispatcher (supervise.md
// §factory-agent probe): a HarnessOffer per configured harness, the configured labels, and
// the base tooling the host itself needs. It never calls the dispatcher.
//
// The harnesses run in the VM, so a harness is offered by its Prober (supervise and probe use
// the login prober: the owner's model login is in the state dir); Configured reports what the
// local config says.
package probe

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"sort"

	"github.com/ContainerSolutions/factory/internal/agent/config"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
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
	// Tooling checks the base tooling; nil skips the check.
	Tooling Tooling
}

// Dropped is a configured harness that isn't advertised, and why.
type Dropped struct {
	Kind   string `json:"kind"` // harness
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

// Run probes every configured harness and the base tooling. Harnesses are offered in name
// order; labels are advertised as configured, in their order.
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
	rep.Capabilities.Labels = slices.Clone(cfg.Labels)
	if o.Tooling != nil {
		rep.Tooling = o.Tooling.Check(ctx)
	}
	return rep
}

// Commands finds programs for BaseTooling; tests fake it.
type Commands interface {
	LookPath(name string) (string, error)
}

// ExecCommands finds real programs.
type ExecCommands struct{}

// LookPath is exec.LookPath.
func (ExecCommands) LookPath(name string) (string, error) { return exec.LookPath(name) }

// BaseTools are the programs the host needs (supervise.md §factory-agent probe): git, which
// fetches the skills for the run bundles.
func BaseTools() []string { return []string{"git"} }

// BaseTooling checks BaseTools on PATH.
type BaseTooling struct{ Commands Commands }

// Check implements Tooling.
func (b BaseTooling) Check(context.Context) []Tool {
	cmds := b.Commands
	if cmds == nil {
		cmds = ExecCommands{}
	}
	var out []Tool
	for _, name := range BaseTools() {
		t := Tool{Name: name, OK: true}
		if _, err := cmds.LookPath(name); err != nil {
			t.OK, t.Problem = false, "not found on PATH"
		}
		out = append(out, t)
	}
	return out
}
