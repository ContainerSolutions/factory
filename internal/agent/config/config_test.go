package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validYAML = `# the LLD's example
dispatcher: https://factory.example.dev
pool: local
joinTokenFile: ~/.config/factory-agent/join-token
labels: [has-local-stack, gif-tooling]
harnesses:
  codex:
    home: ~/.factory-agent/codex-home
    models: [gpt-5-codex]
    efforts: [low, medium, high]
    default: gpt-5-codex
  claude-code:
    configDir: ~/.factory-agent/claude
    models: [claude-sonnet-5-5, claude-opus-5-5]
    efforts: [low, medium, high, max]
    default: claude-sonnet-5-5
localDev: ~/repos/cs/ccf/local-dev
workRoot: ~/.factory-agent/work
lending:
  unlimited: true
`

func opts() Options {
	return Options{
		Home:     func() (string, error) { return "/home/owner", nil },
		Hostname: func() (string, error) { return "Gus-MBP.local", nil },
	}
}

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(validYAML), opts())
	if err != nil {
		t.Fatal(err)
	}
	if c.Dispatcher != "https://factory.example.dev" || c.Pool != "local" || c.Host != "gus-mbp" ||
		c.JoinTokenFile != "/home/owner/.config/factory-agent/join-token" ||
		c.WorkRoot != "/home/owner/.factory-agent/work" || c.LocalDev != "/home/owner/repos/cs/ccf/local-dev" ||
		c.GitHubURL != DefaultGitHubURL || !c.Lending.Unlimited || c.MaxConcurrent() != 1 {
		t.Errorf("config %+v", c)
	}
	if h := c.Harnesses[HarnessCodex]; h.Home != "/home/owner/.factory-agent/codex-home" || h.Dir() != h.Home || h.Default != "gpt-5-codex" {
		t.Errorf("codex %+v", h)
	}
	if h := c.Harnesses[HarnessClaudeCode]; h.ConfigDir != "/home/owner/.factory-agent/claude" || len(h.Efforts) != 4 {
		t.Errorf("claude-code %+v", h)
	}
}

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]byte(`
dispatcher: http://127.0.0.1:8081/
pool: local
joinTokenFile: /etc/factory/join
host: e2e
harnesses: {codex: {home: /var/codex, models: [gpt-5-codex]}}
lending: {queues: [poc], maxConcurrent: 2}
`), opts())
	if err != nil {
		t.Fatal(err)
	}
	if c.Dispatcher != "http://127.0.0.1:8081" || c.Host != "e2e" || c.WorkRoot != "/home/owner/.factory-agent/work" || c.MaxConcurrent() != 2 {
		t.Errorf("config %+v", c)
	}
}

func TestParseInvalid(t *testing.T) {
	base := func(repl ...string) string {
		return strings.NewReplacer(repl...).Replace(validYAML)
	}
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{"unknown key", validYAML + "extra: 1\n", []string{`unknown field "extra"`}},
		{"unknown nested key", base("    default: gpt-5-codex\n", "    default: gpt-5-codex\n    sandbox: off\n"), []string{`unknown field "sandbox"`}},
		{"duplicate key", validYAML + "pool: other\n", []string{`key "pool" already defined`}},
		{"non-string key", validYAML + "1: x\n", []string{"is not a string"}},
		{"merge key", "a: &a {pool: x}\n<<: *a\n", []string{"merge keys"}},
		{"two documents", validYAML + "---\npool: x\n", []string{"exactly one YAML document"}},
		{"empty", "", []string{"empty config"}},
		{"not a mapping", "- a\n", []string{"must be a mapping"}},
		{"yaml 1.2 strings", base("lending:\n  unlimited: true\n", "lending:\n  unlimited: yes\n"), []string{"cannot unmarshal string"}},
		{"missing everything", "lending: {unlimited: true}\nharnesses: {}\n", []string{"dispatcher: required", "pool: required", "joinTokenFile: required", "harnesses: at least one"}},
		{"relative join token", base("~/.config/factory-agent/join-token", "join-token"), []string{"joinTokenFile: \"join-token\" must be absolute"}},
		{"http on a public host", base("https://factory.example.dev", "http://factory.example.dev"), []string{"plain http is only allowed on a loopback host"}},
		{"dispatcher with credentials", base("https://factory.example.dev", "https://u:p@factory.example.dev"), []string{"must not carry credentials"}},
		{"dispatcher with query", base("https://factory.example.dev", "https://factory.example.dev/?x=1"), []string{"must not carry credentials, a query"}},
		{"relative dispatcher", base("https://factory.example.dev", "factory.example.dev"), []string{"absolute http(s) URL"}},
		{"bad github url", base("lending:", "githubURL: ftp://github.com\nlending:"), []string{"githubURL:"}},
		{"bad host", base("lending:", "host: Gus_MBP\nlending:"), []string{"host: \"Gus_MBP\" must match"}},
		{"bad label", base("[has-local-stack, gif-tooling]", "[Has_Local]"), []string{"labels[0]"}},
		{"duplicate label", base("[has-local-stack, gif-tooling]", "[gif-tooling, gif-tooling]"), []string{"labels[1]: \"gif-tooling\" is listed twice"}},
		{"local stack without localDev", base("localDev: ~/repos/cs/ccf/local-dev\n", ""), []string{"localDev: required with the has-local-stack label"}},
		{"unknown harness", base("  claude-code:", "  aider:\n    models: [x]\n  claude-code:"), []string{"harnesses.aider.unknown harness"}},
		{"codex without home", base("    home: ~/.factory-agent/codex-home\n", ""), []string{"harnesses.codex.home: required"}},
		{"codex with configDir", base("    home: ~/.factory-agent/codex-home\n", "    home: ~/.factory-agent/codex-home\n    configDir: /x\n"), []string{"harnesses.codex.configDir: not a codex key"}},
		{"claude without configDir", base("    configDir: ~/.factory-agent/claude\n", ""), []string{"harnesses.claude-code.configDir: required"}},
		{"relative harness dir", base("~/.factory-agent/claude", "claude"), []string{"harnesses.claude-code.configDir: \"claude\" must be absolute"}},
		{"no models", base("    models: [gpt-5-codex]\n", "    models: []\n"), []string{"harnesses.codex.models: at least one model", "default: \"gpt-5-codex\" is not one of models"}},
		{"duplicate model", base("[gpt-5-codex]", "[gpt-5-codex, gpt-5-codex]"), []string{"models[1]"}},
		{"bad effort", base("[low, medium, high]", "[low, extreme]"), []string{"harnesses.codex.efforts[1]: \"extreme\""}},
		{"default not a model", base("    default: claude-sonnet-5-5", "    default: claude-haiku"), []string{"default: \"claude-haiku\" is not one of models"}},
		{"lends nothing", base("  unlimited: true\n", "  maxConcurrent: 1\n"), []string{"lending: set unlimited: true or list the queues"}},
		{"unlimited with lists", base("  unlimited: true\n", "  unlimited: true\n  queues: [poc]\n  hours: 09:00-17:00\n"), []string{"lending: unlimited lends everything; drop queues, hours"}},
		{"negative concurrency", base("  unlimited: true\n", "  unlimited: true\n  maxConcurrent: -1\n"), []string{"lending.maxConcurrent: must not be negative"}},
		{"unknown lending harness", base("  unlimited: true\n", "  queues: [poc]\n  harnesses: [aider]\n"), []string{"lending.harnesses[0]: \"aider\""}},
		{"unknown lending key", base("  unlimited: true\n", "  unlimited: true\n  forever: true\n"), []string{`unknown field "forever"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml), opts())
			if err == nil {
				t.Fatalf("no error, want %q", tt.want)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v\nwant %q", err, w)
				}
			}
		})
	}
}

func TestHostFrom(t *testing.T) {
	for in, want := range map[string]string{
		"Gus-MBP.local": "gus-mbp", "build_box_01": "build-box-01", "-x": "x", "": "agent", "...": "agent", "ünï": "n-",
	} {
		if got := HostFrom(in); got != want || !hostRE.MatchString(got) {
			t.Errorf("HostFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

func writeFile(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // umask may have narrowed it
		t.Fatal(err)
	}
	return p
}

func TestLoadPermissions(t *testing.T) {
	dir := t.TempDir()
	o := opts()
	var warnings []string
	o.Warn = func(s string) { warnings = append(warnings, s) }

	p := writeFile(t, dir, "ok.yaml", validYAML, 0o600)
	c, err := Load(p, o)
	if err != nil || c.Path != p || len(warnings) != 0 {
		t.Fatalf("Load 0600: %v %v", err, warnings)
	}

	p = writeFile(t, dir, "readable.yaml", validYAML, 0o644)
	if _, err := Load(p, o); err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "readable by group or others") {
		t.Fatalf("Load 0644: %v %v", err, warnings)
	}

	p = writeFile(t, dir, "writable.yaml", validYAML, 0o666)
	if _, err := Load(p, o); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("Load 0666: %v", err)
	}

	bad := writeFile(t, dir, "bad.yaml", "pool: local\n", 0o600)
	if _, err := Load(bad, o); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("Load invalid: %v, want the path in the error", err)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml"), o); err == nil {
		t.Fatal("Load missing: no error")
	}
	if _, err := Load(dir, o); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Load dir: %v", err)
	}
}

func TestReadSecretFile(t *testing.T) {
	dir := t.TempDir()
	const secret = "join-secret-value"
	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		want    string
		wantErr string
	}{
		{"0600", secret + "\n", 0o600, secret, ""},
		{"0400", secret, 0o400, secret, ""},
		{"group readable", secret, 0o640, "", "accessible to group or others"},
		{"world readable", secret, 0o604, "", "accessible to group or others"},
		{"empty", "  \n", 0o600, "", "is empty"},
		{"two lines", secret + "\nmore", 0o600, "", "single line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := writeFile(t, dir, strings.ReplaceAll(tt.name, " ", "-"), tt.content, tt.mode)
			got, err := ReadSecretFile(p)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the error carries the secret: %v", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ReadSecretFile = %q, %v", got, err)
			}
		})
	}
	if _, err := ReadSecretFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file: no error")
	}
}

func TestExpandPath(t *testing.T) {
	home := func() (string, error) { return "/h", nil }
	for in, want := range map[string]string{"~": "/h", "~/a/b": "/h/a/b", "/abs": "/abs", "rel": "rel", "~other/x": "~other/x"} {
		if got, err := ExpandPath(in, home); err != nil || got != want {
			t.Errorf("ExpandPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestWorkspace(t *testing.T) {
	long := strings.Repeat("a", 201)
	many := "[" + strings.TrimSuffix(strings.Repeat("x,", 101), ",") + "]"
	tests := []struct {
		name string
		yaml string
		goos string
		want Workspace // checked when no problems are wanted
		errs []string
	}{
		{"defaults on darwin", "{}", "darwin", Workspace{Driver: DriverTart, Size: WorkspaceSize{CPUs: 6, Memory: "8Gi", Disk: "60Gi"}, GatewayPort: 3128}, nil},
		{"defaults on linux", "{}", "linux", Workspace{Driver: DriverKata, Size: WorkspaceSize{CPUs: 6, Memory: "8Gi", Disk: "60Gi"}, GatewayPort: 3128}, nil},
		{"set", "{driver: kata, size: {cpus: 16, memory: 64Gi, disk: 20Gi}, gatewayPort: 1024, warm: {images: [postgres:17.5], kindImages: [ghcr.io/x/agent:0.7.1]}}", "darwin",
			Workspace{Driver: DriverKata, Size: WorkspaceSize{CPUs: 16, Memory: "64Gi", Disk: "20Gi"}, GatewayPort: 1024, Warm: WarmImages{Images: []string{"postgres:17.5"}, KindImages: []string{"ghcr.io/x/agent:0.7.1"}}}, nil},
		{"bad driver", "{driver: docker}", "darwin", Workspace{}, []string{`workspace.driver: "docker"`}},
		{"cpus", "{size: {cpus: 17}}", "darwin", Workspace{}, []string{"workspace.size.cpus: 17"}},
		{"memory too small", "{size: {memory: 1Gi}}", "darwin", Workspace{}, []string{`workspace.size.memory: "1Gi"`}},
		{"memory not Gi", "{size: {memory: 8G}}", "darwin", Workspace{}, []string{`workspace.size.memory: "8G"`}},
		{"memory leading zero", "{size: {memory: 08Gi}}", "darwin", Workspace{}, []string{`workspace.size.memory: "08Gi"`}},
		{"disk", "{size: {disk: 501Gi}}", "darwin", Workspace{}, []string{`workspace.size.disk: "501Gi"`}},
		{"port", "{gatewayPort: 80}", "darwin", Workspace{}, []string{"workspace.gatewayPort: 80"}},
		{"images", "{warm: {images: ['', 'a b', " + long + "], kindImages: " + many + "}}", "darwin", Workspace{},
			[]string{"workspace.warm.images[0]", "workspace.warm.images[1]", "workspace.warm.images[2]", "workspace.warm.kindImages: 101 images"}},
		{"image flag", "{warm: {kindImages: ['--help']}}", "darwin", Workspace{}, []string{`workspace.warm.kindImages[0]: "--help"`}},
		{"unknown key", "{size: {gpus: 1}}", "darwin", Workspace{}, []string{`unknown field "gpus"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := opts()
			o.GOOS = tt.goos
			dir := t.TempDir()
			// host's view: a file with only the workspace block, no worker keys.
			p := writeFile(t, dir, "agent.yaml", "workspace: "+tt.yaml+"\n", 0o600)
			w, err := LoadWorkspace(p, false, o)
			// the worker's view: the same block in a full config.
			_, perr := Parse([]byte(validYAML+"workspace: "+tt.yaml+"\n"), o)
			if tt.errs == nil {
				if err != nil || perr != nil {
					t.Fatalf("LoadWorkspace: %v; Parse: %v", err, perr)
				}
				if !reflect.DeepEqual(w, tt.want) {
					t.Errorf("workspace %+v\nwant %+v", w, tt.want)
				}
				return
			}
			for _, e := range []error{err, perr} {
				for _, want := range tt.errs {
					if e == nil || !strings.Contains(e.Error(), want) {
						t.Errorf("err = %v\nwant %q", e, want)
					}
				}
			}
		})
	}
}

func TestLoadWorkspaceFile(t *testing.T) {
	o := opts()
	o.GOOS = "darwin"
	missing := filepath.Join(t.TempDir(), "agent.yaml")
	if w, err := LoadWorkspace(missing, true, o); err != nil || w.Driver != DriverTart {
		t.Fatalf("missing, missingOK: %+v %v", w, err)
	}
	if _, err := LoadWorkspace(missing, false, o); err == nil {
		t.Fatal("missing: no error")
	}
	p := writeFile(t, t.TempDir(), "agent.yaml", "workspace: {}\n", 0o666)
	if _, err := LoadWorkspace(p, true, o); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("0666: %v", err)
	}
	full := writeFile(t, t.TempDir(), "agent.yaml", validYAML, 0o600)
	if w, err := LoadWorkspace(full, false, o); err != nil || w.GatewayPort != 3128 {
		t.Fatalf("a worker config without workspace: %+v %v", w, err)
	}
}
