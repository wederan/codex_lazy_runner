// Package store persists lazy-runner tasks in a JSON file guarded by flock,
// and passes wake/cancel requests to running lazy-runner processes through
// small files under wake/.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type Status string

const (
	StatusRunning     Status = "running"
	StatusWaiting     Status = "waiting"
	StatusWeekly      Status = "weekly_blocked"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
	StatusCanceled    Status = "canceled"
	StatusInterrupted Status = "interrupted"
)

// Wake request modes.
const (
	WakeCheck  = "check"  // re-check quota now, resume if usable
	WakeForce  = "force"  // resume now without checking
	WakeCancel = "cancel" // stop the task
)

type Task struct {
	ID             string    `json:"id"`
	SessionID      string    `json:"session_id,omitempty"`
	Cwd            string    `json:"cwd"`
	Prompt         string    `json:"prompt,omitempty"`
	ContinuePrompt string    `json:"continue_prompt"`
	CodexArgs      []string  `json:"codex_args,omitempty"` // flags valid for `codex exec resume`
	Status         Status    `json:"status"`
	Resumes        int       `json:"resumes"`
	Attempts       int       `json:"attempts"`
	ResumeAt       time.Time `json:"resume_at"`
	Note           string    `json:"note,omitempty"`
	PID            int       `json:"pid,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Active reports whether a live lazy-runner process is driving the task.
func (t *Task) Active() bool {
	return (t.Status == StatusRunning || t.Status == StatusWaiting) && ProcessAlive(t.PID)
}

type State struct {
	Tasks []*Task `json:"tasks"`
}

// Find looks a task up by exact ID or session ID, then by unique prefix.
// It returns nil when nothing matches.
func (st *State) Find(ref string) (*Task, error) {
	for _, t := range st.Tasks {
		if t.ID == ref || (t.SessionID != "" && t.SessionID == ref) {
			return t, nil
		}
	}
	var hits []*Task
	for _, t := range st.Tasks {
		if strings.HasPrefix(t.ID, ref) || (t.SessionID != "" && strings.HasPrefix(t.SessionID, ref)) {
			hits = append(hits, t)
		}
	}
	switch len(hits) {
	case 0:
		return nil, nil
	case 1:
		return hits[0], nil
	}
	return nil, fmt.Errorf("%q 匹配到多个任务，请提供更完整的 ID", ref)
}

// Sorted returns tasks, most recently updated first.
func (st *State) Sorted() []*Task {
	out := append([]*Task(nil), st.Tasks...)
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

type Store struct {
	Dir string
}

// Default opens $LAZY_RUNNER_HOME, or ~/.codex-lazy-runner.
func Default() (*Store, error) {
	dir := os.Getenv("LAZY_RUNNER_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".codex-lazy-runner")
	}
	return Open(dir)
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "wake"), 0o755); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) withLock(how int, fn func() error) error {
	f, err := os.OpenFile(filepath.Join(s.Dir, "state.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func (s *Store) read() (*State, error) {
	b, err := os.ReadFile(filepath.Join(s.Dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("解析 state.json 失败: %w", err)
	}
	return &st, nil
}

func (s *Store) write(st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, "state-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.Dir, "state.json"))
}

func (s *Store) Load() (*State, error) {
	var st *State
	err := s.withLock(syscall.LOCK_SH, func() (err error) {
		st, err = s.read()
		return err
	})
	return st, err
}

// Update applies fn to the state under an exclusive lock and saves it.
func (s *Store) Update(fn func(*State) error) error {
	return s.withLock(syscall.LOCK_EX, func() error {
		st, err := s.read()
		if err != nil {
			return err
		}
		if err := fn(st); err != nil {
			return err
		}
		return s.write(st)
	})
}

// Put inserts or replaces a task by ID.
func (s *Store) Put(t *Task) error {
	t.UpdatedAt = time.Now()
	cp := *t
	return s.Update(func(st *State) error {
		for i, x := range st.Tasks {
			if x.ID == cp.ID {
				st.Tasks[i] = &cp
				return nil
			}
		}
		st.Tasks = append(st.Tasks, &cp)
		return nil
	})
}

func (s *Store) wakePath(id string) string { return filepath.Join(s.Dir, "wake", id) }

// RequestWake asks the process driving task id to act; see the Wake* modes.
func (s *Store) RequestWake(id, mode string) error {
	return os.WriteFile(s.wakePath(id), []byte(mode), 0o644)
}

// PeekWake returns a pending wake request without consuming it.
func (s *Store) PeekWake(id string) (string, bool) {
	b, err := os.ReadFile(s.wakePath(id))
	if err != nil {
		return "", false
	}
	if mode := strings.TrimSpace(string(b)); mode != "" {
		return mode, true
	}
	return WakeCheck, true
}

// TakeWake consumes a pending wake request.
func (s *Store) TakeWake(id string) (string, bool) {
	mode, ok := s.PeekWake(id)
	if ok {
		os.Remove(s.wakePath(id))
	}
	return mode, ok
}

func (s *Store) LogDir(id string) string { return filepath.Join(s.Dir, "logs", id) }

func NewID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
