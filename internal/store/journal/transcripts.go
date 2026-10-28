package journal

import (
	"encoding/base64"
	"strings"
)

const (
	// TranscriptSuffix ends a transcript blob's name.
	TranscriptSuffix = ".jsonl.gz"
	// encodedPrefix marks a name holding a run name that isn't safe as a file name, in
	// base64url. It is outside the safe set, so the two never collide.
	encodedPrefix = "~"
	// MaxTranscriptName keeps names under the 255-byte limit of the file systems we run on.
	MaxTranscriptName = 240
)

// TranscriptName returns run's blob name (P2's file name in transcripts/), or "" when run is
// too long for one: the run name itself when it is safe, else ~ and its base64url.
func TranscriptName(run string) string {
	name := run
	if !safeName(run) {
		name = encodedPrefix + base64.RawURLEncoding.EncodeToString([]byte(run))
	}
	name += TranscriptSuffix
	if len(name) > MaxTranscriptName {
		return ""
	}
	return name
}

// RunOfTranscript is TranscriptName's inverse; ok is false for a name it never returns.
func RunOfTranscript(name string) (run string, ok bool) {
	base, ok := strings.CutSuffix(name, TranscriptSuffix)
	if !ok || base == "" {
		return "", false
	}
	if enc, isEnc := strings.CutPrefix(base, encodedPrefix); isEnc {
		b, err := base64.RawURLEncoding.DecodeString(enc)
		if err != nil || len(b) == 0 || safeName(string(b)) {
			return "", false
		}
		return string(b), true
	}
	return base, safeName(base)
}

// safeName reports whether a run name is used as its file name as is: letters, digits, '.',
// '_' and '-', not starting with '.' or '-'.
func safeName(s string) bool {
	if s == "" || s[0] == '.' || s[0] == '-' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
