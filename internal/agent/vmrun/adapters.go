package vmrun

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ContainerSolutions/factory/internal/agent/config"
	"github.com/ContainerSolutions/factory/internal/agent/harness"
	"github.com/ContainerSolutions/factory/internal/agent/harness/claudecode"
	"github.com/ContainerSolutions/factory/internal/agent/harness/codex"
	"github.com/ContainerSolutions/factory/internal/agent/runbundle"
	"github.com/ContainerSolutions/factory/internal/clock"
)

// adapterOptions are what vmrun builds the task's adapter with.
type adapterOptions struct {
	// Binaries override a harness's executable by harness name (--harness-bin).
	Binaries map[string]string
	// Clock stamps the adapter's events (and codex's placeholder login).
	Clock clock.Clock
}

// taskAdapters builds the adapters for b's task: only its harness (workspace-run.md §Sequence,
// step 5), with a config.Harness made from the task's resolved harness (no Probe runs). codex gets the bundle's
// codex/account.json as its Account.
func taskAdapters(b *runbundle.Bundle, o adapterOptions) (map[string]harness.Adapter, error) {
	h := b.Task.Spec.Harness
	if h == nil || h.Resolved == nil || h.Resolved.Name == "" {
		return nil, errors.New("the task has no resolved harness")
	}
	r := *h.Resolved
	cfg := config.Harness{Default: r.Model}
	if r.Model != "" {
		cfg.Models = []string{r.Model}
	}
	if r.ReasoningEffort != "" {
		cfg.Efforts = []string{r.ReasoningEffort}
	}
	bin := o.Binaries[r.Name]
	var ad harness.Adapter
	switch r.Name {
	case config.HarnessClaudeCode:
		ad = claudecode.New(claudecode.Options{Config: cfg, Binary: bin, Clock: o.Clock})
	case config.HarnessCodex:
		if b.Manifest.Codex == nil {
			return nil, errors.New("a codex task's bundle has no codex account")
		}
		acct := *b.Manifest.Codex
		ad = codex.New(codex.Options{Config: cfg, Binary: bin, Clock: o.Clock, Account: &acct})
	default:
		return nil, fmt.Errorf("no adapter for harness %q", r.Name)
	}
	return map[string]harness.Adapter{r.Name: ad}, nil
}

// harnessBins is --harness-bin name=path, repeatable: a known harness's executable.
type harnessBins map[string]string

func (h harnessBins) String() string {
	var l []string
	for _, k := range slices.Sorted(maps.Keys(h)) {
		l = append(l, k+"="+h[k])
	}
	return strings.Join(l, ",")
}

func (h harnessBins) Set(s string) error {
	name, p, ok := strings.Cut(s, "=")
	switch {
	case !ok || p == "":
		return fmt.Errorf("%q: want <harness>=<path>", s)
	case !slices.Contains(config.Harnesses(), name):
		return fmt.Errorf("%q: the harness must be one of %s", s, strings.Join(config.Harnesses(), ", "))
	}
	h[name] = p
	return nil
}
