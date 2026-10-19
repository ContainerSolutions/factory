package claudecode

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
		{"unix after the bar", "", "Claude AI usage limit reached|1792431000", time.Unix(1792431000, 0)},
		{"unix in the past", "", "Claude AI usage limit reached|1792400000", now.Add(def)},
		{"resets a time", "", "5-hour limit reached ∙ resets 3pm", time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)},
		{"resets a time with minutes, tomorrow", "", "You've hit your limit · resets 9:15am", time.Date(2026, 10, 20, 9, 15, 0, 0, time.UTC)},
		{"resets in a zone", "", "You've hit your limit · resets 6pm (America/New_York)", time.Date(2026, 10, 19, 22, 0, 0, 0, time.UTC)},
		{"resets a date", "", "Weekly limit reached ∙ resets Oct 21, 9am", time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)},
		{"resets a date at a time in a zone", "", "Opus limit reached · resets Oct 22 at 1am (Europe/Lisbon)", time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)},
		{"an unknown zone: now's", "", "limit reached · resets 4pm (Mars/Olympus)", time.Date(2026, 10, 19, 16, 0, 0, 0, time.UTC)},
		{"try again in", "", "rate limit: try again in 45 seconds", now.Add(45 * time.Second)},
		{"a reset field", `{"error":{"retry_after_seconds":120}}`, "429", now.Add(2 * time.Minute)},
		{"no reset", "", "API Error: 429 Too Many Requests", now.Add(def)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resetTime([]byte(tt.raw), tt.msg, now, def); !got.Equal(tt.want) {
				t.Errorf("resetTime = %s, want %s", got, tt.want)
			}
		})
	}
}
