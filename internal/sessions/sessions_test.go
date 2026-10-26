package sessions

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const (
	limitedID  = "01a16c5d-7a42-76d1-a339-00ca78c9037a"
	subagentID = "01a16c5d-2cbe-76e3-b0a9-99c4d7cd91b9"
	finishedID = "01a1760e-afdb-74f0-8dca-1f5ace2f3b02"
	parentID   = "01a1615c-0f92-75d1-b99f-c05994500f47"
)

// Lines follow the shape of real codex 0.15x rollouts.
var sampleRollouts = map[string]string{
	limitedID: `{"timestamp":"2026-10-24T05:23:47.907Z","type":"session_meta","payload":{"id":"` + limitedID + `","forked_from_id":"` + parentID + `","cwd":"/work/app","originator":"codex_cli_rs","cli_version":"0.154.0","source":"cli"}}
{"timestamp":"2026-10-24T05:23:47.910Z","type":"session_meta","payload":{"id":"` + parentID + `","cwd":"/elsewhere","source":"cli"}}
{"timestamp":"2026-10-24T05:23:47.933Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1","started_at":1792819427}}
{"timestamp":"2026-10-24T05:25:47.511Z","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"limit_id":"codex","primary":{"used_percent":91.0,"window_minutes":300,"resets_at":1792998478},"secondary":{"used_percent":14.0,"window_minutes":10080,"resets_at":1793585278}}}}
{"timestamp":"2026-10-24T05:25:47.512Z","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"limit_id":"premium","primary":null,"secondary":null}}}
{"timestamp":"2026-10-24T05:25:50.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working"}]}}
{"timestamp":"2026-10-24T05:27:32.043Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1","last_agent_message":null,"error":{"message":"You’ve hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 3:07 PM.","codex_error_info":"usage_limit_exceeded"}}}
`,
	subagentID: `{"type":"session_meta","payload":{"id":"` + subagentID + `","cwd":"/work/app","source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parentID + `","depth":1}}}}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"s1"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"s1","error":{"message":"You've hit your usage limit.","codex_error_info":"usage_limit_exceeded"}}}
`,
	finishedID: `{"type":"session_meta","payload":{"id":"` + finishedID + `","cwd":"/work/other","source":"exec"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"a"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"a","error":{"message":"You've hit your usage limit.","codex_error_info":"usage_limit_exceeded"}}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"b"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"b","last_agent_message":"done"}}
`,
}

// writeHome lays the rollouts out like $CODEX_HOME; the subagent is newest,
// then the finished session, then the limited one.
func writeHome(t *testing.T) string {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "10", "24")
	os.MkdirAll(dir, 0o755)
	age := map[string]time.Duration{subagentID: time.Minute, finishedID: 2 * time.Minute, limitedID: 3 * time.Minute}
	for id, body := range sampleRollouts {
		p := filepath.Join(dir, "rollout-2026-10-24T13-23-47-"+id+".jsonl")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age[id])
		os.Chtimes(p, mt, mt)
	}
	os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(
		`{"id":"`+limitedID+`","thread_name":"old name"}`+"\n"+
			`{"id":"`+limitedID+`","thread_name":"refactor parser"}`+"\n"), 0o644)
	return home
}

func TestParse(t *testing.T) {
	home := writeHome(t)
	p, err := FindByID(home, limitedID)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() != limitedID || s.Meta.Cwd != "/work/app" {
		t.Errorf("meta = %+v", s.Meta)
	}
	if !s.UsageLimited() || s.TurnOpen {
		t.Errorf("UsageLimited = %v, TurnOpen = %v", s.UsageLimited(), s.TurnOpen)
	}
	if s.RateLimits == nil || s.RateLimits.Primary.ResetsAt != 1792998478 {
		t.Errorf("rate limits = %+v", s.RateLimits)
	}

	p, _ = FindByID(home, finishedID)
	if s, _ := Parse(p); s.UsageLimited() {
		t.Error("session that completed after a limit is still reported as limited")
	}
}

func TestLatestUsageLimitedSkipsSubagents(t *testing.T) {
	home := writeHome(t)
	s, err := LatestUsageLimited(home, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() != limitedID {
		t.Errorf("got %s, want %s", s.ID(), limitedID)
	}
	if _, err := LatestUsageLimited(home, time.Now()); err == nil {
		t.Error("expected no session within an empty window")
	}
}

func TestResolveID(t *testing.T) {
	home := writeHome(t)
	if got := ResolveID(home, "refactor parser"); got != limitedID {
		t.Errorf("ResolveID = %s", got)
	}
	if got := ResolveID(home, "old name"); got != "old name" {
		t.Errorf("renamed-away name resolved to %s", got)
	}
}

func TestLockHeld(t *testing.T) {
	home := t.TempDir()
	if held, err := LockHeld(home, limitedID); held || err != nil {
		t.Fatalf("missing lock file: held=%v err=%v", held, err)
	}
	os.MkdirAll(filepath.Join(home, "thread-writer-locks"), 0o755)
	f, err := os.Create(lockPath(home, limitedID))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if held, _ := LockHeld(home, limitedID); held {
		t.Fatal("unlocked file reported as held")
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	if held, _ := LockHeld(home, limitedID); !held {
		t.Fatal("locked file reported as free")
	}
}
