package stack

import (
	"context"
	"maps"
	"testing"
)

func TestRunEnv(t *testing.T) {
	full := State{Project: "factory-tsk-1", Versions: StackVersions{API: "1.0.0", UI: "2.0.0", Agent: "0.9.0"}}.
		Env("http://localhost:8000", "http://localhost:8080", "/home/o/.factory-agent")
	for _, tt := range []struct {
		name string
		env  map[string]string
		want map[string]string
	}{
		{"the stack the pipeline put in the run", full, full},
		{"other variables are left out", map[string]string{EnvURL: "u", EnvAPI: "a", "FACTORY_STACK_X": "x", "GH_TOKEN": "t"}, map[string]string{EnvURL: "u", EnvAPI: "a"}},
		{"no stack", map[string]string{EnvDir: "/d", EnvProject: "p"}, nil},
		{"no API", map[string]string{EnvURL: "u"}, nil},
		{"no UI", map[string]string{EnvAPI: "a"}, nil},
	} {
		got := RunEnv(func(k string) string { return tt.env[k] })
		if !maps.Equal(got, tt.want) || (tt.want == nil) != (got == nil) {
			t.Errorf("%s: RunEnv = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestWaitURLsNeedsBoth(t *testing.T) {
	for _, o := range []Options{{}, {UIURL: "http://localhost:8000"}, {APIURL: "http://localhost:8080"}} {
		if err := WaitURLs(context.Background(), o, 0); err == nil {
			t.Errorf("WaitURLs(%+v) = nil, want an error", o)
		}
	}
}
