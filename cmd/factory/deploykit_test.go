package main

// The PoC run kit under deploy/poc (RUN-S1-T01): .env.example names every variable factory
// serve reads for the shipped catalog, run.sh requires exactly those, and the dev events go
// through factory dispatch and fire dev RuleSets only, never against compliance-framework/docs.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/rules"
)

const (
	deployKitDir   = "../../deploy/poc"
	shippedCatalog = "../../catalog"
)

// loadShippedCatalog loads catalog/ as factory serve does.
func loadShippedCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, errs := catalog.LoadDir(os.DirFS(shippedCatalog), catalog.Options{})
	if c == nil {
		t.Fatalf("catalog/ does not load: %v", errs)
	}
	return c
}

// catalogSecretEnvs is the variable of every secretRef in c's objects, sorted.
func catalogSecretEnvs(c *catalog.Catalog) []string {
	var objs []any
	for _, m := range []any{c.Providers, c.Groups, c.Templates, c.Rules, c.Queues, c.Pools} {
		iter := reflect.ValueOf(m).MapRange()
		for iter.Next() {
			objs = append(objs, iter.Value().Interface())
		}
	}
	for _, rs := range c.RuleSets {
		objs = append(objs, rs.Spec)
	}
	for _, ts := range c.TaskSpecs {
		objs = append(objs, ts.Spec)
	}
	seen := map[string]bool{}
	for _, o := range objs {
		collectSecretRefs(reflect.ValueOf(o), func(r v1alpha1.SecretRef) { seen[secretEnv(r)] = true })
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

var secretRefType = reflect.TypeFor[v1alpha1.SecretRef]()

// collectSecretRefs calls found with every SecretRef reachable from v (v1alpha1 objects are
// plain data: no cycles).
func collectSecretRefs(v reflect.Value, found func(v1alpha1.SecretRef)) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			collectSecretRefs(v.Elem(), found)
		}
	case reflect.Struct:
		if v.Type() == secretRefType {
			found(v.Interface().(v1alpha1.SecretRef))
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				collectSecretRefs(v.Field(i), found)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			collectSecretRefs(v.Index(i), found)
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			collectSecretRefs(iter.Value(), found)
		}
	default:
	}
}

var envLineRE = regexp.MustCompile(`^([A-Z][A-Z0-9_]*)=(.*)$`)

// envExample is the variables deploy/poc/.env.example sets (its uncommented NAME= lines).
func envExample(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(deployKitDir, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := envLineRE.FindStringSubmatch(line)
		switch {
		case m == nil:
			t.Errorf(".env.example:%d: not a NAME= line or a comment: %q", n, line)
		case m[2] != "":
			t.Errorf(".env.example:%d: %s has a value; the example holds names only", n, m[1])
		case slices.Contains(names, m[1]):
			t.Errorf(".env.example:%d: %s is listed twice", n, m[1])
		default:
			names = append(names, m[1])
		}
	}
	return names
}

// TestDeployEnvExample: every variable factory serve needs for the shipped catalog has its
// line in .env.example (the catalog's secretRefs and serve's own), and nothing else does.
func TestDeployEnvExample(t *testing.T) {
	names := envExample(t)
	secrets := catalogSecretEnvs(loadShippedCatalog(t))
	if !slices.Contains(secrets, "FACTORY_SECRET_LOCAL_POOL_JOIN_TOKEN") || !slices.Contains(secrets, "FACTORY_SECRET_GITHUB_PUBLIC_WEBHOOK_SECRET") {
		t.Fatalf("the secretRef walk missed the catalog's known refs: %v", secrets)
	}
	want := append([]string{envPublicURL, envOAuthClientID, envOAuthClientSecret, envSessionKey, envCLIToken}, secrets...)
	for _, n := range want {
		if !slices.Contains(names, n) {
			t.Errorf("deploy/poc/.env.example has no %s= line", n)
		}
	}
	for _, n := range names {
		if !slices.Contains(want, n) {
			t.Errorf("deploy/poc/.env.example sets %s, which neither factory serve nor the catalog reads (optional run.sh settings stay commented out)", n)
		}
	}
}

// TestDeployRunShRequires: run.sh checks exactly the variables .env.example sets.
func TestDeployRunShRequires(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(deployKitDir, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)\nrequired=\(\n(.*?)\n\)\n`).FindSubmatch(b)
	if m == nil {
		t.Fatal("run.sh has no required=( … ) list")
	}
	got := strings.Fields(string(m[1]))
	want := envExample(t)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("run.sh requires %v; .env.example sets %v", got, want)
	}
}

// prodDocsRE is the prod docs repo, not mock-docs or another docs-* repo.
var prodDocsRE = regexp.MustCompile(`compliance-framework/docs($|[^-\w])`)

// TestDeployEvents: each deploy/poc/events file goes through factory dispatch unchanged, and
// against the shipped catalog it dispatches release-docs to mock-docs through dev RuleSets
// only: no prod RuleSet fires, and no decision names compliance-framework/docs.
func TestDeployEvents(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(deployKitDir, "events", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no events under deploy/poc/events: %v", err)
	}
	c := loadShippedCatalog(t)
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			cs := &commandsServer{}
			ts := httptest.NewServer(cs.handler(http.StatusAccepted, `{"type":"dispatch","event":"inject/req-1"}`))
			defer ts.Close()
			var out, errOut bytes.Buffer
			env := getenvOf(map[string]string{envCLIToken: testCLIToken})
			if code := runDispatch(context.Background(), []string{"--event", file, "--server", ts.URL}, env, &out, &errOut); code != 0 {
				t.Fatalf("factory dispatch: exit %d: %s", code, errOut.String())
			}
			if len(cs.cmds) != 1 || cs.cmds[0].Args == nil || cs.cmds[0].Args.Event == nil {
				t.Fatalf("commands %+v", cs.cmds)
			}
			ev := *cs.cmds[0].Args.Event
			// No id, so the same file can be dispatched again (the server names it inject/…).
			if ev.ID != "" {
				t.Errorf("the event has id %q; leave it out so each dispatch is a new event", ev.ID)
			}
			if ev.Data.Object == nil {
				t.Fatal("the event has no data.object")
			}

			// What the server stores: an id, a time, the provider.
			ev.Kind, ev.ID, ev.Time = v1alpha1.KindEvent, "inject/deploy-kit", time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
			o := ev.Data.Object
			f := rules.Fixture{
				Name:  filepath.Base(file),
				Event: ev,
				WorkItem: rules.WorkItemView{ID: "wi-deploy-kit", Links: []v1alpha1.Link{
					{Provider: o.Provider, Ref: o.Ref, Kind: o.Kind, Primary: true},
				}},
			}
			res, err := f.Evaluate(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range res.Matches {
				if m.Fired() && !strings.HasPrefix(m.RuleSet, "dev-") {
					t.Errorf("rule %s fires through %s, not a dev RuleSet", m.Rule, m.RuleSet)
				}
			}
			releaseDocs := false
			for i, d := range res.Decisions {
				if !strings.HasPrefix(d.RuleSet, "dev-") {
					t.Errorf("decision %d (%s) comes from %s, not a dev RuleSet", i, d.Rule, d.RuleSet)
				}
				b, err := json.Marshal(d.Action)
				if err != nil {
					t.Fatal(err)
				}
				if prodDocsRE.Match(b) {
					t.Errorf("decision %d (%s) names compliance-framework/docs: %s", i, d.Rule, b)
				}
				if dis := d.Action.Dispatch; dis != nil && dis.TaskSpec == "release-docs" && dis.Target != nil &&
					dis.Target.Repo == "compliance-framework/mock-docs" && d.Rail.Verdict == v1alpha1.RailAllowed {
					releaseDocs = true
				}
			}
			if !releaseDocs {
				t.Errorf("no allowed release-docs dispatch to compliance-framework/mock-docs; decisions %+v", res.Decisions)
			}
		})
	}
}
