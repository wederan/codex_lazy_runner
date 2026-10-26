// Command lazy-runner runs Codex tasks and automatically resumes them after
// the Codex usage limit resets (or a reset credit is used).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"codex-lazy-runner/internal/appserver"
	"codex-lazy-runner/internal/codex"
	"codex-lazy-runner/internal/runner"
	"codex-lazy-runner/internal/sessions"
	"codex-lazy-runner/internal/store"
)

const version = "0.1.0"

const usageText = `lazy-runner：Codex 额度用完后自动续跑

用法：
  lazy-runner run [选项] <提示词> [-- <codex exec 参数>]   新建任务，额度耗尽后自动等待并续跑
  lazy-runner resume [选项] <任务ID|会话ID|会话名> [-- <codex 参数>]
                                                         接管已中断的会话（含 TUI/Desktop 中跑断的）
  lazy-runner resume --last                              接管最近一个因额度中断的会话
  lazy-runner resume --list                              列出最近因额度中断的会话
  lazy-runner status [--all]                             查看额度、额度卡和任务
  lazy-runner wake [--force] <任务ID>... | --all         让等待中的任务立即检查额度并续跑
  lazy-runner cancel <任务ID>                            取消任务
  lazy-runner credits list                               列出额度卡
  lazy-runner credits use [--id ID] [--yes]              使用一张额度卡，成功后唤醒等待中的任务

运行 "lazy-runner <命令> -h" 查看各命令的选项。

环境变量：
  CODEX_HOME          codex 数据目录（默认 ~/.codex）
  LAZY_RUNNER_HOME    lazy-runner 数据目录（默认 ~/.codex-lazy-runner）
  LAZY_RUNNER_CODEX   codex 可执行文件（默认 codex）
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return 2
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "resume":
		return cmdResume(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "wake":
		return cmdWake(args[1:])
	case "cancel":
		return cmdCancel(args[1:])
	case "credits":
		return cmdCredits(args[1:])
	case "version", "--version", "-v":
		fmt.Println(version)
		return 0
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return 0
	}
	fmt.Fprintf(os.Stderr, "未知命令 %q\n\n%s", args[0], usageText)
	return 2
}

// ---- shared options ----

type runFlags struct {
	continuePrompt    string
	poll, buffer      time.Duration
	maxResumes        int
	maxErrors         int
	autoCredit        bool
	autoCreditMinWait time.Duration
	noNotify          bool
	keepAwake         bool
	codexBin          string
}

func addRunFlags(fs *flag.FlagSet) *runFlags {
	f := &runFlags{}
	fs.StringVar(&f.continuePrompt, "continue-prompt", runner.DefaultContinuePrompt, "续跑时发送给 codex 的提示词")
	fs.DurationVar(&f.poll, "poll", 5*time.Minute, "等待期间检查额度的间隔（可及时发现额度卡重置）")
	fs.DurationVar(&f.buffer, "buffer", 2*time.Minute, "到达重置时间后再等待的缓冲")
	fs.IntVar(&f.maxResumes, "max-resumes", 20, "最多续跑次数")
	fs.IntVar(&f.maxErrors, "max-errors", 3, "非额度错误的最多连续重试次数")
	fs.BoolVar(&f.autoCredit, "auto-credit", false, "额度耗尽时自动使用额度卡（默认关闭）")
	fs.DurationVar(&f.autoCreditMinWait, "auto-credit-min-wait", time.Hour, "距离自然重置不足该时长时不自动用卡")
	fs.BoolVar(&f.noNotify, "no-notify", false, "不发送系统通知")
	fs.BoolVar(&f.keepAwake, "keep-awake", false, "运行期间阻止系统空闲睡眠（macOS caffeinate）")
	fs.StringVar(&f.codexBin, "codex", "", "codex 可执行文件路径")
	return f
}

func (f *runFlags) config(st *store.Store) runner.Config {
	return runner.Config{
		CodexBin:          codexBin(f.codexBin),
		CodexHome:         codexHome(),
		Store:             st,
		Out:               os.Stdout,
		Poll:              f.poll,
		Buffer:            f.buffer,
		MaxResumes:        f.maxResumes,
		MaxErrors:         f.maxErrors,
		AutoCredit:        f.autoCredit,
		AutoCreditMinWait: f.autoCreditMinWait,
		Notify:            !f.noNotify,
	}
}

func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

func codexBin(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if b := os.Getenv("LAZY_RUNNER_CODEX"); b != "" {
		return b
	}
	return "codex"
}

func appClient(flagValue string) *appserver.Client {
	return &appserver.Client{Bin: codexBin(flagValue)}
}

// parseArgs parses flags interspersed with positional arguments; anything
// after a literal "--" is returned untouched as passthrough.
func parseArgs(fs *flag.FlagSet, args []string) (positional, passthrough []string, err error) {
	before := args
	for i, a := range args {
		if a == "--" {
			before, passthrough = args[:i], args[i+1:]
			break
		}
	}
	for {
		if err := fs.Parse(before); err != nil {
			return nil, nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, passthrough, nil
		}
		positional = append(positional, rest[0])
		before = rest[1:]
	}
}

func newFlagSet(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "用法：lazy-runner %s\n\n选项：\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

func flagExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
}

// keepAwake prevents idle sleep until this process exits.
func keepAwake() {
	if runtime.GOOS != "darwin" {
		return
	}
	cmd := exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "警告："+format+"\n", args...)
}

func errorf(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "错误："+format+"\n", args...)
	return 1
}

func absDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("目录 %s 不存在", abs)
	}
	return abs, nil
}

// ---- run ----

func cmdRun(args []string) int {
	fs := newFlagSet("run", "run [选项] <提示词> [-- <codex exec 参数>]")
	rf := addRunFlags(fs)
	cd := fs.String("cd", "", "工作目录（默认当前目录）")
	promptFile := fs.String("prompt-file", "", "从文件读取提示词（\"-\" 表示标准输入）")
	pos, codexArgs, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}

	prompt := strings.Join(pos, " ")
	if *promptFile != "" || prompt == "-" {
		var b []byte
		if *promptFile != "" && *promptFile != "-" {
			b, err = os.ReadFile(*promptFile)
		} else {
			b, err = io.ReadAll(os.Stdin)
		}
		if err != nil {
			return errorf("读取提示词失败：%v", err)
		}
		prompt = string(b)
	}
	if strings.TrimSpace(prompt) == "" {
		fs.Usage()
		return 2
	}

	var execArgs []string
	for _, a := range codexArgs {
		if a != "--json" {
			execArgs = append(execArgs, a)
		}
	}
	resumeArgs, argCwd, warnings := codex.ResumeCompatible(execArgs)
	for _, w := range warnings {
		warnf("%s", w)
	}
	cwd := "."
	if argCwd != "" {
		cwd = argCwd
	} else if *cd != "" {
		cwd = *cd
	}
	if cwd, err = absDir(cwd); err != nil {
		return errorf("%v", err)
	}

	st, err := store.Default()
	if err != nil {
		return errorf("%v", err)
	}
	t := &store.Task{
		ID:             store.NewID(),
		Cwd:            cwd,
		Prompt:         prompt,
		ContinuePrompt: rf.continuePrompt,
		CodexArgs:      resumeArgs,
		Status:         store.StatusRunning,
		CreatedAt:      time.Now(),
	}
	ctx, stop := signalContext()
	defer stop()
	if rf.keepAwake {
		keepAwake()
	}
	fmt.Printf("任务 %s：在 %s 中运行 codex\n", t.ID, cwd)
	return runner.New(rf.config(st)).Start(ctx, t, codex.ExecArgs(prompt, execArgs))
}

// ---- resume ----

func cmdResume(args []string) int {
	fs := newFlagSet("resume", "resume [选项] <任务ID|会话ID|会话名> | --last | --list [-- <codex 参数>]")
	rf := addRunFlags(fs)
	last := fs.Bool("last", false, "接管最近一个因额度中断的会话")
	list := fs.Bool("list", false, "列出最近因额度中断的会话")
	days := fs.Int("days", 7, "--last/--list 查找最近多少天的会话")
	pos, codexArgs, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	home := codexHome()
	since := time.Now().AddDate(0, 0, -*days)
	if *list {
		return listInterrupted(home, since)
	}
	if *last == (len(pos) == 1) || len(pos) > 1 {
		fs.Usage()
		return 2
	}

	st, err := store.Default()
	if err != nil {
		return errorf("%v", err)
	}
	state, err := st.Load()
	if err != nil {
		return errorf("%v", err)
	}

	var t *store.Task
	var sess *sessions.Session
	sessionID := ""
	if *last {
		if sess, err = sessions.LatestUsageLimited(home, since); err != nil {
			return errorf("最近 %d 天内%v", *days, err)
		}
		sessionID = sess.ID()
		t, _ = state.Find(sessionID)
	} else {
		if t, err = state.Find(pos[0]); err != nil {
			return errorf("%v", err)
		}
		if t != nil {
			sessionID = t.SessionID
		} else {
			sessionID = sessions.ResolveID(home, pos[0])
		}
		if sessionID != "" {
			if p, err := sessions.FindByID(home, sessionID); err == nil {
				sess, _ = sessions.Parse(p)
			} else if t == nil {
				warnf("%v；将直接按会话 ID 续跑", err)
			}
		}
	}

	if t != nil && t.Active() {
		return errorf("任务 %s 正由进程 %d 托管（状态 %s）", t.ID, t.PID, t.Status)
	}
	if t != nil && t.SessionID == "" {
		return errorf("任务 %s 尚未创建 codex 会话，无法续跑", t.ID)
	}
	if t == nil {
		t = &store.Task{ID: store.NewID(), SessionID: sessionID, CreatedAt: time.Now()}
		if sess != nil {
			t.Cwd = sess.Meta.Cwd
		}
	}
	if fsSet(fs, "continue-prompt") || t.ContinuePrompt == "" {
		t.ContinuePrompt = rf.continuePrompt
	}
	if len(codexArgs) > 0 {
		resumeArgs, argCwd, warnings := codex.ResumeCompatible(codexArgs)
		for _, w := range warnings {
			warnf("%s", w)
		}
		t.CodexArgs = resumeArgs
		if argCwd != "" {
			t.Cwd = argCwd
		}
	}
	if t.Cwd == "" {
		t.Cwd = "."
		warnf("无法确定会话的工作目录，使用当前目录")
	}
	if t.Cwd, err = absDir(t.Cwd); err != nil {
		return errorf("%v", err)
	}

	// A task parked by lazy-runner itself stopped on the quota even when
	// the session log is unavailable.
	hit := t.Status == store.StatusWaiting || t.Status == store.StatusWeekly
	lastMsg := ""
	if sess != nil {
		if sess.Meta.IsSubagent() {
			warnf("这是一个子代理会话，通常应续跑它的父会话")
		}
		hit = hit || sess.UsageLimited()
		if sess.LastError != nil {
			lastMsg = sess.LastError.Message
		}
		if !hit {
			fmt.Println("提示：该会话最后一轮不是因额度中断，额度可用时将直接续跑")
		}
	}

	ctx, stop := signalContext()
	defer stop()
	if rf.keepAwake {
		keepAwake()
	}
	fmt.Printf("任务 %s：接管会话 %s（目录 %s）\n", t.ID, t.SessionID, t.Cwd)
	return runner.New(rf.config(st)).Takeover(ctx, t, hit, lastMsg)
}

func fsSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func listInterrupted(home string, since time.Time) int {
	names := sessions.Names(home)
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tLAST_ACTIVE\tLOCKED\tCWD\tNAME")
	n := 0
	for _, s := range sessions.Recent(home, since) {
		if s.Meta.IsSubagent() || !s.UsageLimited() {
			continue
		}
		n++
		locked := "no"
		if held, _ := sessions.LockHeld(home, s.ID()); held {
			locked = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.ID(), runner.FormatTime(s.ModTime), locked, s.Meta.Cwd, clip(names[s.ID()], 40))
	}
	tw.Flush()
	if n == 0 {
		fmt.Println("（没有因额度中断的会话）")
	}
	return 0
}

func clip(s string, n int) string {
	if rs := []rune(s); len(rs) > n {
		return string(rs[:n]) + "…"
	}
	return s
}

// ---- status ----

func cmdStatus(args []string) int {
	fs := newFlagSet("status", "status [选项]")
	all := fs.Bool("all", false, "显示全部任务（默认最近 15 个）")
	noQuota := fs.Bool("no-quota", false, "不查询账户额度")
	bin := fs.String("codex", "", "codex 可执行文件路径")
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	if !*noQuota {
		ctx, stop := signalContext()
		acc, err := appClient(*bin).ReadAccount(ctx, false)
		stop()
		if err != nil {
			warnf("查询额度失败：%v", err)
		} else {
			printQuota(acc)
		}
		fmt.Println()
	}

	st, err := store.Default()
	if err != nil {
		return errorf("%v", err)
	}
	state, err := st.Load()
	if err != nil {
		return errorf("%v", err)
	}
	tasks := state.Sorted()
	if !*all && len(tasks) > 15 {
		tasks = tasks[:15]
	}
	if len(tasks) == 0 {
		fmt.Println("暂无任务")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tSTATUS\tRESUMES\tSESSION\tRESUME_AT\tCWD\tNOTE")
	stale := false
	for _, t := range tasks {
		status := string(t.Status)
		if (t.Status == store.StatusRunning || t.Status == store.StatusWaiting) && !store.ProcessAlive(t.PID) {
			status += "(stale)"
			stale = true
		}
		resumeAt := "-"
		if t.Status == store.StatusWaiting || t.Status == store.StatusWeekly {
			resumeAt = runner.FormatTime(t.ResumeAt)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n", t.ID, status, t.Resumes, orDash(t.SessionID), resumeAt, t.Cwd, clip(t.Note, 60))
	}
	tw.Flush()
	if stale {
		fmt.Println("\nstale 表示托管进程已退出，可用 lazy-runner resume <TASK> 重新接管")
	}
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printQuota(acc *appserver.Account) {
	window := func(name string, w *appserver.Window) {
		if w == nil {
			return
		}
		left := runner.FormatDuration(time.Until(w.ResetTime()))
		fmt.Printf("  %s：已用 %.0f%%，%s 重置（%s 后）\n", name, w.UsedPercent, runner.FormatTime(w.ResetTime()), left)
	}
	fmt.Println("额度：")
	if acc.OrdinaryUsageAllowed != nil {
		ok := "否"
		if *acc.OrdinaryUsageAllowed {
			ok = "是"
		}
		fmt.Printf("  当前可用：%s\n", ok)
	}
	if rl := acc.RateLimits; rl != nil {
		window("5 小时", rl.Primary)
		window("每周  ", rl.Secondary)
	}
	if c := acc.RateLimitResetCredits; c != nil {
		fmt.Printf("  额度卡：%d 张可用\n", c.AvailableCount)
	}
}

// ---- wake / cancel ----

func cmdWake(args []string) int {
	fs := newFlagSet("wake", "wake [--force] <任务ID>... | --all")
	force := fs.Bool("force", false, "不检查额度，直接续跑")
	all := fs.Bool("all", false, "唤醒全部等待中的任务")
	pos, _, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if *all == (len(pos) > 0) {
		fs.Usage()
		return 2
	}
	st, state, code := loadState()
	if state == nil {
		return code
	}
	mode := store.WakeCheck
	if *force {
		mode = store.WakeForce
	}
	var targets []*store.Task
	if *all {
		for _, t := range state.Tasks {
			if t.Status == store.StatusWaiting && t.Active() {
				targets = append(targets, t)
			}
		}
		if len(targets) == 0 {
			fmt.Println("没有等待中的任务")
			return 0
		}
	} else {
		for _, ref := range pos {
			t, err := state.Find(ref)
			if err != nil || t == nil {
				return errorf("找不到任务 %s %v", ref, errOrEmpty(err))
			}
			if !t.Active() {
				return errorf("任务 %s 当前没有托管进程（状态 %s），请用 lazy-runner resume %s", t.ID, t.Status, t.ID)
			}
			targets = append(targets, t)
		}
	}
	for _, t := range targets {
		if err := st.RequestWake(t.ID, mode); err != nil {
			return errorf("%v", err)
		}
		fmt.Printf("已唤醒任务 %s（%s）\n", t.ID, mode)
	}
	return 0
}

func cmdCancel(args []string) int {
	fs := newFlagSet("cancel", "cancel <任务ID>...")
	pos, _, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) == 0 {
		fs.Usage()
		return 2
	}
	st, state, code := loadState()
	if state == nil {
		return code
	}
	for _, ref := range pos {
		t, err := state.Find(ref)
		if err != nil || t == nil {
			return errorf("找不到任务 %s %v", ref, errOrEmpty(err))
		}
		if t.Active() {
			if err := st.RequestWake(t.ID, store.WakeCancel); err != nil {
				return errorf("%v", err)
			}
			fmt.Printf("已请求取消任务 %s（正在运行的 codex 会被中断）\n", t.ID)
			continue
		}
		t.Status, t.Note = store.StatusCanceled, ""
		if err := st.Put(t); err != nil {
			return errorf("%v", err)
		}
		fmt.Printf("任务 %s 已取消\n", t.ID)
	}
	return 0
}

func loadState() (*store.Store, *store.State, int) {
	st, err := store.Default()
	if err != nil {
		return nil, nil, errorf("%v", err)
	}
	state, err := st.Load()
	if err != nil {
		return nil, nil, errorf("%v", err)
	}
	return st, state, 0
}

func errOrEmpty(err error) string {
	if err == nil {
		return ""
	}
	return "（" + err.Error() + "）"
}

// ---- credits ----

func cmdCredits(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, "用法：lazy-runner credits list | use [--id ID] [--yes] [--no-wake]\n")
		return 2
	}
	switch args[0] {
	case "list", "ls":
		return creditsList(args[1:])
	case "use":
		return creditsUse(args[1:])
	}
	fmt.Fprintf(os.Stderr, "未知子命令 %q\n", args[0])
	return 2
}

func creditsList(args []string) int {
	fs := newFlagSet("credits list", "credits list")
	bin := fs.String("codex", "", "codex 可执行文件路径")
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	ctx, stop := signalContext()
	defer stop()
	acc, err := appClient(*bin).ReadAccount(ctx, true)
	if err != nil {
		return errorf("查询失败：%v", err)
	}
	printQuota(acc)
	printCredits(acc)
	return 0
}

func printCredits(acc *appserver.Account) {
	c := acc.RateLimitResetCredits
	switch {
	case c == nil || c.AvailableCount == 0 && len(c.Credits) == 0:
		fmt.Println("\n没有额度卡")
		return
	case len(c.Credits) == 0:
		fmt.Println("\n服务端这次没有返回额度卡明细，可稍后重试")
		return
	}
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tEXPIRES\tTITLE")
	for _, cr := range c.Credits {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", cr.ID, cr.Status, time.Unix(cr.ExpiresAt, 0).Format("2006-01-02 15:04"), cr.Title)
	}
	tw.Flush()
}

func creditsUse(args []string) int {
	fs := newFlagSet("credits use", "credits use [--id ID] [--yes] [--no-wake]")
	id := fs.String("id", "", "指定额度卡 ID（默认由服务端选择）")
	yes := fs.Bool("yes", false, "跳过确认")
	noWake := fs.Bool("no-wake", false, "重置后不唤醒等待中的任务")
	bin := fs.String("codex", "", "codex 可执行文件路径")
	if err := fs.Parse(args); err != nil {
		return flagExit(err)
	}
	ctx, stop := signalContext()
	defer stop()
	app := appClient(*bin)
	acc, err := app.ReadAccount(ctx, false)
	if err != nil {
		return errorf("查询额度失败：%v", err)
	}
	printQuota(acc)
	if c := acc.RateLimitResetCredits; c == nil || c.AvailableCount == 0 {
		return errorf("没有可用的额度卡")
	}
	if !*yes {
		fmt.Print("\n确认使用 1 张额度卡重置额度？此操作不可撤销 [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Println("已放弃")
			return 1
		}
	}
	outcome, err := app.ConsumeResetCredit(ctx, *id, appserver.NewIdempotencyKey())
	if err != nil {
		return errorf("使用额度卡失败：%v", err)
	}
	switch outcome {
	case appserver.OutcomeReset:
		fmt.Println("额度已重置")
	case appserver.OutcomeNothingToReset:
		return errorf("当前没有需要重置的额度窗口，额度卡未消耗")
	case appserver.OutcomeNoCredit:
		return errorf("没有可用的额度卡")
	case appserver.OutcomeAlreadyRedeemed:
		fmt.Println("该请求已兑换过")
	default:
		fmt.Printf("结果：%s\n", outcome)
	}
	if *noWake {
		return 0
	}

	st, state, code := loadState()
	if state == nil {
		return code
	}
	for _, t := range state.Tasks {
		switch {
		case t.Status == store.StatusWaiting && t.Active():
			if err := st.RequestWake(t.ID, store.WakeCheck); err == nil {
				fmt.Printf("已唤醒等待中的任务 %s\n", t.ID)
			}
		case t.Status == store.StatusWeekly, t.Status == store.StatusWaiting:
			fmt.Printf("任务 %s 没有托管进程，执行 lazy-runner resume %s 继续\n", t.ID, t.ID)
		}
	}
	return 0
}
