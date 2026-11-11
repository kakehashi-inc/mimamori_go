package modules

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// LockHolder reports whether a process holds the lock file at path and, if
// so, its process id. It never modifies the file.
func LockHolder(path string) (pid int, running bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return 0, false, nil
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		return 0, false, err
	}
	b := make([]byte, 32)
	n, _ := f.Read(b)
	pid, err = strconv.Atoi(strings.TrimSpace(string(b[:n])))
	if err != nil || pid <= 0 {
		return 0, true, fmt.Errorf("lock %s is held but holds no process id", path)
	}
	return pid, true, nil
}

// StopProcess sends SIGTERM to the process holding the lock and waits until
// the lock is released (the process completes its current processing and
// saves its state first), at most timeout. It returns false when no process
// was running.
func StopProcess(lockPath string, timeout time.Duration) (bool, error) {
	pid, running, err := LockHolder(lockPath)
	if err != nil {
		return false, err
	}
	if !running {
		return false, nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.EPERM) {
			return true, NewExitErrorf(ExitAuth, "not permitted to signal process %d (run as the user running it)", pid)
		}
		return true, fmt.Errorf("sending SIGTERM to process %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if _, still, err := LockHolder(lockPath); err == nil && !still {
			return true, nil
		}
	}
	return true, NewExitErrorf(ExitTimeout, "process %d did not stop within %s", pid, timeout)
}
