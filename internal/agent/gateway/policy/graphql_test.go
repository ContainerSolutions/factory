package policy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCheckGraphQLDocument(t *testing.T) {
	tests := []struct {
		name, doc string
		read      bool
	}{
		{"anonymous", `{ viewer { login } }`, true},
		{"query", `query { viewer { login } }`, true},
		{"named with variables", `query PR($owner: String!, $n: Int = 3, $f: In = {a: [1, 2.5e3, -4]}) @dir(x: "y") { repository(owner: $owner) { pullRequest(number: $n) { title } } }`, true},
		{"fragments", `query { ...F } fragment F on Query { viewer { ...on User { login } } }`, true},
		{"gh's shape", "query PullRequestByNumber($owner: String!) {\n  repository(owner: $owner) {\n    pullRequest(number: 1) {id,title}\n  }\n}\n", true},
		{"comments and commas", "# a mutation in a comment\r{ a, b } # mutation { x }\n,", true},
		{"a string holding a mutation", `{ a(x: "} mutation { b }") }`, true},
		{"an escaped quote", `{ a(x: "\" } mutation { b }") }`, true},
		{"an escaped backslash", `{ a(x: "\\") b }`, true},
		{"a block string", `{ a(x: """ } mutation { b } """) }`, true},
		{"a block string's escaped quotes", `{ a(x: """ \""" } mutation { b } """) }`, true},
		{"a block string with newlines", "{ a(x: \"\"\"\nline\n\"\"\") }", true},
		{"the BOM", "\xef\xbb\xbf{ a }", true},
		{"two operations", `query A { a } query B { b }`, true},
		{"unicode in a string", `{ a(x: "café") }`, true},

		{"mutation", `mutation { addComment(input: {}) { id } }`, false},
		{"subscription", `subscription { s }`, false},
		{"a mutation after a query", `query { a } mutation { b }`, false},
		{"a mutation after a fragment", `fragment F on Q { a } mutation M { b }`, false},
		{"a mutation after a comment", "# c\nmutation { b }", false},
		{"a mutation after a CR comment", "# c\rmutation { b }", false},
		{"upper case", `MUTATION { b }`, false},
		{"an operation named mutation", `query mutation { a }`, false},
		{"schema definition", `schema { query: Q }`, false},
		{"extend", `extend type Q { a: Int }`, false},
		{"unknown keyword", `Query { a }`, false},
		{"empty", ``, false},
		{"only a comment", `# nothing`, false},
		{"unterminated selection", `{ a `, false},
		{"unterminated definition", `query Q`, false},
		{"unbalanced", `{ a ) }`, false},
		{"a stray closer", `{ a } }`, false},
		{"a value at the top", `"x" { a }`, false},
		{"a number at the top", `1 { a }`, false},
		{"a spread at the top", `...F`, false},
		{"unterminated string", `{ a(x: "abc) }`, false},
		{"a newline in a string", "{ a(x: \"a\nb\") }", false},
		{"a bad escape", `{ a(x: "\q") }`, false},
		{"unterminated block string", `{ a(x: """abc) }`, false},
		{"a control character", "{ a\x00 }", false},
		{"a control character in a comment", "# a\x01\n{ a }", false},
		{"a line separator in a comment", "# a\u2028mutation { b }\n{ a }", false},
		{"a bad number", `{ a(x: 1a) }`, false},
		{"a lone dot", `{ a(x: .5) }`, false},
		{"a non-ASCII name", `{ ä }`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckGraphQLDocument(tt.doc)
			if (err == nil) != tt.read {
				t.Fatalf("%q: %v, want read %v", tt.doc, err, tt.read)
			}
			if err != nil && !errors.Is(err, ErrGraphQLNotRead) {
				t.Errorf("error %v isn't ErrGraphQLNotRead", err)
			}
		})
	}
}

func TestCheckGraphQLRequest(t *testing.T) {
	tests := []struct {
		name, body string
		read       bool
	}{
		{"query", `{"query":"{ viewer { login } }"}`, true},
		{"with variables and name", `{"query":"query Q($a: Int) { a(x: $a) }","variables":{"a":1},"operationName":"Q"}`, true},
		{"null variables", `{"query":"{ a }","variables":null}`, true},
		{"escaped key", `{"q\u0075ery":"{ a }"}`, true},
		{"trailing whitespace", "{\"query\":\"{ a }\"}\n", true},

		{"mutation", `{"query":"mutation { a }"}`, false},
		{"escaped mutation", `{"query":"\u006dutation { a }"}`, false},
		{"a batch", `[{"query":"{ a }"}]`, false},
		{"no query", `{"variables":{}}`, false},
		{"null query", `{"query":null}`, false},
		{"query not a string", `{"query":{"a":1}}`, false},
		{"empty query", `{"query":""}`, false},
		{"a duplicate key", `{"query":"{ a }","query":"mutation { b }"}`, false},
		{"a case-variant duplicate", `{"query":"{ a }","Query":"mutation { b }"}`, false},
		{"a case-variant key", `{"Query":"{ a }"}`, false},
		{"an unknown key", `{"query":"{ a }","extensions":{"persistedQuery":{}}}`, false},
		{"trailing data", `{"query":"{ a }"}{"query":"mutation { b }"}`, false},
		{"invalid UTF-8", "{\"query\":\"{ a(x: \\\"\xff\\\") }\"}", false},
		{"not JSON", `query { a }`, false},
		{"empty", ``, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := CheckGraphQLRequest([]byte(tt.body)); (err == nil) != tt.read {
				t.Fatalf("%s: %v, want read %v", tt.body, err, tt.read)
			}
		})
	}
}

// FuzzCheckGraphQLDocument: no input panics, and a document that passes leaves the lexer at
// the top level, outside any comment or string, so a mutation added after it (or before it) is
// always seen.
func FuzzCheckGraphQLDocument(f *testing.F) {
	for _, s := range []string{
		`{ a }`, `query Q($a: In = {b: [1]}) { a(x: $a) }`, `{ a(x: "\" }") } # c`, `{ a(x: """ \""" """) }`,
		"\xef\xbb\xbf{ a }", `fragment F on Q { a } { ...F }`, `mutation { a }`, `{ a(x: "\\") }`, "{ a } #",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, doc string) {
		if CheckGraphQLDocument(doc) != nil {
			return
		}
		for _, bad := range []string{doc + "\nmutation { x }", doc + "\nsubscription { x }", "mutation { x }\n" + doc} {
			if CheckGraphQLDocument(bad) == nil {
				t.Fatalf("%q passes", bad)
			}
		}
		if CheckGraphQLDocument(doc+"\n"+doc) != nil {
			t.Fatalf("%q passes, but twice doesn't", doc)
		}
	})
}

// FuzzCheckGraphQLRequest: no input panics, and a body that passes is one JSON object whose
// query, as encoding/json reads it, passes too.
func FuzzCheckGraphQLRequest(f *testing.F) {
	for _, s := range []string{
		`{"query":"{ a }"}`, `{"query":"mutation { a }"}`, `[{"query":"{ a }"}]`, `{"query":"{ a }","query":"x"}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, body string) {
		if CheckGraphQLRequest([]byte(body)) != nil {
			return
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("%q passes but isn't a JSON object: %v", body, err)
		}
		var q string
		if err := json.Unmarshal(m["query"], &q); err != nil || CheckGraphQLDocument(q) != nil ||
			strings.TrimSpace(q) == "" {
			t.Fatalf("%q passes but its query %q doesn't", body, q)
		}
	})
}
