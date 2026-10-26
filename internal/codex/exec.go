// Package codex runs `codex exec --json` and follows its event stream.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ExecArgs builds `codex exec` arguments for a new task.
func ExecArgs(prompt string, extra []string) []string {
	args := append([]string{"exec", "--json"}, extra...)
	return append(args, "--", prompt)
}

// ResumeArgs builds `codex exec resume` arguments; extra must already be
// resume-compatible (see ResumeCompatible).
func ResumeArgs(sessionID, prompt string, extra []string) []string {
	args := append([]string{"exec", "resume", "--json", "--skip-git-repo-check"}, extra...)
	return append(args, "--", sessionID, prompt)
}

// Result summarizes one codex exec run.
type Result struct {
	ThreadID         string
	TurnCompleted    bool
	TurnFailed       bool
	ErrorMessage     string // turn.failed message, else the last error event
	LastAgentMessage string
	ExitErr          error
	Stderr           string // tail of stderr
}

type Runner struct {
	Bin string
	Out io.Writer // human-readable progress
}

type event struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
	Item *item `json:"item"`
}

type item struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Message  string `json:"message"`
	Command  string `json:"command"`
	ExitCode *int   `json:"exit_code"`
	Changes  []struct {
		Path string `json:"path"`
	} `json:"changes"`
}

// Run executes codex in dir. Raw events go to logPrefix+".jsonl" and stderr
// to logPrefix+".stderr.log"; onThread is called once the session ID is known.
// The returned error is only about starting the process; exit status is in
// Result.ExitErr. Cancelling ctx sends SIGINT to codex's process group.
func (r *Runner) Run(ctx context.Context, dir string, args []string, logPrefix string, onThread func(string)) (*Result, error) {
	if err := os.MkdirAll(filepath.Dir(logPrefix), 0o755); err != nil {
		return nil, err
	}
	events, err := os.Create(logPrefix + ".jsonl")
	if err != nil {
		return nil, err
	}
	defer events.Close()
	errLog, err := os.Create(logPrefix + ".stderr.log")
	if err != nil {
		return nil, err
	}
	defer errLog.Close()

	tail := &tailBuffer{max: 4096}
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Dir = dir
	cmd.Stderr = io.MultiWriter(errLog, tail)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT) }
	cmd.WaitDelay = 20 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 codex 失败: %w", err)
	}

	res := &Result{}
	var lastError string
	br := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			events.Write(line)
			if msg := r.handle(line, res, onThread); msg != "" {
				lastError = msg
			}
		}
		if err != nil {
			break
		}
	}
	res.ExitErr = cmd.Wait()
	res.Stderr = tail.String()
	if res.ErrorMessage == "" {
		res.ErrorMessage = lastError
	}
	return res, nil
}

// handle applies one event to res, prints a summary and returns the
// message of top-level error events.
func (r *Runner) handle(line []byte, res *Result, onThread func(string)) string {
	var ev event
	if json.Unmarshal(line, &ev) != nil {
		return ""
	}
	switch ev.Type {
	case "thread.started":
		res.ThreadID = ev.ThreadID
		if onThread != nil {
			onThread(ev.ThreadID)
		}
	case "turn.completed":
		res.TurnCompleted = true
		r.printf("本轮完成")
	case "turn.failed":
		res.TurnFailed = true
		if ev.Error != nil {
			res.ErrorMessage = ev.Error.Message
		}
		r.printf("本轮失败：%s", res.ErrorMessage)
	case "error":
		r.printf("codex：%s", ev.Message)
		return ev.Message
	case "item.completed":
		if it := ev.Item; it != nil {
			switch it.Type {
			case "agent_message":
				res.LastAgentMessage = it.Text
				r.printf("codex › %s", truncate(it.Text, 1500))
			case "command_execution":
				if it.ExitCode != nil && *it.ExitCode != 0 {
					r.printf("$ %s  (exit %d)", truncate(it.Command, 200), *it.ExitCode)
				} else {
					r.printf("$ %s", truncate(it.Command, 200))
				}
			case "file_change":
				var paths []string
				for _, c := range it.Changes {
					paths = append(paths, c.Path)
				}
				r.printf("修改文件：%s", strings.Join(paths, ", "))
			case "error":
				r.printf("codex：%s", it.Message)
			}
		}
	}
	return ""
}

func (r *Runner) printf(format string, args ...any) {
	if r.Out != nil {
		fmt.Fprintf(r.Out, "  · "+format+"\n", args...)
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if rs := []rune(s); len(rs) > n {
		return string(rs[:n]) + "…"
	}
	return s
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// ExitCode returns the process exit code, or -1 if unknown.
func (r *Result) ExitCode() int {
	var ee *exec.ExitError
	if errors.As(r.ExitErr, &ee) {
		return ee.ExitCode()
	}
	if r.ExitErr == nil {
		return 0
	}
	return -1
}
