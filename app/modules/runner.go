package modules

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ErrCommandTimeout is returned when an external command exceeds its timeout.
var ErrCommandTimeout = errors.New("command timed out")

// CommandResult is the outcome of RunCommand.
type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// commandSearchPath is used to find the audit tools, which live in sbin
// directories that may be missing from PATH of a service.
var commandSearchPath = []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin", "/usr/local/sbin", "/usr/local/bin"}

// LookupCommand finds an executable by name in PATH or the sbin directories.
func LookupCommand(name string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, dir := range commandSearchPath {
		p := dir + "/" + name
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("command %q not found", name)
}

// RunCommand runs an external command with a timeout WITHOUT creating any
// goroutine: the process writes to unlinked temporary files and is reaped by
// polling wait4 with WNOHANG from the calling flow. On timeout the whole
// process group is killed. maxOutput limits how much of stdout is read.
func RunCommand(timeout time.Duration, maxOutput int64, name string, args ...string) (*CommandResult, error) {
	path, err := LookupCommand(name)
	if err != nil {
		return nil, err
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer devnull.Close()
	outF, err := os.CreateTemp("", "mimamori-out-*")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(outF.Name())
	defer outF.Close()
	errF, err := os.CreateTemp("", "mimamori-err-*")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(errF.Name())
	defer errF.Close()

	env := []string{"LC_ALL=C", "LANG=C", "PATH=/usr/sbin:/sbin:/usr/bin:/bin"}
	proc, err := os.StartProcess(path, append([]string{path}, args...), &os.ProcAttr{
		Files: []*os.File{devnull, outF, errF},
		Env:   env,
		Sys:   &syscall.SysProcAttr{Setpgid: true},
	})
	if err != nil {
		return nil, err
	}
	pid := proc.Pid
	deadline := time.Now().Add(timeout)
	var ws syscall.WaitStatus
	timedOut := false
	sleep := 2 * time.Millisecond
	for {
		wpid, werr := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if werr == syscall.EINTR {
			continue
		}
		if werr != nil {
			_ = proc.Release()
			return nil, werr
		}
		if wpid == pid {
			break
		}
		if time.Now().After(deadline) {
			timedOut = true
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			for {
				_, werr = syscall.Wait4(pid, &ws, 0, nil)
				if werr != syscall.EINTR {
					break
				}
			}
			break
		}
		time.Sleep(sleep)
		if sleep < 50*time.Millisecond {
			sleep *= 2
		}
	}
	_ = proc.Release()

	res := &CommandResult{ExitCode: ws.ExitStatus()}
	if ws.Signaled() {
		res.ExitCode = 128 + int(ws.Signal())
	}
	res.Stdout = readBack(outF, maxOutput)
	res.Stderr = readBack(errF, 64<<10)
	if timedOut {
		return res, fmt.Errorf("%s: %w after %s", name, ErrCommandTimeout, timeout)
	}
	return res, nil
}

func readBack(f *os.File, limit int64) []byte {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(f, limit))
	return b
}

// RunSimple runs a short command with the default timeout and returns an
// error that includes stderr when the exit code is not zero.
func RunSimple(name string, args ...string) (*CommandResult, error) {
	res, err := RunCommand(DefaultCommandTimeoutSec*time.Second, 16<<20, name, args...)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(string(res.Stderr))
		if msg == "" {
			msg = strings.TrimSpace(string(res.Stdout))
		}
		return res, fmt.Errorf("%s %s: exit %d: %s", name, strings.Join(args, " "), res.ExitCode, msg)
	}
	return res, nil
}
