package modules

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrToolStopped is returned when a tool was ended, or not started, because
// the agent stops.
var ErrToolStopped = errors.New("ended because the agent stops")

// toolKillGrace is the time between SIGTERM and SIGKILL when a stop ends a
// tool (replaced by tests).
var toolKillGrace = CommandKillGrace

// CommandResult is the outcome of RunTool.
type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// LookupCommand finds an executable by name in the directories of PATH. A
// name containing "/" is used as given.
func LookupCommand(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", M("common.command_not_in_path", "name", strconv.Quote(name))
	}
	return p, nil
}

// commandEnv is the environment of the commands whose output is parsed: the
// environment of this process with the locale and the time zone fixed, so
// the output format and the times given to the command (ausearch -ts) do
// not depend on the user's settings.
func commandEnv() []string {
	env := []string{}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "LC_ALL=") || strings.HasPrefix(kv, "TZ=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "LC_ALL=C", "TZ=UTC")
}

// RunTool runs an external program and waits until it ends. Nothing limits
// its running time. When stop is not nil and is closed while the program
// runs, its process group gets SIGTERM, after toolKillGrace SIGKILL, and
// RunTool waits until it has ended and returns ErrToolStopped (with what it
// wrote). When stop is already closed, the program is not started.
//
// Nothing is written to a file: the output is read from pipes whose reads are
// bounded by an immediate deadline, and the process is reaped by polling
// wait4 with WNOHANG, all from the calling flow. maxOutput bounds what is
// kept of standard output (the rest is read and dropped, so the program
// never blocks on a full pipe); standard error is kept up to 64 KiB.
func RunTool(stop <-chan struct{}, maxOutput int64, name string, args ...string) (*CommandResult, error) {
	if isClosed(stop) {
		return nil, fmt.Errorf("%s: %w", name, ErrToolStopped)
	}
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

	proc, err := os.StartProcess(path, append([]string{path}, args...), &os.ProcAttr{
		Files: []*os.File{devnull, outW, errW},
		Env:   commandEnv(),
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
	var ws syscall.WaitStatus
	stopped, killed := false, false
	var killAt time.Time
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
		now := time.Now()
		if !stopped && isClosed(stop) {
			stopped = true
			killAt = now.Add(toolKillGrace)
			_ = syscall.Kill(-pid, syscall.SIGTERM)
		}
		if stopped && !killed && !now.Before(killAt) {
			killed = true
			_ = syscall.Kill(-pid, syscall.SIGKILL)
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
	if stopped {
		return res, fmt.Errorf("%s: %w", name, ErrToolStopped)
	}
	return res, nil
}

// limitedBuffer keeps at most limit bytes and discards the rest; total counts
// every byte written.
type limitedBuffer struct {
	buf   []byte
	limit int64
	total int64
}

func (b *limitedBuffer) Write(p []byte) {
	b.total += int64(len(p))
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

// RunSimple runs a tool through RunTool with 16 MiB of output and returns an
// error that includes stderr when the exit code is not zero.
func RunSimple(stop <-chan struct{}, name string, args ...string) (*CommandResult, error) {
	res, err := RunTool(stop, 16<<20, name, args...)
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
