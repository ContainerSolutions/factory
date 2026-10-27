package claudecode

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ContainerSolutions/factory/internal/agent/harness/clirun"
)

var (
	// "Claude AI usage limit reached|1792418700": the reset as Unix seconds after the bar.
	// UNVERIFIED (owner to verify on a VM workspace run)
	pipeResetRE = regexp.MustCompile(`(?i)limit reached\s*\|\s*([0-9]{9,11})\b`)
	// "resets 3pm", "resets 3:30pm (Europe/Lisbon)", "resets Oct 21, 9am": a wall time, with an
	// optional date and IANA zone.
	// UNVERIFIED (owner to verify on a VM workspace run)
	resetsRE = regexp.MustCompile(`(?i)\bresets\s+(?:at\s+)?((?:[a-z]{3} [0-9]{1,2}(?:st|nd|rd|th)?,?\s+(?:at\s+)?)?[0-9]{1,2}(?::[0-9]{2})?\s*[ap]m)(?:\s*\(([^)]+)\))?`)
)

// resetTime is when a claude rate limit ends: the Unix time after "limit reached|", a
// "resets <time> (<zone>)", else what clirun.ResetTime finds (a reset field, "try again in
// …", "resets at …"), else now + def. A reset in the past or beyond clirun.MaxThrottle is
// ignored.
func resetTime(raw []byte, msg string, now time.Time, def time.Duration) time.Time {
	if m := pipeResetRE.FindStringSubmatch(msg); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			if t := time.Unix(n, 0); clirun.ResetPlausible(t, now) {
				return t
			}
		}
	}
	if m := resetsRE.FindStringSubmatch(msg); m != nil {
		loc := now.Location()
		if z := strings.TrimSpace(m[2]); z != "" {
			if l, err := time.LoadLocation(z); err == nil {
				loc = l
			}
		}
		if t, ok := clirun.ParseAt(m[1], now.In(loc)); ok && clirun.ResetPlausible(t, now) {
			return t
		}
	}
	return clirun.ResetTime(raw, msg, now, def)
}
