package modules

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// ProcessLock is an exclusive advisory lock on a file that prevents a second
// instance of the same role from starting.
type ProcessLock struct {
	f *os.File
}

// lockAttempts and lockRetryPause bound how long AcquireLock waits for a
// lock that is busy: "status" and "stop" hold a shared lock on the file for
// a moment while they check it, which must not make a start fail.
const (
	lockAttempts   = 5
	lockRetryPause = 100 * time.Millisecond
)

// AcquireLock takes the lock or fails when another process still holds it
// after a few short retries.
func AcquireLock(path string) (*ProcessLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if attempt >= lockAttempts {
			f.Close()
			return nil, fmt.Errorf("another instance is already running (lock %s)", path)
		}
		time.Sleep(lockRetryPause)
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
