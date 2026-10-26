package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"codex-lazy-runner/internal/codex"
	"codex-lazy-runner/internal/store"
)

const fakeThread = "0199aaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

var fakeBin string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "fakecodex")
	if err != nil {
		panic(err)
	}
	fakeBin = filepath.Join(tmp, "codex")
	if out, err := exec.Command("go", "build", "-o", fakeBin, "./testdata/fakecodex").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build fakecodex: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

type env struct {
	t     *testing.T
	fake  string // FAKE_CODEX_DIR
	home  string // CODEX_HOME
	st    *store.Store
	cfg   Config
	tasks chan int
}

func newEnv(t *testing.T, script string) *env {
	e := &env{t: t, fake: t.TempDir(), home: t.TempDir(), tasks: make(chan int, 1)}
	t.Setenv("FAKE_CODEX_DIR", e.fake)
	var err error
	if e.st, err = store.Open(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(e.fake, "script"), []byte(script), 0o644)
	e.cfg = Config{
		CodexBin: fakeBin, CodexHome: e.home, Store: e.st, Out: io.Discard,
		Poll: 300 * time.Millisecond, Tick: 20 * time.Millisecond, LockPoll: 50 * time.Millisecond,
		ErrorBackoff: 10 * time.Millisecond, MaxErrors: 3,
	}
	return e
}

func (e *env) account(allowed bool, primary, secondary float64, resetIn time.Duration, credits int) {
	reset := time.Now().Add(resetIn).Unix()
	acc := fmt.Sprintf(`{"ordinaryUsageAllowed":%t,"rateLimits":{"limitId":"codex",
		"primary":{"usedPercent":%v,"windowDurationMins":300,"resetsAt":%d},
		"secondary":{"usedPercent":%v,"windowDurationMins":10080,"resetsAt":%d}},
		"rateLimitResetCredits":{"availableCount":%d,"credits":[]}}`,
		allowed, primary, reset, secondary, reset+86400, credits)
	if err := os.WriteFile(filepath.Join(e.fake, "account.json"), []byte(acc), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) newTask() *store.Task {
	return &store.Task{ID: store.NewID(), Cwd: e.fake, ContinuePrompt: "继续", Status: store.StatusRunning, CreatedAt: time.Now()}
}

func (e *env) start(task *store.Task) {
	go func() { e.tasks <- New(e.cfg).Start(context.Background(), task, codex.ExecArgs("do it", nil)) }()
}

func (e *env) takeover(task *store.Task, hit bool, msg string) {
	go func() { e.tasks <- New(e.cfg).Takeover(context.Background(), task, hit, msg) }()
}

func (e *env) result() int {
	select {
	case code := <-e.tasks:
		return code
	case <-time.After(15 * time.Second):
		e.t.Fatal("runner did not finish")
		return -1
	}
}

func (e *env) task(id string) *store.Task {
	st, err := e.st.Load()
	if err != nil {
		e.t.Fatal(err)
	}
	t, _ := st.Find(id)
	if t == nil {
		e.t.Fatalf("task %s not stored", id)
	}
	return t
}

func (e *env) waitStatus(id string, want store.Status) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := e.st.Load(); st != nil {
			if t, _ := st.Find(id); t != nil && t.Status == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("task %s never reached %s", id, want)
}

func (e *env) calls() [][]string {
	b, _ := os.ReadFile(filepath.Join(e.fake, "calls.jsonl"))
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var args []string
		json.Unmarshal([]byte(line), &args)
		out = append(out, args)
	}
	return out
}

func TestResumesWhenQuotaReturns(t *testing.T) {
	e := newEnv(t, "limit ok")
	e.account(false, 100, 20, time.Hour, 0)
	task := e.newTask()
	e.start(task)
	e.waitStatus(task.ID, store.StatusWaiting)
	if got := e.task(task.ID); got.ResumeAt.Before(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("resume_at = %v, want about an hour ahead", got.ResumeAt)
	}
	// Quota comes back early, e.g. a reset credit used from the web UI.
	e.account(true, 0, 20, 5*time.Hour, 0)
	if code := e.result(); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	calls := e.calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
	want := []string{"exec", "resume", "--json", "--skip-git-repo-check", "--", fakeThread, "继续"}
	if strings.Join(calls[1], " ") != strings.Join(want, " ") {
		t.Fatalf("resume call = %q, want %q", calls[1], want)
	}
	got := e.task(task.ID)
	if got.Status != store.StatusCompleted || got.Resumes != 1 || got.SessionID != fakeThread {
		t.Fatalf("task = %+v", got)
	}
}

func TestWakeForce(t *testing.T) {
	e := newEnv(t, "limit ok")
	e.account(false, 100, 20, time.Hour, 0)
	task := e.newTask()
	e.start(task)
	e.waitStatus(task.ID, store.StatusWaiting)
	e.st.RequestWake(task.ID, store.WakeForce)
	if code := e.result(); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
}

func TestWeeklyLimitParksTask(t *testing.T) {
	e := newEnv(t, "limit ok")
	e.account(false, 100, 100, time.Hour, 2)
	task := e.newTask()
	e.start(task)
	if code := e.result(); code != ExitWeekly {
		t.Fatalf("exit = %d", code)
	}
	if got := e.task(task.ID); got.Status != store.StatusWeekly {
		t.Fatalf("status = %s", got.Status)
	}
	if n := len(e.calls()); n != 1 {
		t.Fatalf("calls = %d", n)
	}
	if _, err := os.Stat(filepath.Join(e.fake, "consumed")); err == nil {
		t.Fatal("credit consumed without --auto-credit")
	}
}

func TestAutoCredit(t *testing.T) {
	e := newEnv(t, "limit ok")
	e.cfg.AutoCredit = true
	e.account(false, 100, 20, 3*time.Hour, 1)
	task := e.newTask()
	e.start(task)
	if code := e.result(); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if _, err := os.Stat(filepath.Join(e.fake, "consumed")); err != nil {
		t.Fatal("no credit consumed")
	}
}

func TestAutoCreditSkippedForShortWait(t *testing.T) {
	e := newEnv(t, "limit ok")
	e.cfg.AutoCredit, e.cfg.AutoCreditMinWait = true, time.Hour
	e.account(false, 100, 20, 30*time.Minute, 1)
	task := e.newTask()
	e.start(task)
	e.waitStatus(task.ID, store.StatusWaiting)
	e.st.RequestWake(task.ID, store.WakeForce)
	if code := e.result(); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if _, err := os.Stat(filepath.Join(e.fake, "consumed")); err == nil {
		t.Fatal("credit consumed although reset is near")
	}
}

func TestFailsFastWithoutSession(t *testing.T) {
	e := newEnv(t, "nostart")
	e.account(true, 0, 0, time.Hour, 0)
	task := e.newTask()
	e.start(task)
	if code := e.result(); code != ExitFailed {
		t.Fatalf("exit = %d", code)
	}
	got := e.task(task.ID)
	if got.Status != store.StatusFailed || !strings.Contains(got.Note, "trusted directory") {
		t.Fatalf("task = %+v", got)
	}
}

func TestRetriesTransientErrors(t *testing.T) {
	e := newEnv(t, "error ok")
	e.account(true, 10, 10, time.Hour, 0)
	task := e.newTask()
	e.start(task)
	if code := e.result(); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	calls := e.calls()
	if len(calls) != 2 || calls[1][1] != "resume" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestCancelWhileWaiting(t *testing.T) {
	e := newEnv(t, "limit ok")
	e.account(false, 100, 20, time.Hour, 0)
	task := e.newTask()
	e.start(task)
	e.waitStatus(task.ID, store.StatusWaiting)
	e.st.RequestWake(task.ID, store.WakeCancel)
	if code := e.result(); code != ExitCanceled {
		t.Fatalf("exit = %d", code)
	}
	if got := e.task(task.ID); got.Status != store.StatusCanceled {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestCancelWhileRunning(t *testing.T) {
	e := newEnv(t, "hang")
	e.account(true, 0, 0, time.Hour, 0)
	task := e.newTask()
	e.start(task)
	e.waitStatus(task.ID, store.StatusRunning)
	time.Sleep(200 * time.Millisecond)
	e.st.RequestWake(task.ID, store.WakeCancel)
	if code := e.result(); code != ExitCanceled {
		t.Fatalf("exit = %d", code)
	}
}

// Without app-server, the reset time comes from the session log.
func TestTakeoverFallsBackToSessionLog(t *testing.T) {
	e := newEnv(t, "ok")
	reset := time.Now().Add(2 * time.Second).Unix()
	dir := filepath.Join(e.home, "sessions", "2026", "10", "26")
	os.MkdirAll(dir, 0o755)
	log := fmt.Sprintf(`{"type":"session_meta","payload":{"id":"%[1]s","cwd":"%[2]s","source":"exec"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"a"}}
{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":100,"window_minutes":300,"resets_at":%[3]d},"secondary":{"used_percent":5,"window_minutes":10080,"resets_at":%[3]d}}}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"a","error":{"message":"You've hit your usage limit.","codex_error_info":"usage_limit_exceeded"}}}
`, fakeThread, e.fake, reset)
	os.WriteFile(filepath.Join(dir, "rollout-2026-10-26T10-00-00-"+fakeThread+".jsonl"), []byte(log), 0o644)

	task := e.newTask()
	task.SessionID = fakeThread
	start := time.Now()
	e.takeover(task, true, "You've hit your usage limit.")
	if code := e.result(); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if time.Since(start) < time.Second {
		t.Fatal("resumed before the logged reset time")
	}
	if calls := e.calls(); len(calls) != 1 || calls[0][1] != "resume" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestTakeoverWaitsForSessionLock(t *testing.T) {
	e := newEnv(t, "ok")
	e.account(true, 0, 0, time.Hour, 0)
	lockDir := filepath.Join(e.home, "thread-writer-locks")
	os.MkdirAll(lockDir, 0o755)
	f, err := os.Create(filepath.Join(lockDir, fakeThread+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	task := e.newTask()
	task.SessionID = fakeThread
	e.takeover(task, true, "")
	e.waitStatus(task.ID, store.StatusWaiting)
	if n := len(e.calls()); n != 0 {
		t.Fatalf("resumed while the session was locked")
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if code := e.result(); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
}

// When app-server says quota is usable but codex still refuses, the runner
// must back off instead of resuming in a tight loop.
func TestNoSpinOnInconsistentQuota(t *testing.T) {
	e := newEnv(t, "limit limit limit ok")
	e.cfg.Poll = time.Hour
	e.account(true, 50, 20, time.Hour, 0)
	task := e.newTask()
	e.start(task)
	time.Sleep(500 * time.Millisecond)
	if n := len(e.calls()); n != 2 {
		t.Fatalf("calls = %d, want 2 (initial + one immediate resume)", n)
	}
	e.st.RequestWake(task.ID, store.WakeCancel)
	if code := e.result(); code != ExitCanceled {
		t.Fatalf("exit = %d", code)
	}
}
