package v1alpha1

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// hostCases are workspace.hosts entries with the sentinel ValidateHost wraps (nil = valid).
var hostCases = []struct {
	host string
	want error
}{
	{"registry.npmjs.org", nil},
	{"registry.yarnpkg.com", nil},
	{"npm.pkg.github.com:443", nil},
	{"example.com:1", nil},
	{"example.com:65535", nil},
	{"*.npmjs.org", nil},
	{"*.example.com", nil},
	{"*.a.b:8080", nil},
	{"a.b", nil},
	{"xn--bcher-kva.example", nil},
	{"a-b.c0.d", nil},
	{"1password.com", nil},
	{"localhost.example.com", nil},
	{"mylocalhost.example.com", nil},
	{"notlocalhost", ErrHostSyntax},
	{strings.Repeat("a", 63) + ".com", nil},

	{"", ErrHostSyntax},
	{"https://registry.npmjs.org", ErrHostSyntax},
	{"registry.npmjs.org/", ErrHostSyntax},
	{"registry.npmjs.org/pkg", ErrHostSyntax},
	{"example.com?x=1", ErrHostSyntax},
	{"user@example.com", ErrHostSyntax},
	{"user:pass@example.com", ErrHostSyntax},
	{"Registry.npmjs.org", ErrHostSyntax},
	{"example.com.", ErrHostSyntax},
	{".example.com", ErrHostSyntax},
	{"exa..mple.com", ErrHostSyntax},
	{"-example.com", ErrHostSyntax},
	{"example-.com", ErrHostSyntax},
	{"exa_mple.com", ErrHostSyntax},
	{"example.123", ErrHostSyntax},
	{"1.2.3", ErrHostSyntax},
	{strings.Repeat("a", 64) + ".com", ErrHostSyntax},
	{" example.com", ErrHostSyntax},
	{"internal", ErrHostSyntax},
	{"com:443", ErrHostSyntax},

	{"*", ErrHostWildcard},
	{"*example.com", ErrHostWildcard},
	{"a.*.example.com", ErrHostWildcard},
	{"*.*.example.com", ErrHostWildcard},
	{"example.*", ErrHostWildcard},
	{"**.example.com", ErrHostWildcard},
	{"*.com", ErrHostWildcard},
	{"*.com:443", ErrHostWildcard},

	{"10.0.0.1", ErrHostIP},
	{"10.0.0.1:8080", ErrHostIP},
	{"::1", ErrHostIP},
	{"[::1]", ErrHostIP},
	{"[2001:db8::1]:443", ErrHostIP},

	{"localhost", ErrHostLocalhost},
	{"localhost:8080", ErrHostLocalhost},
	{"api.localhost", ErrHostLocalhost},
	{"*.localhost", ErrHostLocalhost},

	{"example.com:", ErrHostPort},
	{"example.com:0", ErrHostPort},
	{"example.com:080", ErrHostPort},
	{"example.com:65536", ErrHostPort},
	{"example.com:+80", ErrHostPort},
	{"example.com:http", ErrHostPort},
}

func TestValidateHost(t *testing.T) {
	for _, tt := range hostCases {
		t.Run(tt.host, func(t *testing.T) {
			err := ValidateHost(tt.host)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			var he *HostError
			if !errors.As(err, &he) {
				t.Fatalf("want a *HostError wrapping %v, got %v", tt.want, err)
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if he.Host != tt.host {
				t.Errorf("Host = %q", he.Host)
			}
		})
	}
}

func TestValidateHostLength(t *testing.T) {
	label := strings.Repeat("a", 62)
	name := strings.Join([]string{label, label, label, label}, ".") // 251
	if err := ValidateHost(name + ".a"); err != nil {
		t.Errorf("253 characters: %v", err)
	}
	if err := ValidateHost(name + ".ab"); !errors.Is(err, ErrHostSyntax) {
		t.Errorf("254 characters: err = %v, want %v", err, ErrHostSyntax)
	}
	if err := ValidateHost("*." + name + ".a:443"); err != nil {
		t.Errorf("253 characters with wildcard and port: %v", err)
	}
}

func taskSpecWithHosts(hosts []string) string {
	var b strings.Builder
	b.WriteString("kind: TaskSpec\nmetadata: { name: t }\nspec:\n  workspace:\n    hosts:\n")
	for _, h := range hosts {
		fmt.Fprintf(&b, "      - %q\n", h)
	}
	return b.String()
}

func TestTaskSpecValidateHosts(t *testing.T) {
	many := make([]string, MaxWorkspaceHosts+1)
	for i := range many {
		many[i] = fmt.Sprintf("h%d.example.com", i)
	}
	tests := []struct {
		name  string
		hosts []string
		field string
		want  error
	}{
		{"valid", []string{"registry.npmjs.org", "*.yarnpkg.com:443"}, "", nil},
		{"at the limit", many[:MaxWorkspaceHosts], "", nil},
		{"over the limit", many, "spec.workspace.hosts", ErrTooManyHosts},
		{"scheme", []string{"registry.npmjs.org", "https://x.org"}, "spec.workspace.hosts[1]", ErrHostSyntax},
		{"wildcard", []string{"a.*.org"}, "spec.workspace.hosts[0]", ErrHostWildcard},
		{"IP", []string{"127.0.0.1"}, "spec.workspace.hosts[0]", ErrHostIP},
		{"localhost", []string{"localhost:3000"}, "spec.workspace.hosts[0]", ErrHostLocalhost},
		{"port", []string{"x.org:99999"}, "spec.workspace.hosts[0]", ErrHostPort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode([]byte(taskSpecWithHosts(tt.hosts)))
			if tt.want == nil {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want a *ValidationError, got %v", err)
			}
			if ve.Field != tt.field {
				t.Errorf("field = %q, want %q", ve.Field, tt.field)
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestTaskSpecWithoutWorkspaceValidates(t *testing.T) {
	if _, err := Decode([]byte("kind: TaskSpec\nmetadata: { name: t }\nspec: {}\n")); err != nil {
		t.Fatal(err)
	}
}

// TestHostSchemaMatchesGo checks that taskspec.schema.json and ValidateHost agree on every
// host case, and that the schema caps the list at MaxWorkspaceHosts.
func TestHostSchemaMatchesGo(t *testing.T) {
	sch := compileSchemas(t)["taskspec.schema.json"]
	doc := func(hosts ...string) any {
		hs := make([]any, len(hosts))
		for i, h := range hosts {
			hs[i] = h
		}
		return schemaInstance(t, map[string]any{"kind": "TaskSpec", "metadata": map[string]any{"name": "t"},
			"spec": map[string]any{"workspace": map[string]any{"hosts": hs}}})
	}
	for _, tt := range hostCases {
		schemaOK := sch.Validate(doc(tt.host)) == nil
		if goOK := ValidateHost(tt.host) == nil; schemaOK != goOK {
			t.Errorf("%q: schema accepts = %v, ValidateHost accepts = %v", tt.host, schemaOK, goOK)
		}
	}
	hosts := make([]string, MaxWorkspaceHosts+1)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("h%d.example.com", i)
	}
	if err := sch.Validate(doc(hosts[:MaxWorkspaceHosts]...)); err != nil {
		t.Errorf("%d hosts: %v", MaxWorkspaceHosts, err)
	}
	if sch.Validate(doc(hosts...)) == nil {
		t.Errorf("%d hosts: schema accepts", len(hosts))
	}
}

func TestTaskValidateHosts(t *testing.T) {
	many := make([]string, MaxWorkspaceHosts+1)
	for i := range many {
		many[i] = fmt.Sprintf("h%d.example.com", i)
	}
	tests := []struct {
		name  string
		hosts []string
		field string
		want  error
	}{
		{"none", nil, "", nil},
		{"valid", []string{"registry.npmjs.org", "*.yarnpkg.com:443"}, "", nil},
		{"at the limit", many[:MaxWorkspaceHosts], "", nil},
		{"over the limit", many, "spec.hosts", ErrTooManyHosts},
		{"single label", []string{"internal"}, "spec.hosts[0]", ErrHostSyntax},
		{"wildcard over a TLD", []string{"registry.npmjs.org", "*.com"}, "spec.hosts[1]", ErrHostWildcard},
		{"IP", []string{"127.0.0.1"}, "spec.hosts[0]", ErrHostIP},
		{"localhost", []string{"localhost:3000"}, "spec.hosts[0]", ErrHostLocalhost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(map[string]any{"kind": KindTask, "metadata": map[string]any{"id": "t1", "workItem": "w"},
				"spec": map[string]any{"taskSpec": map[string]any{"name": "ts"}, "queue": "q", "hosts": tt.hosts}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = Decode(b)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want a *ValidationError, got %v", err)
			}
			if ve.Field != tt.field {
				t.Errorf("field = %q, want %q", ve.Field, tt.field)
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestTaskHostSchemaMatchesGo checks that the Task's schema (the one the OpenAPI components
// are reflected from) and ValidateHost agree on every host case, and that it caps the list.
func TestTaskHostSchemaMatchesGo(t *testing.T) {
	sch := taskSchema(t)
	inst := func(hosts ...string) any {
		hs := make([]any, len(hosts))
		for i, h := range hosts {
			hs[i] = h
		}
		return schemaInstance(t, map[string]any{"kind": KindTask, "metadata": map[string]any{"id": "t1", "workItem": "w"},
			"spec": map[string]any{"taskSpec": map[string]any{"name": "ts"}, "queue": "q", "hosts": hs}})
	}
	if err := sch.Validate(inst()); err != nil {
		t.Fatalf("task without hosts: %v", err)
	}
	for _, tt := range hostCases {
		schemaOK := sch.Validate(inst(tt.host)) == nil
		if goOK := ValidateHost(tt.host) == nil; schemaOK != goOK {
			t.Errorf("%q: schema accepts = %v, ValidateHost accepts = %v", tt.host, schemaOK, goOK)
		}
	}
	hosts := make([]string, MaxWorkspaceHosts+1)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("h%d.example.com", i)
	}
	if err := sch.Validate(inst(hosts[:MaxWorkspaceHosts]...)); err != nil {
		t.Errorf("%d hosts: %v", MaxWorkspaceHosts, err)
	}
	if sch.Validate(inst(hosts...)) == nil {
		t.Errorf("%d hosts: schema accepts", len(hosts))
	}
}
