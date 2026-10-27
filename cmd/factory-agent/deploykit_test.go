package main

// The PoC run kit's agent config (deploy/poc/agent.yaml.example, RUN-S1-T01): it parses with
// the agent's own loader, and it mentions every key the loader accepts, so the example can't
// drift from the config type.

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/agent/config"
	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

const agentExample = "../../deploy/poc/agent.yaml.example"

func exampleOptions() config.Options {
	return config.Options{
		Home:     func() (string, error) { return "/home/owner", nil },
		Hostname: func() (string, error) { return "mymac.local", nil },
	}
}

func TestDeployAgentExample(t *testing.T) {
	data, err := os.ReadFile(agentExample)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(data, exampleOptions())
	if err != nil {
		t.Fatalf("agent.yaml.example does not load: %v", err)
	}
	if cfg.Pool != "local" || cfg.JoinTokenFile != "/home/owner/.config/factory-agent/join-token" || cfg.Host != "mymac" {
		t.Errorf("pool %q, joinTokenFile %q, host %q", cfg.Pool, cfg.JoinTokenFile, cfg.Host)
	}
	for _, h := range config.Harnesses() {
		if _, ok := cfg.Harnesses[h]; !ok {
			t.Errorf("no %s harness", h)
		}
	}

	// The loader is strict, so a misspelt key in a copy of the example is an error.
	if _, err := config.Parse(append(slices.Clone(data), "\nunknownKey: true\n"...), exampleOptions()); err == nil ||
		!strings.Contains(err.Error(), "unknownKey") {
		t.Errorf("an unknown key: %v, want an error naming it", err)
	}

	// Every key the loader knows appears in the example, set or commented out.
	for _, typ := range []reflect.Type{reflect.TypeFor[config.Config](), reflect.TypeFor[config.Harness](), reflect.TypeFor[v1alpha1.Lending]()} {
		for f := range typ.Fields() {
			key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if key == "" || key == "-" {
				continue
			}
			if !regexp.MustCompile(`(?m)^[ #]*` + regexp.QuoteMeta(key) + `:`).Match(data) {
				t.Errorf("agent.yaml.example doesn't mention %s.%s (%q)", typ.Name(), f.Name, key)
			}
		}
	}
}
