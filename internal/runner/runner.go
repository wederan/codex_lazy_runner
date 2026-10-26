// Package runner drives a Codex task to completion: it runs codex, waits
// out exhausted quotas and resumes the session once quota is usable again.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"codex-lazy-runner/internal/appserver"
	"codex-lazy-runner/internal/codex"
	"codex-lazy-runner/internal/limit"
	"codex-lazy-runner/internal/notify"
	"codex-lazy-runner/internal/sessions"
	"codex-lazy-runner/internal/store"
)

const DefaultContinuePrompt = "上一轮因 Codex 额度限制中断。请先检查当前进度，然后从中断处继续完成原任务；如果任务已经全部完成，请简要总结结果。"

// Process exit codes.
const (
	ExitOK          = 0
	ExitFailed      = 1
	ExitWeekly      = 3
	ExitCanceled    = 4
	ExitInterrupted = 130
)

type Config struct {
	CodexBin  string
	CodexHome string
	Store     *store.Store
	Out       io.Writer

	Poll         time.Duration // how often to re-check quota while waiting
	Tick         time.Duration // granularity of wake-request and clock checks
	Buffer       time.Duration // slack added after the reported reset time
	LockPoll     time.Duration // how often to re-check a held session lock
	ErrorBackoff time.Duration // base delay before retrying a non-quota failure
	MaxResumes   int
	MaxErrors    int

	AutoCredit        bool          // redeem a reset credit instead of waiting
	AutoCreditMinWait time.Duration // only when the wait is at least this long
	Notify            bool
}

type Runner struct {
	cfg       Config
	app       *appserver.Client
	codex     *codex.Runner
	store     *store.Store
	appWarned bool
}

var (
	errCanceled = errors.New("任务已取消")
	errWeekly   = errors.New("周额度已用完")
)

func New(cfg Config) *Runner {
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.Poll, 5*time.Minute)
	def(&cfg.Tick, 2*time.Second)
	def(&cfg.LockPoll, 15*time.Second)
	def(&cfg.ErrorBackoff, time.Minute)
	if cfg.Buffer < 0 {
		cfg.Buffer = 0
	}
	if cfg.MaxResumes <= 0 {
		cfg.MaxResumes = 20
	}
	if cfg.MaxErrors < 0 {
		cfg.MaxErrors = 0
	}
	return &Runner{
		cfg:   cfg,
		app:   &appserver.Client{Bin: cfg.CodexBin},
		codex: &codex.Runner{Bin: cfg.CodexBin, Out: cfg.Out},
		store: cfg.Store,
	}
}

// Start runs a new task with `codex exec` and resumes it until it finishes.
func (r *Runner) Start(ctx context.Context, t *store.Task, execArgs []string) int {
	return r.drive(ctx, t, execArgs, false, "")
}

// Takeover adopts an existing session. hit tells whether its last turn
// stopped on the usage limit, lastMsg is that turn's error text.
func (r *Runner) Takeover(ctx context.Context, t *store.Task, hit bool, lastMsg string) int {
	return r.drive(ctx, t, nil, hit, lastMsg)
}

// drive runs args (or, when nil, waits for quota and resumes the session)
// in a loop until the turn completes or the task cannot continue.
func (r *Runner) drive(ctx context.Context, t *store.Task, args []string, hit bool, lastMsg string) int {
	r.store.TakeWake(t.ID) // drop stale requests meant for a previous process
	t.PID = os.Getpid()
	needQuota := args == nil
	errCount := 0
	var minWait time.Duration
	quick := false // the last quota check passed without waiting
	for {
		if args == nil {
			if needQuota {
				waited, err := r.waitUsable(ctx, t, hit, lastMsg, minWait)
				if err != nil {
					return r.stop(t, err)
				}
				quick = !waited
			}
			if err := r.waitLock(ctx, t); err != nil {
				return r.stop(t, err)
			}
			if t.Resumes >= r.cfg.MaxResumes {
				return r.fail(t, fmt.Sprintf("已达到最大续跑次数 %d", r.cfg.MaxResumes))
			}
			t.Resumes++
			r.logf("第 %d 次续跑会话 %s", t.Resumes, t.SessionID)
			args = codex.ResumeArgs(t.SessionID, t.ContinuePrompt, t.CodexArgs)
		}

		t.Status, t.ResumeAt, t.Note = store.StatusRunning, time.Time{}, ""
		t.Attempts++
		r.save(t)
		res, canceled, err := r.runCodex(ctx, t, args)
		args = nil
		switch {
		case canceled:
			return r.stop(t, errCanceled)
		case ctx.Err() != nil:
			return r.stop(t, ctx.Err())
		case err != nil:
			return r.fail(t, err.Error())
		case res.TurnCompleted && !res.TurnFailed:
			t.Status, t.Note = store.StatusCompleted, ""
			r.save(t)
			msg := fmt.Sprintf("任务 %s 已完成（续跑 %d 次）", t.ID, t.Resumes)
			r.logf("%s", msg)
			r.notify(msg)
			return ExitOK
		case t.SessionID == "":
			// codex failed before creating a session: a usage or config
			// error that retrying will not fix.
			return r.fail(t, "codex 未能启动会话："+firstNonEmpty(res.ErrorMessage, res.Stderr, fmt.Sprint(res.ExitErr)))
		case r.isUsageLimit(ctx, res):
			hit, lastMsg, needQuota, errCount = true, res.ErrorMessage, true, 0
			// Quota looked usable yet codex refused again: don't spin.
			minWait = 0
			if quick {
				minWait = r.cfg.Poll
			}
		default:
			errCount++
			if errCount > r.cfg.MaxErrors {
				return r.fail(t, "codex 连续异常结束："+firstNonEmpty(res.ErrorMessage, res.Stderr, fmt.Sprint(res.ExitErr)))
			}
			delay := r.cfg.ErrorBackoff * time.Duration(errCount)
			r.logf("codex 异常结束（%s），%s 后重试（%d/%d）",
				firstNonEmpty(res.ErrorMessage, fmt.Sprint(res.ExitErr)), delay, errCount, r.cfg.MaxErrors)
			t.Status, t.ResumeAt, t.Note = store.StatusWaiting, wallNow().Add(delay), "异常后等待重试"
			r.save(t)
			if _, _, err := r.sleepUntil(ctx, t.ID, t.ResumeAt); err != nil {
				return r.stop(t, err)
			}
			needQuota, quick = false, false
		}
	}
}

// runCodex runs one codex attempt; a `cancel` request interrupts it.
func (r *Runner) runCodex(ctx context.Context, t *store.Task, args []string) (*codex.Result, bool, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var canceled atomic.Bool
	go func() {
		tk := time.NewTicker(r.cfg.Tick)
		defer tk.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-tk.C:
				if mode, ok := r.store.PeekWake(t.ID); ok && mode == store.WakeCancel {
					canceled.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	logPrefix := filepath.Join(r.store.LogDir(t.ID), fmt.Sprintf("%02d", t.Attempts))
	res, err := r.codex.Run(runCtx, t.Cwd, args, logPrefix, func(id string) {
		if t.SessionID == "" {
			t.SessionID = id
			r.logf("codex 会话 ID：%s", id)
			r.save(t)
		}
	})
	if canceled.Load() {
		r.store.TakeWake(t.ID)
	}
	return res, canceled.Load(), err
}

func (r *Runner) isUsageLimit(ctx context.Context, res *codex.Result) bool {
	if limit.IsUsageLimitMessage(res.ErrorMessage) {
		return true
	}
	acc, err := r.app.ReadAccount(ctx, false)
	return err == nil && acc.OrdinaryUsageAllowed != nil && !*acc.OrdinaryUsageAllowed
}

// waitUsable blocks until quota is usable, checking no earlier than
// minWait from now, and reports whether it had to wait. It returns
// errWeekly when the weekly quota is exhausted (the task is then parked,
// not waited on).
func (r *Runner) waitUsable(ctx context.Context, t *store.Task, hit bool, lastMsg string, minWait time.Duration) (bool, error) {
	waited := false
	if minWait > 0 {
		r.logf("额度显示可用但 codex 仍提示额度不足，%s 后再检查", minWait)
		t.Status, t.ResumeAt, t.Note = store.StatusWaiting, wallNow().Add(minWait), "等待额度状态同步"
		r.save(t)
		_, mode, err := r.sleepUntil(ctx, t.ID, t.ResumeAt)
		if err != nil || mode == store.WakeForce {
			return true, err
		}
		waited = true
	}
	triedCredit := false
	lastKey := ""
	for {
		snap, src := r.snapshot(ctx, t, hit, lastMsg)
		if ctx.Err() != nil {
			return waited, ctx.Err()
		}
		now := wallNow()
		d := limit.Decide(snap, now)
		var target time.Time
		switch d.Kind {
		case limit.Ready:
			if hit {
				r.logf("额度可用（来源：%s）", src)
			}
			return waited, nil
		case limit.WeeklyBlocked:
			if r.cfg.AutoCredit && !triedCredit && snap.Credits > 0 {
				triedCredit = true
				if r.useCredit(ctx, t) {
					continue
				}
			}
			t.Status, t.ResumeAt = store.StatusWeekly, d.ResumeAt
			t.Note = "周额度已用完；使用额度卡后执行 lazy-runner resume " + t.ID
			r.save(t)
			msg := fmt.Sprintf("任务 %s：周额度已用完（%s 重置），不自动等待。使用额度卡后可执行 lazy-runner resume %s",
				t.ID, FormatTime(d.ResumeAt), t.ID)
			r.logf("%s", msg)
			r.notify(msg)
			return waited, errWeekly
		case limit.WaitReset:
			if r.cfg.AutoCredit && !triedCredit && snap.Credits > 0 && d.ResumeAt.Sub(now) >= r.cfg.AutoCreditMinWait {
				triedCredit = true
				if r.useCredit(ctx, t) {
					continue
				}
			}
			target = d.ResumeAt.Add(r.cfg.Buffer)
		default:
			target = now.Add(r.cfg.Poll)
		}

		if t.Status != store.StatusWaiting || !t.ResumeAt.Equal(target) || t.Note != d.Reason {
			t.Status, t.ResumeAt, t.Note = store.StatusWaiting, target, d.Reason
			r.save(t)
		}
		if key := fmt.Sprint(d.Kind, d.ResumeAt.Unix()); key != lastKey {
			lastKey = key
			var msg string
			if d.Kind == limit.WaitReset {
				msg = fmt.Sprintf("%s，%s 重置，将于 %s 自动续跑", d.Reason, FormatTime(d.ResumeAt), FormatTime(target))
			} else {
				msg = fmt.Sprintf("%s，每 %s 检查一次", d.Reason, r.cfg.Poll)
			}
			r.logf("%s（来源：%s）。执行 lazy-runner wake %s 可立即检查", msg, src, t.ID)
			r.notify(fmt.Sprintf("任务 %s：%s", t.ID, msg))
		}

		next := target
		if p := now.Add(r.cfg.Poll); p.Before(next) {
			next = p
		}
		woke, mode, err := r.sleepUntil(ctx, t.ID, next)
		waited = true
		if err != nil {
			return waited, err
		}
		if mode == store.WakeForce {
			r.logf("收到强制续跑请求")
			return waited, nil
		}
		if woke {
			r.logf("收到唤醒请求，重新检查额度")
			continue
		}
		// Without an authoritative source, just try once the time has come.
		if snap.Allowed == nil && d.Kind == limit.WaitReset && !wallNow().Before(target) {
			return waited, nil
		}
	}
}

// snapshot reads quota from codex app-server, falling back to the session
// log and the error text.
func (r *Runner) snapshot(ctx context.Context, t *store.Task, hit bool, lastMsg string) (limit.Snapshot, string) {
	acc, err := r.app.ReadAccount(ctx, false)
	if err == nil && acc.RateLimits != nil {
		r.appWarned = false
		return SnapshotFromAccount(acc, hit), "codex app-server"
	}
	if err != nil && ctx.Err() == nil && !r.appWarned {
		r.appWarned = true
		r.logf("通过 codex app-server 查询额度失败（%v），改用会话日志估算", err)
	}
	snap := limit.Snapshot{Hit: hit, Credits: -1}
	if t.SessionID != "" {
		if p, err := sessions.FindByID(r.cfg.CodexHome, t.SessionID); err == nil {
			if s, err := sessions.Parse(p); err == nil {
				if rl := s.RateLimits; rl != nil {
					snap.Primary, snap.Secondary = logWindow(rl.Primary), logWindow(rl.Secondary)
				}
				if lastMsg == "" && s.LastError != nil {
					lastMsg = s.LastError.Message
				}
			}
		}
	}
	if at, ok := limit.ParseTryAgain(lastMsg, time.Now()); ok {
		snap.TryAgainAt = at
	}
	return snap, "会话日志"
}

// SnapshotFromAccount converts an app-server account reply.
func SnapshotFromAccount(acc *appserver.Account, hit bool) limit.Snapshot {
	s := limit.Snapshot{Allowed: acc.OrdinaryUsageAllowed, Hit: hit, Credits: -1}
	win := func(w *appserver.Window) *limit.Window {
		if w == nil {
			return nil
		}
		return &limit.Window{UsedPercent: w.UsedPercent, ResetsAt: w.ResetTime()}
	}
	if rl := acc.RateLimits; rl != nil {
		s.Primary, s.Secondary = win(rl.Primary), win(rl.Secondary)
	}
	if c := acc.RateLimitResetCredits; c != nil {
		s.Credits = c.AvailableCount
	}
	return s
}

func logWindow(w *sessions.Window) *limit.Window {
	if w == nil {
		return nil
	}
	return &limit.Window{UsedPercent: w.UsedPercent, ResetsAt: time.Unix(w.ResetsAt, 0)}
}

func (r *Runner) useCredit(ctx context.Context, t *store.Task) bool {
	outcome, err := r.app.ConsumeResetCredit(ctx, "", appserver.NewIdempotencyKey())
	if err != nil {
		r.logf("自动使用额度卡失败：%v", err)
		return false
	}
	r.logf("自动使用额度卡，结果：%s", outcome)
	if outcome != appserver.OutcomeReset {
		return false
	}
	r.notify(fmt.Sprintf("任务 %s：已自动使用一张额度卡重置额度", t.ID))
	return true
}

// waitLock waits while another process (Codex Desktop, a TUI...) holds
// the session's writer lock.
func (r *Runner) waitLock(ctx context.Context, t *store.Task) error {
	warned := false
	for {
		held, err := sessions.LockHeld(r.cfg.CodexHome, t.SessionID)
		if err != nil || !held {
			return nil // when unsure, let codex itself decide
		}
		if !warned {
			warned = true
			msg := fmt.Sprintf("会话 %s 正被其他进程占用（%s）。请在 Codex Desktop/TUI 中关闭该会话或退出程序，释放后自动续跑",
				shortID(t.SessionID), sessions.LockHolder(r.cfg.CodexHome, t.SessionID))
			t.Status, t.Note = store.StatusWaiting, "等待会话锁释放"
			r.save(t)
			r.logf("%s", msg)
			r.notify(fmt.Sprintf("任务 %s：%s", t.ID, msg))
		}
		_, mode, err := r.sleepUntil(ctx, t.ID, wallNow().Add(r.cfg.LockPoll))
		if err != nil {
			return err
		}
		if mode == store.WakeForce {
			return nil
		}
	}
}

// sleepUntil waits until the wall-clock time `until` or a wake request.
// It polls the wall clock instead of using one long timer because Go's
// monotonic clock stops while macOS sleeps.
func (r *Runner) sleepUntil(ctx context.Context, id string, until time.Time) (woke bool, mode string, err error) {
	tk := time.NewTicker(r.cfg.Tick)
	defer tk.Stop()
	for {
		if mode, ok := r.store.TakeWake(id); ok {
			if mode == store.WakeCancel {
				return false, mode, errCanceled
			}
			return true, mode, nil
		}
		if !wallNow().Before(until) {
			return false, "", nil
		}
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-tk.C:
		}
	}
}

func (r *Runner) stop(t *store.Task, err error) int {
	switch {
	case errors.Is(err, errWeekly):
		return ExitWeekly
	case errors.Is(err, errCanceled):
		t.Status, t.Note = store.StatusCanceled, ""
		r.save(t)
		r.logf("任务 %s 已取消", t.ID)
		return ExitCanceled
	}
	t.Status, t.Note = store.StatusInterrupted, "lazy-runner 被中断；执行 lazy-runner resume "+t.ID+" 可继续"
	r.save(t)
	r.logf("已中断。执行 lazy-runner resume %s 可继续", t.ID)
	return ExitInterrupted
}

func (r *Runner) fail(t *store.Task, reason string) int {
	t.Status, t.Note = store.StatusFailed, reason
	r.save(t)
	msg := fmt.Sprintf("任务 %s 失败：%s", t.ID, reason)
	r.logf("%s", msg)
	r.notify(msg)
	return ExitFailed
}

func (r *Runner) save(t *store.Task) {
	if err := r.store.Put(t); err != nil {
		r.logf("保存任务状态失败：%v", err)
	}
}

func (r *Runner) logf(format string, args ...any) {
	fmt.Fprintf(r.cfg.Out, "[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func (r *Runner) notify(msg string) {
	if r.cfg.Notify {
		notify.Send("Codex 自动续跑", msg)
	}
}

// wallNow strips the monotonic reading so comparisons use the wall clock.
func wallNow() time.Time { return time.Now().Round(0) }

// FormatTime renders t compactly in local time.
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	now := time.Now()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

// FormatDuration renders d as e.g. "45m", "3h45m" or "6d22h".
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Minute)
	days, hours, mins := int(d/(24*time.Hour)), int(d/time.Hour)%24, int(d/time.Minute)%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

func shortID(id string) string {
	if len(id) > 13 {
		return id[:13] + "…"
	}
	return id
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
