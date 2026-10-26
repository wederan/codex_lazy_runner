package sessions

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func lockPath(codexHome, id string) string {
	return filepath.Join(codexHome, "thread-writer-locks", id+".lock")
}

// LockHeld reports whether another process holds the session's writer
// lock, i.e. it is open in Codex Desktop, a TUI or another exec.
func LockHeld(codexHome, id string) (bool, error) {
	f, err := os.Open(lockPath(codexHome, id))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	fd := int(f.Fd())
	err = syscall.Flock(fd, syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		return false, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

// LockHolder describes the process holding the session lock (best effort).
func LockHolder(codexHome, id string) string {
	out, err := exec.Command("lsof", "-t", lockPath(codexHome, id)).Output()
	if err != nil {
		return "未知进程"
	}
	pid := strings.Fields(string(out))
	if len(pid) == 0 {
		return "未知进程"
	}
	comm, _ := exec.Command("ps", "-o", "comm=", "-p", pid[0]).Output()
	name := strings.TrimSpace(string(comm))
	if i := strings.Index(name, ".app/"); i >= 0 {
		name = filepath.Base(name[:i+4])
	}
	return fmt.Sprintf("PID %s %s", pid[0], name)
}
