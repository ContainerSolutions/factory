package patterns

import (
	"errors"
	"regexp/syntax"
	"testing"

	"github.com/ContainerSolutions/factory/internal/apis/v1alpha1"
)

func TestCompileMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern any
		match   []string
		noMatch []string
	}{
		{name: "exact", pattern: "claude-sonnet-5-5",
			match: []string{"claude-sonnet-5-5"}, noMatch: []string{"claude-sonnet-5-55", "xclaude-sonnet-5-5", "", "Claude-Sonnet-5-5"}},
		{name: "exact keeps regex metacharacters literal", pattern: "gpt-5.1+[x]",
			match: []string{"gpt-5.1+[x]"}, noMatch: []string{"gpt-511+[x]", "gpt-5.1x"}},
		{name: "any", pattern: "*", match: []string{"", "anything", "a/b c\n"}},
		{name: "glob star", pattern: "claude-sonnet-5-*",
			match: []string{"claude-sonnet-5-", "claude-sonnet-5-5", "claude-sonnet-5-5/x"}, noMatch: []string{"claude-sonnet-4-5", "xclaude-sonnet-5-5"}},
		{name: "glob is anchored at both ends", pattern: "*-codex",
			match: []string{"gpt-5-codex"}, noMatch: []string{"gpt-5-codex-mini"}},
		{name: "glob question mark is one character", pattern: "gpt-5?-codex",
			match: []string{"gpt-5x-codex", "gpt-5é-codex"}, noMatch: []string{"gpt-5-codex", "gpt-5xy-codex"}},
		{name: "glob is case-sensitive", pattern: "claude-*", noMatch: []string{"Claude-x", "CLAUDE-x"}},
		{name: "alternation", pattern: "claude-{sonnet,opus}-5-*",
			match: []string{"claude-sonnet-5-5", "claude-opus-5-1"}, noMatch: []string{"claude-haiku-5-5"}},
		{name: "nested alternation", pattern: "{gpt-5{,.1}-codex,claude-*}",
			match: []string{"gpt-5-codex", "gpt-5.1-codex", "claude-x"}, noMatch: []string{"gpt-5.2-codex", "gpt-5{,.1}-codex"}},
		{name: "empty alternative", pattern: "gpt-5{,-mini}",
			match: []string{"gpt-5", "gpt-5-mini"}, noMatch: []string{"gpt-5-"}},
		{name: "comma outside braces is literal", pattern: "a,b*", match: []string{"a,bc"}, noMatch: []string{"a"}},
		{name: "glob dot is literal", pattern: "gpt-5.*", match: []string{"gpt-5.1"}, noMatch: []string{"gpt-511"}},
		{name: "escape", pattern: `a\*b\{c\}\\`, match: []string{`a*b{c}\`}, noMatch: []string{`axb{c}\`}},
		{name: "escaped multibyte rune", pattern: `\é*`, match: []string{"é", "éa"}, noMatch: []string{"e"}},
		{name: "regex anchored as written", pattern: `re:^gpt-5(\.\d+)?-codex$`,
			match: []string{"gpt-5-codex", "gpt-5.1-codex"}, noMatch: []string{"gpt-5-mini", "xgpt-5-codex", "gpt-5-codex-x"}},
		{name: "unanchored regex matches a substring", pattern: `re:sonnet`,
			match: []string{"claude-sonnet-5-5", "sonnet"}, noMatch: []string{"claude-opus-5-5"}},
		{name: "regex flags", pattern: `re:(?i)^claude-`, match: []string{"CLAUDE-x"}},
		{name: "re: inside a glob is literal", pattern: `xre:*`, match: []string{"xre:a"}, noMatch: []string{"re:a"}},
		{name: "list", pattern: []string{"low", "medium"},
			match: []string{"low", "medium"}, noMatch: []string{"high", ""}},
		{name: "list of patterns", pattern: []string{"claude-opus-*", `re:^gpt-5$`},
			match: []string{"claude-opus-5-5", "gpt-5"}, noMatch: []string{"gpt-5-codex"}},
		{name: "StringOrList scalar", pattern: v1alpha1.Scalar("gpt-5*"), match: []string{"gpt-5.1"}, noMatch: []string{"gpt-4"}},
		{name: "StringOrList list", pattern: v1alpha1.List("low"), match: []string{"low"}, noMatch: []string{"medium"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Compile(tt.pattern)
			if err != nil {
				t.Fatalf("Compile(%#v): %v", tt.pattern, err)
			}
			for _, s := range tt.match {
				if !p.Match(s) {
					t.Errorf("%s should match %q", p, s)
				}
			}
			for _, s := range tt.noMatch {
				if p.Match(s) {
					t.Errorf("%s should not match %q", p, s)
				}
			}
		})
	}
}

func TestCompileErrors(t *testing.T) {
	tests := []struct {
		name    string
		pattern any
		reason  string
	}{
		{name: "empty string", pattern: "", reason: "empty pattern"},
		{name: "empty list", pattern: []string{}, reason: "empty list"},
		{name: "empty list element", pattern: []string{"low", ""}, reason: "empty pattern"},
		{name: "zero StringOrList", pattern: v1alpha1.StringOrList{}, reason: "want one string"},
		{name: "empty StringOrList list", pattern: v1alpha1.List(), reason: "empty list"},
		{name: "unsupported type", pattern: 42, reason: "unsupported type int"},
		{name: "invalid UTF-8", pattern: "gpt-\xff", reason: "invalid UTF-8"},
		{name: "empty regex", pattern: "re:", reason: "empty regex"},
		{name: "bad regex", pattern: "re:gpt-(5", reason: "bad regex"},
		{name: "regex lookahead is not RE2", pattern: "re:(?=x)", reason: "bad regex"},
		{name: "unclosed brace", pattern: "claude-{sonnet,opus-5", reason: "unbalanced braces: missing '}'"},
		{name: "unclosed nested brace", pattern: "{a,{b,c}", reason: "unbalanced braces: missing '}'"},
		{name: "stray closing brace", pattern: "claude}-5", reason: "unbalanced braces: unexpected '}'"},
		{name: "trailing escape", pattern: `gpt-5\`, reason: "trailing escape"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Compile(tt.pattern)
			var pe *PatternError
			if !errors.As(err, &pe) {
				t.Fatalf("Compile(%#v) = %v, %v; want a *PatternError", tt.pattern, p, err)
			}
			if pe.Reason != tt.reason {
				t.Errorf("reason = %q, want %q", pe.Reason, tt.reason)
			}
			if Validate(tt.pattern) == nil {
				t.Error("Validate accepted what Compile rejects")
			}
		})
	}
}

func TestPatternErrorUnwrapsRegexpError(t *testing.T) {
	_, err := Compile("re:(")
	var re *syntax.Error
	if !errors.As(err, &re) {
		t.Fatalf("want the regexp error in the chain, got %v", err)
	}
}

func TestString(t *testing.T) {
	tests := []struct {
		pattern any
		want    string
	}{
		{"*", "*"},
		{"gpt-5", "gpt-5"},
		{"gpt-5*", "gpt-5*"},
		{`re:^gpt$`, `re:^gpt$`},
		{[]string{"low", "med*"}, "[low, med*]"},
	}
	for _, tt := range tests {
		p, err := Compile(tt.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}

func FuzzCompile(f *testing.F) {
	for _, s := range []string{
		"", "*", "claude-{sonnet,opus}-5-*", "gpt-5?-codex", `re:^gpt-5(\.\d+)?-codex$`, "re:(",
		"{{{", "}", `\`, `a\{`, "{a,{b,c},}", "\xff", "re:" + "a{1000}{1000}",
	} {
		f.Add(s, "claude-sonnet-5-5")
	}
	f.Fuzz(func(t *testing.T, pattern, s string) {
		for _, v := range []any{pattern, []string{pattern, "x"}} {
			p, err := Compile(v)
			if err != nil {
				var pe *PatternError
				if !errors.As(err, &pe) {
					t.Fatalf("Compile(%q) returned an untyped error: %v", pattern, err)
				}
				continue
			}
			p.Match(s)
			_ = p.String()
		}
	})
}
