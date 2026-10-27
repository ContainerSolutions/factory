package pipeline

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/agent/runner"
	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

func TestHosts(t *testing.T) {
	// many is one more than the cap: the last one is dropped.
	many := make([]string, v1alpha1.MaxWorkspaceHosts+1)
	for i := range many {
		many[i] = fmt.Sprintf("h%d.example.com", i)
	}
	base := []string{"github.com", "factory.example.dev"}
	tests := []struct {
		name  string
		task  []string
		want  []string
		warns int
	}{
		{name: "no task hosts", want: base},
		{
			name: "task hosts appended in order",
			task: []string{"registry.npmjs.org", "*.pkg.dev", "proxy.golang.org:443"},
			want: append(slices.Clone(base), "registry.npmjs.org", "*.pkg.dev", "proxy.golang.org:443"),
		},
		{
			name: "duplicates of the git host and of each other removed",
			task: []string{"github.com", "proxy.golang.org", "factory.example.dev", "proxy.golang.org"},
			want: append(slices.Clone(base), "proxy.golang.org"),
		},
		{
			name:  "invalid hosts dropped, order stable",
			task:  []string{"10.0.0.1", "proxy.golang.org", "localhost", "*.com", "api.localhost:8080", "https://x.example.com", "sum.golang.org"},
			want:  append(slices.Clone(base), "proxy.golang.org", "sum.golang.org"),
			warns: 5,
		},
		{
			name:  "past the cap ignored",
			task:  many,
			want:  append(slices.Clone(base), many[:v1alpha1.MaxWorkspaceHosts]...),
			warns: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			r := &run{
				job:  &runner.Job{Dispatcher: "https://factory.example.dev/api"},
				task: v1alpha1.Task{Spec: v1alpha1.TaskRunSpec{Hosts: tt.task}},
				base: &url.URL{Scheme: "https", Host: "github.com"},
				log:  slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
			}
			if got := r.hosts(); !slices.Equal(got, tt.want) {
				t.Errorf("hosts %q, want %q", got, tt.want)
			}
			if n := strings.Count(logs.String(), "level=WARN"); n != tt.warns {
				t.Errorf("%d warnings, want %d:\n%s", n, tt.warns, logs.String())
			}
		})
	}
}
