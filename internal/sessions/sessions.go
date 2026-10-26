// Package sessions reads Codex's on-disk session state: the rollout logs
// under $CODEX_HOME/sessions, the session index and per-thread writer locks.
package sessions

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codex-lazy-runner/internal/limit"
)

const usageLimitCode = "usage_limit_exceeded"

type Window struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

type RateLimits struct {
	LimitID   string  `json:"limit_id"`
	Primary   *Window `json:"primary"`
	Secondary *Window `json:"secondary"`
}

type Meta struct {
	ID         string          `json:"id"`
	Cwd        string          `json:"cwd"`
	Originator string          `json:"originator"`
	CLIVersion string          `json:"cli_version"`
	Source     json.RawMessage `json:"source"`
}

// IsSubagent reports whether the session was spawned by another agent.
func (m Meta) IsSubagent() bool { return bytes.Contains(m.Source, []byte(`"subagent"`)) }

// TurnError is the error a turn ended with.
type TurnError struct {
	Message string          `json:"message"`
	Info    json.RawMessage `json:"codex_error_info"`
}

// Code returns codex_error_info; for object-shaped infos, its single key.
func (e *TurnError) Code() string {
	var s string
	if json.Unmarshal(e.Info, &s) == nil {
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(e.Info, &m) == nil {
		for k := range m {
			return k
		}
	}
	return ""
}

func (e *TurnError) IsUsageLimit() bool {
	return e != nil && (e.Code() == usageLimitCode || limit.IsUsageLimitMessage(e.Message))
}

// Session is the state of one rollout log.
type Session struct {
	Path    string
	ModTime time.Time
	Meta    Meta
	// LastError is the error of the last finished turn, nil if it succeeded.
	LastError *TurnError
	// TurnOpen is set when the last turn started but never finished.
	TurnOpen bool
	// RateLimits is the latest quota snapshot recorded in the log.
	RateLimits *RateLimits
}

func (s *Session) ID() string {
	if s.Meta.ID != "" {
		return s.Meta.ID
	}
	return idFromPath(s.Path)
}

// UsageLimited reports whether the session stopped on the usage limit.
func (s *Session) UsageLimited() bool { return s.LastError.IsUsageLimit() }

// Only lines containing one of these are decoded; the rest are skipped
// cheaply since rollouts can be tens of megabytes.
var interesting = [][]byte{
	[]byte(`"session_meta"`), []byte(`"task_started"`), []byte(`"task_complete"`),
	[]byte(`"turn_aborted"`), []byte(`"token_count"`), []byte(`"type":"error"`),
}

// Parse reads a rollout log.
func Parse(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	s := &Session{Path: path, ModTime: st.ModTime()}
	fileID := idFromPath(path)
	var turnErr *TurnError // error event seen in the still-open turn

	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && containsAny(line, interesting) {
			s.apply(line, fileID, &turnErr)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if s.TurnOpen && turnErr.IsUsageLimit() {
		s.LastError = turnErr
	}
	return s, nil
}

func (s *Session) apply(line []byte, fileID string, turnErr **TurnError) {
	var env struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &env) != nil {
		return
	}
	switch env.Type {
	case "session_meta":
		var m Meta
		// Forked sessions also embed their parent's meta; keep our own.
		if json.Unmarshal(env.Payload, &m) == nil && (s.Meta.ID == "" || m.ID == fileID) && s.Meta.ID != fileID {
			s.Meta = m
		}
	case "event_msg":
		var p struct {
			Type       string      `json:"type"`
			Error      *TurnError  `json:"error"`
			RateLimits *RateLimits `json:"rate_limits"`
		}
		if json.Unmarshal(env.Payload, &p) != nil {
			return
		}
		switch p.Type {
		case "task_started":
			s.TurnOpen, *turnErr = true, nil
		case "task_complete":
			s.TurnOpen, s.LastError, *turnErr = false, p.Error, nil
		case "turn_aborted":
			s.TurnOpen, s.LastError, *turnErr = false, nil, nil
		case "error":
			var e TurnError
			if json.Unmarshal(env.Payload, &e) == nil {
				*turnErr = &e
			}
		case "token_count":
			if rl := p.RateLimits; rl != nil && (rl.Primary != nil || rl.Secondary != nil) {
				s.RateLimits = rl
			}
		}
	}
}

func containsAny(b []byte, subs [][]byte) bool {
	for _, s := range subs {
		if bytes.Contains(b, s) {
			return true
		}
	}
	return false
}

func idFromPath(p string) string {
	name := strings.TrimSuffix(filepath.Base(p), ".jsonl")
	if len(name) >= 36 {
		return name[len(name)-36:]
	}
	return ""
}

func sessionsDir(codexHome string) string { return filepath.Join(codexHome, "sessions") }

type rolloutFile struct {
	path string
	mod  time.Time
}

// rollouts lists rollout files modified after since, newest first.
func rollouts(codexHome string, since time.Time) []rolloutFile {
	var files []rolloutFile
	_ = filepath.WalkDir(sessionsDir(codexHome), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(since) {
			files = append(files, rolloutFile{p, info.ModTime()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	return files
}

// FindByID returns the rollout path of a session.
func FindByID(codexHome, id string) (string, error) {
	var found string
	suffix := "-" + id + ".jsonl"
	_ = filepath.WalkDir(sessionsDir(codexHome), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), suffix) {
			found = p
			return fs.SkipAll
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("在 %s 中找不到会话 %s 的日志", sessionsDir(codexHome), id)
	}
	return found, nil
}

// Recent parses rollouts modified after since, newest first; unreadable
// files are skipped.
func Recent(codexHome string, since time.Time) []*Session {
	var out []*Session
	for _, f := range rollouts(codexHome, since) {
		if s, err := Parse(f.path); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// LatestUsageLimited returns the newest top-level session that stopped on
// the usage limit.
func LatestUsageLimited(codexHome string, since time.Time) (*Session, error) {
	for _, f := range rollouts(codexHome, since) {
		s, err := Parse(f.path)
		if err == nil && !s.Meta.IsSubagent() && s.UsageLimited() {
			return s, nil
		}
	}
	return nil, errors.New("没有找到因额度中断的会话")
}

// Names maps session IDs to thread names from session_index.jsonl.
func Names(codexHome string) map[string]string {
	names := map[string]string{}
	f, err := os.Open(filepath.Join(codexHome, "session_index.jsonl"))
	if err != nil {
		return names
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var e struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID != "" {
			names[e.ID] = e.ThreadName // later entries are renames
		}
	}
	return names
}

// ResolveID maps an exact thread name to its session ID; other refs are
// returned unchanged.
func ResolveID(codexHome, ref string) string {
	for id, name := range Names(codexHome) {
		if name == ref {
			return id
		}
	}
	return ref
}
