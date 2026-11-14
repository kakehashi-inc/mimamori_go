package modules

import (
	"bytes"
	"errors"
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
// A command runs on the calling flow (the agent's worker that runs the set):
// the process is polled with wait4 and its output pipe is drained in the
// same loop, which also lets the caller run its monitoring step.

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

// signalHelperScript is the script the helper's bash runs with the process
// group ID as $1 and the signal name as $2.
const signalHelperScript = `kill -s "$2" -- -"$1"`

// CommandExecHooks connect the wait loop of a running command to the agent.
// Every field may be nil.
type CommandExecHooks struct {
	// MonitorHere is called from the wait loop on every pass while the agent
	// is not stopping. It runs the monitoring step on this flow when the
	// agent's scheduler gives it to this worker, and returns after it.
	MonitorHere func()
	// Stop is closed when the agent stops; the command is then ended
	// (SIGTERM, grace, SIGKILL) and its result is an error with the reason
	// interrupted. A nil channel never stops the command.
	Stop <-chan struct{}
	// Logf writes to the agent's log.
	Logf func(format string, a ...any)
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
// directly, or bash through sudo (viaSudo).
func commandArgv(osUser, marker, workDir, commandLine string) (path string, argv []string, viaSudo bool, err error) {
	bash, err := LookupCommand("bash")
	if err != nil {
		return "", nil, false, err
	}
	if IsAgentUser(osUser) {
		if workDir == "" {
			// The direct start keeps the agent's environment, which may lack
			// HOME (a service started by a supervisor): the home directory
			// comes from /etc/passwd. Without an entry the plain cd is kept.
			workDir = agentHomeDir()
		}
		wrapped := []string{"-c", commandWrapper, AppName, marker, workDir, commandLine}
		return bash, append([]string{bash}, wrapped...), false, nil
	}
	wrapped := []string{"-c", commandWrapper, AppName, marker, workDir, commandLine}
	sudo, err := LookupCommand("sudo")
	if err != nil {
		return "", nil, false, errNoSwitchProgram
	}
	return sudo, append([]string{sudo, "-n", "-H", "-u", osUser, "--", bash}, wrapped...), true, nil
}

// errNoSwitchProgram: the project's messages do not name the switching
// program.
var errNoSwitchProgram = errors.New("the program that switches to another OS user was not found in PATH")

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

// agentHomeDir returns the home directory of the user the agent runs as from
// /etc/passwd, or "" when the user has no entry or the entry no home.
func agentHomeDir() string {
	byName, byUID := ReadPasswd()
	uid := os.Geteuid()
	name, ok := byUID[uid]
	if !ok {
		return ""
	}
	if e, ok := byName[name]; ok && e.UID == uid {
		return e.Home
	}
	return ""
}

// runWrapped starts the wrapper and waits for it.
func runWrapped(in CommandExecInput, hooks CommandExecHooks) (wrappedOutput, rawExit) {
	marker, err := RandomHex(16)
	if err != nil {
		return failedStart(err)
	}
	path, argv, viaSudo, err := commandArgv(in.OSUser, marker, in.WorkDir, in.Command.CommandLine)
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
		// The error names the program's path; the output does not.
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
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
	sig := &groupSignaler{pgid: proc.Pid, logf: hooks.Logf}
	if viaSudo {
		sig.osUser = in.OSUser
	}
	raw := waitProcess(proc, r, buf, timeout, grace, hooks, sig)
	drainAll(pipeOutput{r, buf})
	return splitMarkers(buf.Bytes(), buf.total, marker), raw
}

// waitProcess polls the process until it ends, draining the pipe, letting the
// caller run its monitoring step, and ending the process group on the time limit or a
// stop (SIGTERM, then SIGKILL after the grace). When the group could not be
// signalled (a command of another OS user that the helper could not end
// either) and the agent is stopping, it stops waiting one grace after the
// SIGKILL, so that the agent's stop never hangs on such a command.
func waitProcess(proc *os.Process, r *os.File, buf *limitedBuffer, timeout, grace time.Duration, hooks CommandExecHooks,
	sig *groupSignaler) rawExit {
	drainPipe := func() { drain(r, buf) }
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
			sig.send(syscall.SIGKILL, drainPipe)
			raw.signaled = true
			return raw
		}
		if wpid == pid {
			break
		}
		now := time.Now()
		stopping := isClosed(hooks.Stop)
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
				sig.send(syscall.SIGTERM, drainPipe)
			}
		}
		if terminating && !killed && now.After(killAt) {
			killed = true
			sig.send(syscall.SIGKILL, drainPipe)
		}
		if killed && stopping && sig.refused && now.After(killAt.Add(grace)) {
			sig.logfSafe("a command run as OS user %q could not be ended; no longer waiting for it", sig.osUser)
			return raw
		}
		if !stopping && hooks.MonitorHere != nil {
			hooks.MonitorHere()
			drain(r, buf)
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
		sig.send(syscall.SIGKILL, drainPipe)
	}
	return raw
}

// isClosed reports whether the channel is closed (a nil channel never is).
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// groupSignaler sends signals to the process group of a command. A command
// started through sudo belongs to another OS user; an agent that does not run
// as root may not signal it (sudo itself runs as root and the command's
// processes belong to that user: EPERM). The signal is then sent through the
// switch program as that user, with the same sudoers rule that started the
// command.
type groupSignaler struct {
	pgid int
	// osUser is set when the command was started through sudo.
	osUser string
	logf   func(format string, a ...any)
	// refused records that a signal to the group was refused and could not
	// be sent through the helper either.
	refused bool
}

func (g *groupSignaler) logfSafe(format string, a ...any) {
	if g.logf != nil {
		g.logf(format, a...)
	}
}

// send sends sig to the process group. drainPipe is called while the helper
// is waited for.
func (g *groupSignaler) send(sig syscall.Signal, drainPipe func()) {
	err := killProcessGroup(-g.pgid, sig)
	if err == nil || !errors.Is(err, syscall.EPERM) || g.osUser == "" {
		return
	}
	if err := signalGroupAsUser(g.osUser, g.pgid, sig, drainPipe); err != nil {
		g.refused = true
		g.logfSafe("ending a command run as OS user %q failed: %v", g.osUser, err)
	}
}

// killProcessGroup and signalGroupAsUser can be replaced by tests (an agent
// that runs as root is never refused).
var (
	killProcessGroup  = syscall.Kill
	signalGroupAsUser = signalGroupAs
)

// signalGroupAs runs "sudo -n -u USER -- BASH -c 'kill -s SIG -- -PGID'" and
// waits in the calling flow until it ends, without a time limit (it is an
// external tool), draining the command's pipe meanwhile.
func signalGroupAs(osUser string, pgid int, sig syscall.Signal, drainPipe func()) error {
	name := "KILL"
	if sig == syscall.SIGTERM {
		name = "TERM"
	}
	bash, err := LookupCommand("bash")
	if err != nil {
		return err
	}
	sudo, err := LookupCommand("sudo")
	if err != nil {
		return errNoSwitchProgram
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	argv := []string{sudo, "-n", "-u", osUser, "--", bash, "-c", signalHelperScript, AppName, strconv.Itoa(pgid), name}
	proc, err := os.StartProcess(sudo, argv, &os.ProcAttr{
		Dir:   "/",
		Files: []*os.File{devnull, devnull, devnull},
		Env:   os.Environ(),
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		// The error names the program's path; the messages do not.
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return fmt.Errorf("the helper could not be started: %v", err)
	}
	defer func() { _ = proc.Release() }()
	var ws syscall.WaitStatus
	for {
		wpid, err := syscall.Wait4(proc.Pid, &ws, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if wpid == proc.Pid {
			break
		}
		drainPipe()
		time.Sleep(10 * time.Millisecond)
	}
	if !ws.Exited() || ws.ExitStatus() != 0 {
		return fmt.Errorf("the signal could not be sent (status %d)", ws.ExitStatus())
	}
	return nil
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
