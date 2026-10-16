package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestDurationParse(t *testing.T) {
	tests := []struct {
		in      Duration
		want    time.Duration
		wantErr bool
	}{
		{in: "30s", want: 30 * time.Second},
		{in: "10m", want: 10 * time.Minute},
		{in: "1h30m", want: 90 * time.Minute},
		{in: "7d", want: 7 * 24 * time.Hour},
		{in: "1d12h", want: 36 * time.Hour},
		{in: "0d", want: 0},
		{in: "", wantErr: true},
		{in: "d", wantErr: true},
		{in: "-1d", wantErr: true},
		{in: "1.5d", wantErr: true},
		{in: "1d-2h", wantErr: true},
		{in: "7days", wantErr: true},
		{in: "10", wantErr: true},
		{in: "999999999999d", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.in), func(t *testing.T) {
			got, err := tt.in.Parse()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Parse(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Parse(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestStringOrListRoundTrip(t *testing.T) {
	type holder struct {
		V StringOrList `json:"v,omitzero"`
	}
	tests := []struct {
		name   string
		in     string
		want   []string
		isList bool
	}{
		{"pattern", `v: "*"`, []string{"*"}, false},
		{"scalar", `v: gpt-5*`, []string{"gpt-5*"}, false},
		{"list", `v: [low, medium]`, []string{"low", "medium"}, true},
		{"one-item list stays a list", `v: [high]`, []string{"high"}, true},
		{"empty list stays a list", `v: []`, []string{}, true},
		{"absent", `{}`, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h holder
			if err := unmarshalYAML([]byte(tt.in), &h); err != nil {
				t.Fatal(err)
			}
			if h.V.IsList() != tt.isList || len(h.V.Values) != len(tt.want) {
				t.Fatalf("decoded %+v, want %v (list=%v)", h.V, tt.want, tt.isList)
			}
			for i := range tt.want {
				if h.V.Values[i] != tt.want[i] {
					t.Fatalf("decoded %v, want %v", h.V.Values, tt.want)
				}
			}
			out, err := marshalYAML(h)
			if err != nil {
				t.Fatal(err)
			}
			var a, b any
			if err := yaml.Unmarshal([]byte(tt.in), &a); err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal(out, &b); err != nil {
				t.Fatal(err)
			}
			ja, _ := json.Marshal(a)
			jb, _ := json.Marshal(b)
			if string(ja) != string(jb) {
				t.Errorf("round trip: %s → %s", ja, jb)
			}
		})
	}
	var h holder
	if err := unmarshalYAML([]byte(`v: { a: b }`), &h); err == nil {
		t.Error("accepted an object")
	}
	if err := unmarshalYAML([]byte(`v: [1, [2]]`), &h); err == nil {
		t.Error("accepted a nested list")
	}
	if got, _ := json.Marshal(List("a")); string(got) != `["a"]` {
		t.Errorf("List(a) = %s", got)
	}
	if got, _ := json.Marshal(Scalar("a")); string(got) != `"a"` {
		t.Errorf("Scalar(a) = %s", got)
	}
}

func TestExprOr(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantExpr string
		wantName string
		wantErr  bool
	}{
		{name: "catalog-load template", in: `"{{ params.harness }}"`, wantExpr: "{{ params.harness }}"},
		{name: "runtime CEL", in: `"=item.state.harness"`, wantExpr: "=item.state.harness"},
		{name: "value", in: `{"name":"codex","config":{"model":"gpt-5*"}}`, wantName: "codex"},
		{name: "unknown field in value", in: `{"name":"codex","effort":"x"}`, wantErr: true},
		{name: "empty expression", in: `""`, wantErr: true},
		{name: "wrong type", in: `42`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e ExprOr[HarnessRequest]
			err := json.Unmarshal([]byte(tt.in), &e)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if e.Expr != tt.wantExpr || e.IsExpr() != (tt.wantExpr != "") || e.Value.Name != tt.wantName {
				t.Fatalf("decoded %+v", e)
			}
			out, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			var a, b any
			_ = json.Unmarshal([]byte(tt.in), &a)
			_ = json.Unmarshal(out, &b)
			ja, _ := json.Marshal(a)
			jb, _ := json.Marshal(b)
			if string(ja) != string(jb) {
				t.Errorf("round trip: %s → %s", ja, jb)
			}
		})
	}
}

func TestTemplateBody(t *testing.T) {
	var b TemplateBody
	if err := json.Unmarshal([]byte(`"hello {{ .x }}"`), &b); err != nil || b.Text != "hello {{ .x }}" {
		t.Fatalf("text body: %+v, %v", b, err)
	}
	if err := json.Unmarshal([]byte(`{"blocks":"a.json"}`), &b); err != nil || b.Blocks != "a.json" || b.Text != "" {
		t.Fatalf("blocks body: %+v, %v", b, err)
	}
	if _, err := json.Marshal(TemplateBody{Text: "a", Blocks: "b"}); err == nil {
		t.Error("marshalled a body with both text and blocks")
	}
}
