package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// bom is the Unicode byte order mark, an ignored token.
const bom = "\xef\xbb\xbf"

// MaxGraphQLBody bounds a POST /graphql body the gateway reads to check it.
const MaxGraphQLBody = 1 << 20

// ErrGraphQLNotRead is a POST /graphql body that isn't provably a read: not a single JSON
// object with only query, variables and operationName, or a document with anything but
// queries and fragments at its top level, or one that doesn't lex.
var ErrGraphQLNotRead = errors.New("the GraphQL request is not a query")

// CheckGraphQLRequest reports whether body, a POST /graphql request body, only reads: one JSON
// object (no batch array, no trailing data, valid UTF-8, no key twice under any case) whose
// keys are query (a non-empty string), variables and operationName, and whose query document
// passes CheckGraphQLDocument. Anything else is ErrGraphQLNotRead. It fails closed: a body
// another parser might read differently is refused.
func CheckGraphQLRequest(body []byte) error {
	if !utf8.Valid(body) {
		return ErrGraphQLNotRead
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ErrGraphQLNotRead
	}
	seen := map[string]bool{}
	query, hasQuery := "", false
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok || seen[strings.ToLower(key)] {
			return ErrGraphQLNotRead
		}
		seen[strings.ToLower(key)] = true
		switch key {
		case "query":
			var s *string
			if err := dec.Decode(&s); err != nil || s == nil {
				return ErrGraphQLNotRead
			}
			query, hasQuery = *s, true
		case "variables", "operationName":
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return ErrGraphQLNotRead
			}
		default:
			return ErrGraphQLNotRead
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return ErrGraphQLNotRead
	}
	if _, err := dec.Token(); err != io.EOF || !hasQuery {
		return ErrGraphQLNotRead
	}
	return CheckGraphQLDocument(query)
}

// CheckGraphQLDocument reports whether a GraphQL document only reads. It lexes the document
// (skipping comments, strings and block strings) and checks its top level, outside every
// bracket: each definition starts with '{' (an anonymous query), query or fragment; no
// top-level name is mutation or subscription in any case (so an operation named mutation is
// refused too); and between definitions only names, '@', '(' and '{' appear. A lexical error,
// an unbalanced bracket, an unterminated definition or an empty document is ErrGraphQLNotRead.
func CheckGraphQLDocument(doc string) error {
	var stack []byte // the open brackets
	atStart, defs := true, 0
	for i := 0; i < len(doc); {
		tok, next, err := lexGraphQL(doc, i)
		if err != nil {
			return err
		}
		i = next
		if tok == "" { // ignored: whitespace, a comma, a comment, the BOM
			continue
		}
		top := len(stack) == 0
		if top {
			name := isNameStart(tok[0])
			if name && (strings.EqualFold(tok, "mutation") || strings.EqualFold(tok, "subscription")) {
				return ErrGraphQLNotRead
			}
			switch {
			case atStart && (tok == "{" || tok == "query" || tok == "fragment"):
				atStart = false
				defs++
			case atStart, !name && tok != "@" && tok != "(" && tok != "{":
				return ErrGraphQLNotRead
			}
		}
		switch tok {
		case "{", "(", "[":
			stack = append(stack, tok[0])
		case "}", ")", "]":
			if len(stack) == 0 || stack[len(stack)-1] != opener(tok[0]) {
				return ErrGraphQLNotRead
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 && tok == "}" {
				atStart = true // a selection set at the top level ends its definition
			}
		}
	}
	if len(stack) != 0 || !atStart || defs == 0 {
		return ErrGraphQLNotRead
	}
	return nil
}

// valueTok is the token lexGraphQL returns for a string or a number: a value, which never
// appears at the top level, and which no name or punctuator equals.
const valueTok = "0"

// opener is the bracket a closing one closes.
func opener(c byte) byte {
	switch c {
	case '}':
		return '{'
	case ')':
		return '('
	}
	return '['
}

// lexGraphQL reads the token at doc[i:] and returns it and the index after it: "" for an
// ignored token, the token's text for a punctuator or a name, and valueTok for a string or a
// number. It is stricter than the spec where that
// costs nothing (no control character in a comment or string, only the spec's escapes), so a
// document another lexer might read differently is an error.
func lexGraphQL(doc string, i int) (tok string, next int, err error) {
	switch c := doc[i]; {
	case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
		return "", i + 1, nil
	case strings.HasPrefix(doc[i:], bom):
		return "", i + len(bom), nil
	case c == '#':
		next, err := skipComment(doc, i+1)
		return "", next, err
	case c == '"':
		if strings.HasPrefix(doc[i:], `"""`) {
			next, err := skipBlockString(doc, i+3)
			return valueTok, next, err
		}
		next, err := skipString(doc, i+1)
		return valueTok, next, err
	case c == '-' || isDigit(c):
		next, err := skipNumber(doc, i)
		return valueTok, next, err
	case isNameStart(c):
		j := i + 1
		for j < len(doc) && (isNameStart(doc[j]) || isDigit(doc[j])) {
			j++
		}
		return doc[i:j], j, nil
	case strings.HasPrefix(doc[i:], "..."):
		return "...", i + 3, nil
	case strings.IndexByte("!$&()[]{}:=@|", c) >= 0:
		return doc[i : i+1], i + 1, nil
	}
	return "", 0, ErrGraphQLNotRead
}

// skipComment returns the index of the line terminator that ends the comment starting at i (or
// the end). It ends at either CR or LF, so it never runs past a line another lexer would end;
// a control character, or a Unicode line or paragraph separator, is an error.
func skipComment(doc string, i int) (int, error) {
	for i < len(doc) {
		r, n := utf8.DecodeRuneInString(doc[i:])
		switch {
		case r == '\n' || r == '\r':
			return i, nil
		case !sourceRune(r, false):
			return 0, ErrGraphQLNotRead
		}
		i += n
	}
	return i, nil
}

// skipString returns the index after the closing quote of the string whose body starts at i.
// The escapes are the spec's (\" \\ \/ \b \f \n \r \t \u); a line terminator or a control
// character is an error.
func skipString(doc string, i int) (int, error) {
	for i < len(doc) {
		r, n := utf8.DecodeRuneInString(doc[i:])
		switch {
		case r == '"':
			return i + 1, nil
		case r == '\\':
			if i+1 >= len(doc) || strings.IndexByte(`"\/bfnrtu`, doc[i+1]) < 0 {
				return 0, ErrGraphQLNotRead
			}
			i += 2
			continue
		case !sourceRune(r, false):
			return 0, ErrGraphQLNotRead
		}
		i += n
	}
	return 0, ErrGraphQLNotRead
}

// skipBlockString returns the index after the closing """ of the block string whose body
// starts at i. Its only escape is \""" (a literal """); line terminators are allowed.
func skipBlockString(doc string, i int) (int, error) {
	for i < len(doc) {
		switch {
		case strings.HasPrefix(doc[i:], `\"""`):
			i += 4
			continue
		case strings.HasPrefix(doc[i:], `"""`):
			return i + 3, nil
		}
		r, n := utf8.DecodeRuneInString(doc[i:])
		if !sourceRune(r, true) {
			return 0, ErrGraphQLNotRead
		}
		i += n
	}
	return 0, ErrGraphQLNotRead
}

// skipNumber returns the index after the number at i: -?digits, then optional .digits and
// [eE][+-]?digits. A number followed by a name character or a dot is an error.
func skipNumber(doc string, i int) (int, error) {
	digits := func() bool {
		j := i
		for i < len(doc) && isDigit(doc[i]) {
			i++
		}
		return i > j
	}
	if doc[i] == '-' {
		i++
	}
	if !digits() {
		return 0, ErrGraphQLNotRead
	}
	if i < len(doc) && doc[i] == '.' {
		i++
		if !digits() {
			return 0, ErrGraphQLNotRead
		}
	}
	if i < len(doc) && (doc[i] == 'e' || doc[i] == 'E') {
		i++
		if i < len(doc) && (doc[i] == '+' || doc[i] == '-') {
			i++
		}
		if !digits() {
			return 0, ErrGraphQLNotRead
		}
	}
	if i < len(doc) && (isNameStart(doc[i]) || isDigit(doc[i]) || doc[i] == '.') {
		return 0, ErrGraphQLNotRead
	}
	return i, nil
}

// sourceRune reports whether r may appear in a comment or a string: tab, and every printable
// rune but DEL, U+0085 and the Unicode line and paragraph separators; CR and LF in a block
// string only.
func sourceRune(r rune, block bool) bool {
	switch {
	case r == '\t':
		return true
	case r == '\n' || r == '\r':
		return block
	case r < 0x20 || r == 0x7f || r == 0x85 || r == 0x2028 || r == 0x2029 || r == utf8.RuneError:
		return false
	}
	return true
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

func isNameStart(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '_' }
