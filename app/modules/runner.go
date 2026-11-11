package modules

import (
	"errors"
	"fmt"
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
// goroutine and without writing any file: the output is read from pipes
// whose reads are bounded by an immediate deadline, and the process is
// reaped by polling wait4 with WNOHANG, all from the calling flow. On timeout
// the whole process group is killed. maxOutput limits how much of stdout is
// kept (the rest is read and discarded so the command never blocks).
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
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer outR.Close()
	errR, errW, err := os.Pipe()
	if err != nil {
		outW.Close()
		return nil, err
	}
	defer errR.Close()

	env := []string{"LC_ALL=C", "LANG=C", "TZ=UTC", "PATH=/usr/sbin:/sbin:/usr/bin:/bin"}
	proc, err := os.StartProcess(path, append([]string{path}, args...), &os.ProcAttr{
		Files: []*os.File{devnull, outW, errW},
		Env:   env,
		Sys:   &syscall.SysProcAttr{Setpgid: true},
	})
	outW.Close()
	errW.Close()
	if err != nil {
		return nil, err
	}
	stdout := &limitedBuffer{limit: maxOutput}
	stderr := &limitedBuffer{limit: 64 << 10}
	pid := proc.Pid
	deadline := time.Now().Add(timeout)
	var ws syscall.WaitStatus
	timedOut := false
	sleep := 2 * time.Millisecond
	for {
		drain(outR, stdout)
		drain(errR, stderr)
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
	// Read what is left after the process ended (until end of file).
	drainAll(outR, stdout)
	drainAll(errR, stderr)

	res := &CommandResult{ExitCode: ws.ExitStatus(), Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if ws.Signaled() {
		res.ExitCode = 128 + int(ws.Signal())
	}
	if timedOut {
		return res, fmt.Errorf("%s: %w after %s", name, ErrCommandTimeout, timeout)
	}
	return res, nil
}

// limitedBuffer keeps at most limit bytes and discards the rest.
type limitedBuffer struct {
	buf   []byte
	limit int64
}

func (b *limitedBuffer) Write(p []byte) {
	if room := b.limit - int64(len(b.buf)); room > 0 {
		if int64(len(p)) > room {
			p = p[:room]
		}
		b.buf = append(b.buf, p...)
	}
}

func (b *limitedBuffer) Bytes() []byte { return b.buf }

// drain reads whatever is available on the pipe now, waiting at most a
// millisecond. (A deadline that has already passed would make the read fail
// before looking at the pipe, so it is set slightly in the future.)
func drain(f *os.File, dst *limitedBuffer) {
	buf := make([]byte, 64<<10)
	for {
		_ = f.SetReadDeadline(time.Now().Add(time.Millisecond))
		n, err := f.Read(buf)
		if n > 0 {
			dst.Write(buf[:n])
		}
		if err != nil || n == 0 {
			return
		}
	}
}

// drainAll reads the pipe until end of file (the writer has exited), bounded
// by a short deadline in case a child of the command still holds it open.
func drainAll(f *os.File, dst *limitedBuffer) {
	buf := make([]byte, 64<<10)
	_ = f.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		n, err := f.Read(buf)
		if n > 0 {
			dst.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
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
