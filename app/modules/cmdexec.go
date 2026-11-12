package modules

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
)

// Execution of one command of a command set on the agent's host.
//
// The command line is handed to a fixed wrapper script run by bash, either
// directly (when the set's OS user is the user the agent runs as) or through
// "sudo -n -H -u USER -- BASH -c WRAPPER ...". The wrapper prints a random
// marker line, enters the working directory, prints the marker again and
// replaces itself with "$BASH -c COMMAND_LINE". The two markers tell the
// three ways a start can end apart: no first marker means the switch to the
// OS user or the shell failed (sudo reports its own failures with exit code
// 1, which could not be told from the command's own 1), a first marker
// without the second means the working directory could not be entered.
//
// Everything runs in the calling flow: the process is polled with wait4 and
// its output pipe is drained in the same loop, which also calls the
// caller's monitoring when it is due.

// commandWrapper is the script bash runs with the arguments marker ($1),
// working directory ($2, empty for the home directory) and command line ($3).
const commandWrapper = `printf '%s\n' "$1"
if [ -n "$2" ]; then cd -- "$2" || exit 1; else cd || exit 1; fi
printf '%s\n' "$1"
exec "$BASH" -c "$3"`

// commandPollMax is the longest pause of the wait loop.
const commandPollMax = 50 * time.Millisecond

// pipeSizeCommand is the pipe buffer asked for (F_SETPIPE_SZ), so that a
// command rarely blocks on its output while the monitoring step runs.
const pipeSizeCommand = 1 << 20

// fSetPipeSz is the fcntl command F_SETPIPE_SZ of Linux.
const fSetPipeSz = 1031

// CommandExecHooks connect the wait loop of a running command to the agent.
// Every field may be nil.
type CommandExecHooks struct {
	// MonitorDue reports whether the monitoring step is due; Monitor runs it.
	// They are called from the wait loop while the command runs.
	MonitorDue func() bool
	Monitor    func()
	// Stopping reports whether the agent is stopping; the command is then
	// ended (SIGTERM, grace, SIGKILL) and its result is an error with the
	// reason interrupted.
	Stopping func() bool
}

// CommandExecInput is one command to run.
type CommandExecInput struct {
	Position int
	OSUser   string
	// WorkDir is the directory the command starts in; empty means the home
	// directory of the OS user.
	WorkDir string
	Command *CommandSpec
	// CollectOutput extracts the values of the command (collect_output).
	CollectOutput bool
	// KillGrace is the time between SIGTERM and SIGKILL (CommandKillGrace
	// when zero).
	KillGrace time.Duration
}

// ExecuteCommand runs one command and returns its result. It never fails:
// whatever goes wrong is an error result with a reason.
func ExecuteCommand(in CommandExecInput, hooks CommandExecHooks) *CommandResultReport {
	c := in.Command
	res := &CommandResultReport{Position: in.Position, CommandLine: c.CommandLine, Note: c.Note, StartedAt: time.Now().Unix()}
	out, raw := runWrapped(in, hooks)
	res.FinishedAt = time.Now().Unix()
	res.Output, res.OutputBytes, res.Truncated = out.output, out.total, out.total > int64(len(out.output))
	switch {
	case raw.interrupted:
		res.Kind, res.Reason = ResultError, ReasonInterrupted
	case raw.timedOut:
		res.Kind, res.Reason = ResultError, ReasonTimeout
	case !out.started:
		res.Kind, res.Reason = ResultError, ReasonStartFailed
	case !out.entered:
		res.Kind, res.Reason = ResultError, ReasonWorkdirFailed
	case raw.signaled:
		res.Kind, res.Reason = ResultError, ReasonSignaled
	default:
		code := raw.exitCode
		res.ExitCode = &code
		res.Kind, res.Reason = c.ClassifyExit(code)
	}
	if raw.exited && out.entered && res.ExitCode == nil {
		// A command that ended by itself after the stop or the time limit
		// still has its exit code.
		code := raw.exitCode
		res.ExitCode = &code
	}
	// The values come from the command's own output; when the command line
	// was never started, the output is the wrapper's (for example sudo's
	// message) and gives no value.
	if in.CollectOutput && out.entered {
		v := ExtractCommandValues(c.Patterns, res.Output, c.LineLimit)
		res.Values = &v
	}
	return res
}

// rawExit is how the process ended.
type rawExit struct {
	exited      bool // it exited by itself (exitCode is valid)
	exitCode    int
	signaled    bool // a signal ended it
	timedOut    bool // the time limit passed
	interrupted bool // the agent was stopping
}

// wrappedOutput is the console output with the marker lines removed.
type wrappedOutput struct {
	started bool // the first marker was seen
	entered bool // the second marker was seen
	output  []byte
	total   int64 // bytes the command (and the wrapper) wrote, without markers
}

// commandArgv returns the program and its arguments for a command: bash
// directly, or bash through sudo.
func commandArgv(osUser, marker, workDir, commandLine string) (string, []string, error) {
	bash, err := LookupCommand("bash")
	if err != nil {
		return "", nil, err
	}
	wrapped := []string{"-c", commandWrapper, AppName, marker, workDir, commandLine}
	if IsAgentUser(osUser) {
		return bash, append([]string{bash}, wrapped...), nil
	}
	sudo, err := LookupCommand("sudo")
	if err != nil {
		// The project's messages do not name the switching program.
		return "", nil, fmt.Errorf("the program that switches to another OS user was not found in PATH")
	}
	return sudo, append([]string{sudo, "-n", "-H", "-u", osUser, "--", bash}, wrapped...), nil
}

// IsAgentUser reports whether an OS user is the user the agent runs as:
// compared by numeric ID when the name resolves, by name otherwise.
func IsAgentUser(osUser string) bool {
	byName, byUID := ReadPasswd()
	uid := os.Geteuid()
	if e, ok := byName[osUser]; ok {
		return e.UID == uid
	}
	if n, err := strconv.Atoi(osUser); err == nil && osUser == strconv.Itoa(n) {
		if _, named := byUID[n]; !named {
			return n == uid
		}
	}
	return osUser == NameOrID(byUID, uid)
}

// runWrapped starts the wrapper and waits for it.
func runWrapped(in CommandExecInput, hooks CommandExecHooks) (wrappedOutput, rawExit) {
	marker, err := RandomHex(16)
	if err != nil {
		return failedStart(err)
	}
	path, argv, err := commandArgv(in.OSUser, marker, in.WorkDir, in.Command.CommandLine)
	if err != nil {
		return failedStart(err)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return failedStart(err)
	}
	defer devnull.Close()
	r, w, err := os.Pipe()
	if err != nil {
		return failedStart(err)
	}
	defer r.Close()
	if sc, err := r.SyscallConn(); err == nil {
		// Best effort: a larger pipe buffer; a failure keeps the default.
		_ = sc.Control(func(fd uintptr) {
			_, _, _ = syscall.Syscall(syscall.SYS_FCNTL, fd, fSetPipeSz, pipeSizeCommand)
		})
	}
	proc, err := os.StartProcess(path, argv, &os.ProcAttr{
		Dir:   "/",
		Files: []*os.File{devnull, w, w},
		Env:   os.Environ(),
		// A new session: its own process group (ended as a whole) and no
		// controlling terminal, so sudo never prompts or allocates one.
		Sys: &syscall.SysProcAttr{Setsid: true},
	})
	w.Close()
	if err != nil {
		return failedStart(err)
	}
	// The raw buffer also holds the two marker lines and a little of what
	// the wrapper may write before them.
	buf := &limitedBuffer{limit: MaxCommandOutputBytes + 2*int64(len(marker)+1) + 4096}
	timeout := time.Duration(in.Command.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = DefaultCommandSetTimeoutSec * time.Second
	}
	grace := in.KillGrace
	if grace <= 0 {
		grace = CommandKillGrace
	}
	raw := waitProcess(proc, r, buf, timeout, grace, hooks)
	drainAll(r, buf)
	return splitMarkers(buf.Bytes(), buf.total, marker), raw
}

// waitProcess polls the process until it ends, draining the pipe, running the
// monitoring when due, and ending the process group on the time limit or a
// stop (SIGTERM, then SIGKILL after the grace).
func waitProcess(proc *os.Process, r *os.File, buf *limitedBuffer, timeout, grace time.Duration, hooks CommandExecHooks) rawExit {
	pid := proc.Pid
	defer func() { _ = proc.Release() }()
	var raw rawExit
	deadline := time.Now().Add(timeout)
	var killAt time.Time
	terminating, killed := false, false
	sleep := 2 * time.Millisecond
	var ws syscall.WaitStatus
	for {
		drain(r, buf)
		wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			// The process cannot be waited for (should not happen): end it.
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			raw.signaled = true
			return raw
		}
		if wpid == pid {
			break
		}
		now := time.Now()
		stopping := hooks.Stopping != nil && hooks.Stopping()
		if !terminating {
			// A stop during the grace of a time limit keeps the reason (the
			// time limit) and the grace.
			switch {
			case stopping:
				raw.interrupted = true
			case now.After(deadline):
				raw.timedOut = true
			}
			if raw.interrupted || raw.timedOut {
				terminating = true
				killAt = now.Add(grace)
				_ = syscall.Kill(-pid, syscall.SIGTERM)
			}
		}
		if terminating && !killed && now.After(killAt) {
			killed = true
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		if !stopping && hooks.MonitorDue != nil && hooks.Monitor != nil && hooks.MonitorDue() {
			drain(r, buf)
			hooks.Monitor()
			drain(r, buf)
			continue
		}
		time.Sleep(sleep)
		if sleep < commandPollMax {
			sleep *= 2
		}
	}
	switch {
	case ws.Exited():
		raw.exited, raw.exitCode = true, ws.ExitStatus()
	case ws.Signaled():
		raw.signaled = true
	}
	if terminating && !killed {
		// The command ended within the grace: make sure nothing of its group
		// is left behind.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	return raw
}

// failedStart is the result of a command whose process could not be started:
// the reason is kept as its output.
func failedStart(err error) (wrappedOutput, rawExit) {
	msg := []byte(fmt.Sprintf("%s: the command could not be started: %v\n", AppName, err))
	return wrappedOutput{output: msg, total: int64(len(msg))}, rawExit{}
}

// splitMarkers removes the two marker lines from the raw output. Text before
// the first marker or between the markers (such as a message of cd) is kept
// as output. total is the number of raw bytes written.
func splitMarkers(raw []byte, total int64, marker string) wrappedOutput {
	line := []byte(marker + "\n")
	var out wrappedOutput
	rest := raw
	var kept []byte
	removed := 0
	if i := findLine(rest, line); i >= 0 {
		out.started = true
		kept = append(kept, rest[:i]...)
		rest = rest[i+len(line):]
		removed += len(line)
		if j := findLine(rest, line); j >= 0 {
			out.entered = true
			kept = append(kept, rest[:j]...)
			rest = rest[j+len(line):]
			removed += len(line)
		}
	}
	kept = append(kept, rest...)
	out.total = total - int64(removed)
	if len(kept) > MaxCommandOutputBytes {
		kept = kept[:MaxCommandOutputBytes]
	}
	out.output = kept
	return out
}

// findLine returns the index of line in b where it starts a line (at 0 or
// after a line break), or -1.
func findLine(b, line []byte) int {
	off := 0
	for {
		i := bytes.Index(b[off:], line)
		if i < 0 {
			return -1
		}
		i += off
		if i == 0 || b[i-1] == '\n' {
			return i
		}
		off = i + 1
	}
}
