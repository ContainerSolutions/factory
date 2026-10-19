package codex

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultThrottle is how long a rate-limited agent waits when the error gives no reset
// (harness-adapters.md: else +15 min).
const DefaultThrottle = 15 * time.Minute

// maxThrottle bounds a parsed reset: a weekly plan limit is the longest.
const maxThrottle = 8 * 24 * time.Hour

// isRateLimit reports whether an error's text says a rate or plan limit was hit.
func isRateLimit(msg string) bool {
	m := strings.ToLower(msg)
	for _, w := range rateLimitWords {
		if strings.Contains(m, w) {
			return true
		}
	}
	return false
}

var (
	againInRE = regexp.MustCompile(`(?i)(?:try again|retry|resets?)\s+in\s+([0-9][0-9a-z ,]*)`)
	againAtRE = regexp.MustCompile(`(?i)(?:try again|retry|resets?)\s+at\s+([^.()]+)`)
	unitRE    = regexp.MustCompile(`(?i)([0-9]+)\s*([a-z]+)`)
	ordinalRE = regexp.MustCompile(`(?i)\b([0-9]{1,2})(st|nd|rd|th)\b`)
)

// resetTime is when a rate limit ends: a resets_in_seconds / resets_at field in the event, a
// "try again in …" or "try again at …" in its text, else now + def. A reset in the past or
// beyond maxThrottle is ignored.
func resetTime(raw []byte, msg string, now time.Time, def time.Duration) time.Time {
	ok := func(t time.Time) bool { return t.After(now) && t.Sub(now) <= maxThrottle }
	if t, found := resetField(raw, now); found && ok(t) {
		return t
	}
	if m := againInRE.FindStringSubmatch(msg); m != nil {
		var d time.Duration
		for _, u := range unitRE.FindAllStringSubmatch(m[1], -1) {
			n, err := strconv.Atoi(u[1])
			if err != nil {
				continue
			}
			if u, ok := unit(u[2]); ok {
				d += time.Duration(n) * u
			}
		}
		if t := now.Add(d); d > 0 && ok(t) {
			return t
		}
	}
	if m := againAtRE.FindStringSubmatch(msg); m != nil {
		if t, found := parseAt(strings.TrimSpace(m[1]), now); found && ok(t) {
			return t
		}
	}
	return now.Add(def)
}

// unit is the duration of a unit word: d, day(s), h, hr(s), hour(s), m, min(s),
// minute(s), s, sec(s), second(s).
func unit(u string) (time.Duration, bool) {
	switch strings.ToLower(u) {
	case "d", "day", "days":
		return 24 * time.Hour, true
	case "h", "hr", "hrs", "hour", "hours":
		return time.Hour, true
	case "m", "min", "mins", "minute", "minutes":
		return time.Minute, true
	case "s", "sec", "secs", "second", "seconds":
		return time.Second, true
	}
	return 0, false
}

// parseAt reads a "try again at" time: RFC 3339, or a local wall time with or without a date
// ("3:05 PM", "Oct 21st, 2026 3:05 PM"). A time of day alone is the next one.
func parseAt(s string, now time.Time) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	s = ordinalRE.ReplaceAllString(s, "$1")
	loc := now.Location()
	for _, layout := range []string{"Jan 2, 2006 3:04 PM", "Jan 2 2006 3:04 PM", "Jan 2, 2006, 3:04 PM", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, true
		}
	}
	for _, layout := range []string{"3:04 PM", "3:04PM", "15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			at := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc)
			if !at.After(now) {
				at = at.AddDate(0, 0, 1)
			}
			return at, true
		}
	}
	return time.Time{}, false
}

// resetField finds resets_in_seconds (or retry_after_seconds) or resets_at (Unix seconds or
// RFC 3339) anywhere in the event.
func resetField(raw []byte, now time.Time) (time.Time, bool) {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return time.Time{}, false
	}
	var walk func(any) (time.Time, bool)
	walk = func(v any) (time.Time, bool) {
		switch x := v.(type) {
		case map[string]any:
			for _, k := range []string{"resets_in_seconds", "retry_after_seconds"} {
				if n, ok := x[k].(float64); ok && n > 0 {
					return now.Add(time.Duration(n * float64(time.Second))), true
				}
			}
			switch r := x["resets_at"].(type) {
			case float64:
				return time.Unix(int64(r), 0), true
			case string:
				if t, err := time.Parse(time.RFC3339, r); err == nil {
					return t, true
				}
			}
			for _, c := range x {
				if t, ok := walk(c); ok {
					return t, true
				}
			}
		case []any:
			for _, c := range x {
				if t, ok := walk(c); ok {
					return t, true
				}
			}
		}
		return time.Time{}, false
	}
	return walk(v)
}
