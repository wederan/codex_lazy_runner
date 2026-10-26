// fakecodex imitates the parts of the codex CLI that lazy-runner uses.
// It is driven by files in $FAKE_CODEX_DIR:
//
//	script        one outcome per exec call: ok | limit | error | nostart | hang
//	account.json  result of account/rateLimits/read (missing = RPC error)
//	calls.jsonl   written: argv of every exec call
//	consumed      written: one line per redeemed reset credit
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

const newThreadID = "0199aaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

var dir = os.Getenv("FAKE_CODEX_DIR")

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "app-server":
		appServer()
	case "exec":
		execCmd(os.Args[1:])
	default:
		os.Exit(2)
	}
}

func appServer() {
	sc := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		switch req.Method {
		case "initialize":
			out.Encode(map[string]any{"id": *req.ID, "result": map[string]any{}})
		case "account/rateLimits/read":
			b, err := os.ReadFile(filepath.Join(dir, "account.json"))
			if err != nil {
				out.Encode(map[string]any{"id": *req.ID, "error": map[string]any{"code": -1, "message": "not logged in"}})
				continue
			}
			out.Encode(map[string]any{"id": *req.ID, "result": json.RawMessage(b)})
		case "account/rateLimitResetCredit/consume":
			f, _ := os.OpenFile(filepath.Join(dir, "consumed"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			fmt.Fprintln(f, string(req.Params))
			f.Close()
			os.WriteFile(filepath.Join(dir, "account.json"), []byte(`{"ordinaryUsageAllowed":true,"rateLimits":{"primary":{"usedPercent":0,"windowDurationMins":300,"resetsAt":0}},"rateLimitResetCredits":{"availableCount":0,"credits":[]}}`), 0o644)
			out.Encode(map[string]any{"id": *req.ID, "result": map[string]any{"outcome": "reset"}})
		}
	}
}

func execCmd(args []string) {
	b, _ := json.Marshal(args)
	f, _ := os.OpenFile(filepath.Join(dir, "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	f.Write(append(b, '\n'))
	f.Close()
	calls, _ := os.ReadFile(filepath.Join(dir, "calls.jsonl"))
	n := strings.Count(string(calls), "\n")
	script, _ := os.ReadFile(filepath.Join(dir, "script"))
	steps := strings.Fields(string(script))
	outcome := "ok"
	if len(steps) > 0 {
		outcome = steps[min(n, len(steps))-1]
	}

	threadID := newThreadID
	if len(args) > 1 && args[1] == "resume" {
		for i, a := range args {
			if a == "--" && i+1 < len(args) {
				threadID = args[i+1]
			}
		}
	}
	emit := func(v map[string]any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	if outcome == "nostart" {
		fmt.Fprintln(os.Stderr, "Not inside a trusted directory and --skip-git-repo-check was not specified.")
		os.Exit(1)
	}
	emit(map[string]any{"type": "thread.started", "thread_id": threadID})
	emit(map[string]any{"type": "turn.started"})
	switch outcome {
	case "ok":
		emit(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_0", "type": "agent_message", "text": "done"}})
		emit(map[string]any{"type": "turn.completed", "usage": map[string]any{}})
	case "limit":
		msg := "You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 3:07 PM."
		emit(map[string]any{"type": "error", "message": msg})
		emit(map[string]any{"type": "turn.failed", "error": map[string]any{"message": msg}})
		os.Exit(1)
	case "error":
		emit(map[string]any{"type": "turn.failed", "error": map[string]any{"message": "stream disconnected before completion"}})
		os.Exit(1)
	case "hang":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		os.Exit(130)
	}
}
