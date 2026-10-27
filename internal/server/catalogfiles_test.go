package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ContainerSolutions/factory/internal/apiclient"
)

// TestCatalogFiles: the list holds every prompt and schema of the loaded catalog, sorted; a
// file is served from the snapshot as the catalog holds it; anything outside prompts/ and
// schemas/, or not a clean path, is a 400, and a clean path the catalog lacks a 404.
func TestCatalogFiles(t *testing.T) {
	e := newEnv(t)
	cli := bearer(cliToken)

	l, err := e.c.ListCatalogFilesWithResponse(e.ctx, cli)
	if err != nil || l.JSON200 == nil {
		t.Fatalf("list: %v %s", err, l.Body)
	}
	var paths []string
	for _, f := range l.JSON200.Items {
		paths = append(paths, f.Path)
		want := apiclient.CatalogFileKindPrompt
		if strings.HasPrefix(f.Path, "schemas/") {
			want = apiclient.CatalogFileKindSchema
		}
		if f.Kind != want {
			t.Errorf("%s: kind %s, want %s", f.Path, f.Kind, want)
		}
	}
	if n := len(e.cat.Assets.Prompts) + len(e.cat.Assets.SchemaSources); len(paths) != n || n == 0 {
		t.Errorf("listed %d files, the catalog holds %d", len(paths), n)
	}
	if !slices.IsSorted(paths) {
		t.Errorf("not sorted: %v", paths)
	}

	schema := "schemas/results/implement.json"
	src := e.cat.Assets.SchemaSources[schema]
	sum := sha256.Sum256(src)
	g, err := e.c.GetCatalogFileWithResponse(e.ctx, &apiclient.GetCatalogFileParams{Path: schema}, cli)
	if err != nil || g.JSON200 == nil {
		t.Fatalf("get %s: %v %s", schema, err, g.Body)
	}
	if f := g.JSON200; f.Kind != apiclient.CatalogFileKindSchema || f.Content != string(src) || f.Digest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("schema file %+v", f)
	}

	for _, tc := range []struct {
		path   string
		status int
	}{
		{"prompts/nope.md.tmpl", http.StatusNotFound},
		{"schemas/results/nope.json", http.StatusNotFound},
		{"taskspecs/implement.yaml", http.StatusBadRequest},
		{"prompts/../taskspecs/implement.yaml", http.StatusBadRequest},
		{"prompts/../../etc/passwd", http.StatusBadRequest},
		{"/prompts/implement.md.tmpl", http.StatusBadRequest},
		{"prompts//implement.md.tmpl", http.StatusBadRequest},
		{"prompts/", http.StatusBadRequest},
		{`prompts\..\x`, http.StatusBadRequest},
		{"prompts/" + strings.Repeat("a", 512), http.StatusBadRequest},
	} {
		r, err := e.c.GetCatalogFileWithResponse(e.ctx, &apiclient.GetCatalogFileParams{Path: tc.path}, cli)
		if err != nil {
			t.Fatal(err)
		}
		code := CodeNotFound
		if tc.status == http.StatusBadRequest {
			code = CodeInvalid
		}
		wantProblem(t, tc.path, r.HTTPResponse, r.Body, tc.status, code)
	}
}
