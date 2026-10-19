package codex

import (
	"testing"
	"time"
)

func TestResetTime(t *testing.T) {
	now := time.Date(2026, 10, 19, 14, 30, 0, 0, time.UTC)
	def := DefaultThrottle
	tests := []struct {
		name string
		raw  string
		msg  string
		want time.Time
	}{
		{"days hours minutes", "", "You've hit your usage limit. Try again in 2 days 3 hours 5 minutes.", now.Add(51*time.Hour + 5*time.Minute)},
		{"seconds", "", "rate limit: try again in 45 seconds", now.Add(45 * time.Second)},
		{"short units", "", "Rate limit reached. Please try again in 1h20m.", now.Add(80 * time.Minute)},
		{"resets in", "", "usage limit, resets in 30 min", now.Add(30 * time.Minute)},
		{"at a time today", "", "You've hit your usage limit. Try again at 3:05 PM.", time.Date(2026, 10, 19, 15, 5, 0, 0, time.UTC)},
		{"at a time tomorrow", "", "You've hit your usage limit. Try again at 9:00 AM.", time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)},
		{"at a date", "", "You've hit your usage limit. Try again at Oct 21st, 2026 3:05 PM.", time.Date(2026, 10, 21, 15, 5, 0, 0, time.UTC)},
		{"at RFC 3339", "", "usage limit; try again at 2026-10-19T18:00:00Z", time.Date(2026, 10, 19, 18, 0, 0, 0, time.UTC)},
		{"resets_in_seconds", `{"type":"error","error":{"resets_in_seconds":600}}`, "usage limit", now.Add(10 * time.Minute)},
		{"resets_at unix", `{"error":{"info":[{"resets_at":1792440000}]}}`, "usage limit", time.Unix(1792440000, 0)},
		{"no reset", "", "429 Too Many Requests", now.Add(def)},
		{"in the past", `{"resets_at":"2026-10-18T00:00:00Z"}`, "usage limit", now.Add(def)},
		{"too far", "", "usage limit: try again in 30 days", now.Add(def)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resetTime([]byte(tt.raw), tt.msg, now, def); !got.Equal(tt.want) {
				t.Errorf("resetTime = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestIsRateLimit(t *testing.T) {
	for msg, want := range map[string]bool{
		"You've hit your usage limit.":                             true,
		"exceeded retry limit, last status: 429 Too Many Requests": true,
		"Rate limit reached for requests":                          true,
		"insufficient_quota":                                       true,
		"stream disconnected before completion":                    false,
		"command failed with exit code 1":                          false,
	} {
		if got := isRateLimit(msg); got != want {
			t.Errorf("isRateLimit(%q) = %v", msg, got)
		}
	}
}
