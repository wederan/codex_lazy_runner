package limit

import (
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.Local)
	in := func(d time.Duration) time.Time { return now.Add(d) }
	yes, no := true, false
	full := func(d time.Duration) *Window { return &Window{UsedPercent: 100, ResetsAt: in(d)} }
	part := func(d time.Duration) *Window { return &Window{UsedPercent: 40, ResetsAt: in(d)} }

	cases := []struct {
		name string
		snap Snapshot
		kind Kind
		at   time.Time
	}{
		{"allowed", Snapshot{Allowed: &yes, Primary: full(time.Hour)}, Ready, time.Time{}},
		{"primary exhausted", Snapshot{Allowed: &no, Primary: full(time.Hour), Secondary: part(48 * time.Hour)}, WaitReset, in(time.Hour)},
		{"weekly exhausted", Snapshot{Allowed: &no, Primary: full(time.Hour), Secondary: full(48 * time.Hour)}, WeeklyBlocked, in(48 * time.Hour)},
		{"blocked, no window", Snapshot{Allowed: &no, Primary: part(time.Hour)}, Unknown, time.Time{}},
		{"stale reset in the past", Snapshot{Allowed: &no, Primary: full(-time.Minute)}, Unknown, time.Time{}},
		{"logs, no hit", Snapshot{Primary: part(time.Hour)}, Ready, time.Time{}},
		{"logs, hit, try-again", Snapshot{Hit: true, Primary: part(time.Hour), TryAgainAt: in(2 * time.Hour)}, WaitReset, in(2 * time.Hour)},
		{"logs, hit, try-again days away", Snapshot{Hit: true, TryAgainAt: in(30 * time.Hour)}, WeeklyBlocked, in(30 * time.Hour)},
		{"logs, hit, primary reset", Snapshot{Hit: true, Primary: part(time.Hour)}, WaitReset, in(time.Hour)},
		{"logs, hit, nothing known", Snapshot{Hit: true}, Unknown, time.Time{}},
	}
	for _, c := range cases {
		d := Decide(c.snap, now)
		if d.Kind != c.kind || !d.ResumeAt.Equal(c.at) {
			t.Errorf("%s: got kind %d at %v, want %d at %v", c.name, d.Kind, d.ResumeAt, c.kind, c.at)
		}
	}
}

func TestParseTryAgain(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 35, 0, 0, time.Local)
	cases := []struct {
		msg  string
		want time.Time
	}{
		{"You’ve hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 3:07 PM.",
			time.Date(2026, 10, 26, 15, 7, 0, 0, time.Local)},
		{"... or try again at 9:15 AM.", time.Date(2026, 10, 27, 9, 15, 0, 0, time.Local)},
		{"... or try again at Oct 30th, 2026 4:20 PM.", time.Date(2026, 10, 30, 16, 20, 0, 0, time.Local)},
		{"... or try again at Nov 2nd 8:00 AM.", time.Date(2026, 11, 2, 8, 0, 0, 0, time.Local)},
	}
	for _, c := range cases {
		got, ok := ParseTryAgain(c.msg, now)
		if !ok || !got.Equal(c.want) {
			t.Errorf("ParseTryAgain(%q) = %v, %v; want %v", c.msg, got, ok, c.want)
		}
	}
	if _, ok := ParseTryAgain("stream disconnected", now); ok {
		t.Error("parsed a message without a time")
	}
}

func TestIsUsageLimitMessage(t *testing.T) {
	for _, msg := range []string{
		"You've hit your usage limit. Upgrade to Pro",
		"You’ve hit your usage limit.",
		"usage_limit_exceeded",
	} {
		if !IsUsageLimitMessage(msg) {
			t.Errorf("not detected: %q", msg)
		}
	}
	if IsUsageLimitMessage("exceeded retry limit, last status: 429 Too Many Requests") {
		t.Error("transient 429 classified as usage limit")
	}
}
