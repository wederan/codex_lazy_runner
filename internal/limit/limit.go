// Package limit decides when an exhausted Codex quota can be used again.
package limit

import (
	"regexp"
	"strings"
	"time"
)

// Window is one rate-limit window: the 5-hour primary or the weekly secondary.
type Window struct {
	UsedPercent float64
	ResetsAt    time.Time
}

// Snapshot is everything known about the account quota at one moment.
type Snapshot struct {
	// Allowed is the authoritative "can run now" flag from codex app-server;
	// nil when the snapshot was rebuilt from session logs.
	Allowed   *bool
	Primary   *Window
	Secondary *Window
	// TryAgainAt is parsed from the usage-limit error text, if any.
	TryAgainAt time.Time
	// Hit reports that a usage-limit error was observed for the task.
	Hit bool
	// Credits is the number of available reset credits, -1 if unknown.
	Credits int
}

type Kind int

const (
	Ready Kind = iota
	WaitReset
	WeeklyBlocked
	Unknown
)

type Decision struct {
	Kind     Kind
	ResumeAt time.Time
	Reason   string
}

// primaryWindow bounds the 5-hour window; a "try again" time further away
// than this can only come from the weekly limit.
const primaryWindow = 5*time.Hour + 10*time.Minute

// Decide tells whether the task can run now, and if not, until when to wait.
func Decide(s Snapshot, now time.Time) Decision {
	exhausted := func(w *Window) bool {
		return w != nil && w.UsedPercent >= 100 && w.ResetsAt.After(now)
	}
	switch {
	case s.Allowed != nil && *s.Allowed:
		return Decision{Kind: Ready, Reason: "额度可用"}
	case exhausted(s.Secondary):
		return Decision{Kind: WeeklyBlocked, ResumeAt: s.Secondary.ResetsAt, Reason: "周额度已用完"}
	case exhausted(s.Primary):
		return Decision{Kind: WaitReset, ResumeAt: s.Primary.ResetsAt, Reason: "5 小时额度已用完"}
	case s.Allowed != nil:
		return Decision{Kind: Unknown, Reason: "账户暂不可用，且无法确定重置时间"}
	case !s.Hit:
		return Decision{Kind: Ready, Reason: "未发现已耗尽的额度"}
	case s.TryAgainAt.After(now):
		if s.TryAgainAt.Sub(now) > primaryWindow {
			return Decision{Kind: WeeklyBlocked, ResumeAt: s.TryAgainAt, Reason: "周额度已用完"}
		}
		return Decision{Kind: WaitReset, ResumeAt: s.TryAgainAt, Reason: "额度已用完"}
	case s.Primary != nil && s.Primary.ResetsAt.After(now):
		return Decision{Kind: WaitReset, ResumeAt: s.Primary.ResetsAt, Reason: "额度已用完"}
	}
	return Decision{Kind: Unknown, Reason: "额度已用完，但无法确定重置时间"}
}

var (
	usageLimitRe = regexp.MustCompile(`(?i)hit your usage limit|usage[ _]limit`)
	tryAgainRe   = regexp.MustCompile(`(?i)try again at\s+(.+?)\.?\s*$`)
	ordinalRe    = regexp.MustCompile(`(\d)(st|nd|rd|th)\b`)
)

// IsUsageLimitMessage reports whether an error text is Codex's usage-limit error.
func IsUsageLimitMessage(msg string) bool {
	return usageLimitRe.MatchString(msg)
}

// ParseTryAgain extracts the time from "... try again at 3:07 PM." style
// messages, interpreted in now's location.
func ParseTryAgain(msg string, now time.Time) (time.Time, bool) {
	m := tryAgainRe.FindStringSubmatch(strings.TrimSpace(msg))
	if m == nil {
		return time.Time{}, false
	}
	s := strings.Join(strings.Fields(ordinalRe.ReplaceAllString(m[1], "$1")), " ")
	loc := now.Location()

	for _, layout := range []string{"3:04 PM", "3:04PM", "15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			at := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc)
			if at.Before(now.Add(-time.Minute)) {
				at = at.AddDate(0, 0, 1)
			}
			return at, true
		}
	}
	for _, layout := range []string{
		"Jan 2, 2006 3:04 PM", "Jan 2, 2006, 3:04 PM",
		"January 2, 2006 3:04 PM", "January 2, 2006, 3:04 PM",
		"2006-01-02 15:04",
	} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, true
		}
	}
	for _, layout := range []string{"Jan 2 3:04 PM", "Jan 2, 3:04 PM", "January 2 3:04 PM", "January 2, 3:04 PM"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			at := time.Date(now.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
			if at.Before(now.Add(-time.Minute)) {
				at = at.AddDate(1, 0, 0)
			}
			return at, true
		}
	}
	return time.Time{}, false
}
