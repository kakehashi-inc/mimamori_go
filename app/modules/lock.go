package modules

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ProcessLock is an exclusive advisory lock on a file that prevents a second
// instance of the same role from starting.
type ProcessLock struct {
	f *os.File
}

// AcquireLock takes the lock or fails immediately when another process holds it.
func AcquireLock(path string) (*ProcessLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another instance is already running (lock %s)", path)
		}
		return nil, err
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return &ProcessLock{f: f}, nil
}

// Release drops the lock.
func (l *ProcessLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
	l.f = nil
}
