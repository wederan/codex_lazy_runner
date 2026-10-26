// Package appserver queries account quota through the stdio JSON-RPC
// interface of `codex app-server`, which costs no tokens.
package appserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Window is one rate-limit window as reported by account/rateLimits/read.
type Window struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins int     `json:"windowDurationMins"`
	ResetsAt           int64   `json:"resetsAt"`
}

func (w *Window) ResetTime() time.Time { return time.Unix(w.ResetsAt, 0) }

type RateLimits struct {
	LimitID              string  `json:"limitId"`
	Primary              *Window `json:"primary"`
	Secondary            *Window `json:"secondary"`
	PlanType             string  `json:"planType"`
	RateLimitReachedType string  `json:"rateLimitReachedType"`
}

// Credit is a rate-limit reset credit ("额度卡").
type Credit struct {
	ID          string `json:"id"`
	ResetType   string `json:"resetType"`
	Status      string `json:"status"`
	Title       string `json:"title"`
	Description string `json:"description"`
	GrantedAt   int64  `json:"grantedAt"`
	ExpiresAt   int64  `json:"expiresAt"`
}

type ResetCredits struct {
	AvailableCount int      `json:"availableCount"`
	Credits        []Credit `json:"credits"`
}

type Account struct {
	OrdinaryUsageAllowed  *bool         `json:"ordinaryUsageAllowed"`
	RateLimits            *RateLimits   `json:"rateLimits"`
	RateLimitResetCredits *ResetCredits `json:"rateLimitResetCredits"`
}

// Outcomes of account/rateLimitResetCredit/consume.
const (
	OutcomeReset           = "reset"
	OutcomeNothingToReset  = "nothingToReset"
	OutcomeNoCredit        = "noCredit"
	OutcomeAlreadyRedeemed = "alreadyRedeemed"
)

type Client struct {
	Bin     string
	Timeout time.Duration
}

// ReadAccount returns the current rate limits and reset credits. Without
// creditDetails only the credit count is fetched (meant for background
// polls); even with it the backend sometimes omits the details.
func (c *Client) ReadAccount(ctx context.Context, creditDetails bool) (*Account, error) {
	var acc Account
	params := map[string]any{"excludeResetCreditDetails": !creditDetails}
	if err := c.call(ctx, "account/rateLimits/read", params, &acc); err != nil {
		return nil, err
	}
	return &acc, nil
}

// ConsumeResetCredit redeems a reset credit; an empty creditID lets the
// backend pick the next available one. Reuse idempotencyKey when retrying.
func (c *Client) ConsumeResetCredit(ctx context.Context, creditID, idempotencyKey string) (string, error) {
	params := map[string]any{"idempotencyKey": idempotencyKey}
	if creditID != "" {
		params["creditId"] = creditID
	}
	var resp struct {
		Outcome string `json:"outcome"`
	}
	if err := c.call(ctx, "account/rateLimitResetCredit/consume", params, &resp); err != nil {
		return "", err
	}
	return resp.Outcome, nil
}

// NewIdempotencyKey returns a random UUIDv4.
func NewIdempotencyKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type rpcMessage struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// call starts a short-lived app-server, performs the initialize handshake,
// sends one request and returns its result.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Bin, "app-server")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 codex app-server 失败: %w", err)
	}
	defer func() {
		stdin.Close() // app-server exits on stdin EOF
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}()

	lines := make(chan []byte)
	go func() {
		defer close(lines)
		br := bufio.NewReader(stdout)
		for {
			line, err := br.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	enc := json.NewEncoder(stdin)
	send := func(msg map[string]any) error {
		msg["jsonrpc"] = "2.0"
		return enc.Encode(msg)
	}
	await := func(id int64) (json.RawMessage, error) {
		for {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("等待 codex app-server 响应超时: %w", ctx.Err())
			case line, ok := <-lines:
				if !ok {
					return nil, fmt.Errorf("codex app-server 意外退出: %s", stderr.lastLine())
				}
				var m rpcMessage
				if json.Unmarshal(line, &m) != nil || m.ID == nil || *m.ID != id || m.Method != "" {
					continue
				}
				if m.Error != nil {
					return nil, fmt.Errorf("codex app-server 返回错误: %s", m.Error.Message)
				}
				return m.Result, nil
			}
		}
	}

	initParams := map[string]any{"clientInfo": map[string]any{"name": "codex-lazy-runner", "version": "0.1.0"}}
	if err := send(map[string]any{"id": 1, "method": "initialize", "params": initParams}); err != nil {
		return err
	}
	if _, err := await(1); err != nil {
		return err
	}
	if err := send(map[string]any{"method": "initialized"}); err != nil {
		return err
	}
	if err := send(map[string]any{"id": 2, "method": method, "params": params}); err != nil {
		return err
	}
	raw, err := await(2)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return json.Unmarshal(raw, out)
}

// lockedBuffer collects stderr while the process runs.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() < 64<<10 {
		b.buf.Write(p)
	}
	return len(p), nil
}

func (b *lockedBuffer) lastLine() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(b.buf.String()), "\n")
	return lines[len(lines)-1]
}
