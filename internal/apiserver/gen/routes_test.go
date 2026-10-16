package gen_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/compliance-framework/factory/api"
	"github.com/compliance-framework/factory/internal/apiserver/gen"
)

// notGenerated are the tags with no generated handler: webhooks are the provider adapter's
// Conn.Handler() (mounted with echo.WrapHandler), ops routes live on the localhost ops listener.
var notGenerated = []string{"webhooks", "ops"}

// TestRoutes checks that RegisterHandlers mounts exactly the spec's public operations under
// /api/v1, and no webhook or ops route.
func TestRoutes(t *testing.T) {
	doc, err := api.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ops, err := api.Operations(doc)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, op := range ops {
		if slices.Contains(notGenerated, op.Tags[0]) {
			continue
		}
		want = append(want, op.Method+" "+echoPath(op.Path))
	}
	slices.Sort(want)

	e := echo.New()
	gen.RegisterHandlersWithBaseURL(e, gen.NewStrictHandler(nil, nil), "/api/v1")
	var got []string
	for _, r := range e.Routes() {
		got = append(got, r.Method+" "+r.Path)
	}
	slices.Sort(got)

	if !slices.Equal(got, want) {
		t.Errorf("routes:\n got %v\nwant %v", got, want)
	}
	for _, r := range got {
		if !strings.Contains(r, " /api/v1/") {
			t.Errorf("route %s is outside /api/v1", r)
		}
	}
}

// echoPath turns /tasks/{id} into Echo's /tasks/:id.
func echoPath(p string) string {
	segments := strings.Split(p, "/")
	for i, s := range segments {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			segments[i] = ":" + s[1:len(s)-1]
		}
	}
	return strings.Join(segments, "/")
}
