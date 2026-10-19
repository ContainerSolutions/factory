package clirun

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
		{"at a lower-case time", "", "usage limit, resets at 3pm", time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)},
		{"at a date without a year", "", "usage limit. Try again at oct 21 at 9:30am.", time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)},
		{"at RFC 3339", "", "usage limit; try again at 2026-10-19T18:00:00Z", time.Date(2026, 10, 19, 18, 0, 0, 0, time.UTC)},
		{"resets_in_seconds", `{"type":"error","error":{"resets_in_seconds":600}}`, "usage limit", now.Add(10 * time.Minute)},
		{"resets_at unix", `{"error":{"info":[{"resets_at":1792440000}]}}`, "usage limit", time.Unix(1792440000, 0)},
		{"no reset", "", "429 Too Many Requests", now.Add(def)},
		{"in the past", `{"resets_at":"2026-10-18T00:00:00Z"}`, "usage limit", now.Add(def)},
		{"too far", "", "usage limit: try again in 30 days", now.Add(def)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResetTime([]byte(tt.raw), tt.msg, now, def); !got.Equal(tt.want) {
				t.Errorf("ResetTime = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestParseAt(t *testing.T) {
	now := time.Date(2026, 12, 30, 14, 30, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Time
	}{
		{"3pm", time.Date(2026, 12, 30, 15, 0, 0, 0, time.UTC)},
		{"2 PM", time.Date(2026, 12, 31, 14, 0, 0, 0, time.UTC)},
		{"14:45", time.Date(2026, 12, 30, 14, 45, 0, 0, time.UTC)},
		{"Jan 2, 9am", time.Date(2027, 1, 2, 9, 0, 0, 0, time.UTC)},
		{"Dec 31 at 1:15 pm", time.Date(2026, 12, 31, 13, 15, 0, 0, time.UTC)},
		{"Oct 21st, 2026 3:05 PM", time.Date(2026, 10, 21, 15, 5, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		got, ok := ParseAt(tt.in, now)
		if !ok || !got.Equal(tt.want) {
			t.Errorf("ParseAt(%q) = %s, %v; want %s", tt.in, got, ok, tt.want)
		}
	}
	if _, ok := ParseAt("whenever", now); ok {
		t.Error("ParseAt(whenever) parsed")
	}
}
