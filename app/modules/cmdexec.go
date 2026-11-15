package modules

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
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
// A command runs on the calling flow (the agent's worker that runs the set)
// and nothing else runs there meanwhile: the main process is polled with
// wait4 and the output pipe is drained in the same loop; after the main
// process ended, the pipe is read on while the processes it left behind
// write. The time limit and the stop end the whole process group: SIGTERM,
// then, when something of the group is still there after the grace,
// SIGKILL to what is left.

// commandWrapper is the script bash runs with the arguments marker ($1),
// working directory ($2, empty for the home directory) and command line ($3).
const commandWrapper = `printf '%s\n' "$1"
if [ -n "$2" ]; then cd -- "$2" || exit 1; else cd || exit 1; fi
printf '%s\n' "$1"
exec "$BASH" -c "$3"`

// commandPollMax is the longest pause of the wait loop.
const commandPollMax = 50 * time.Millisecond

// pipeSizeCommand is the pipe buffer asked for (F_SETPIPE_SZ), so that a
// command rarely blocks on its output between two reads.
const pipeSizeCommand = 1 << 20

// fSetPipeSz is the fcntl command F_SETPIPE_SZ of Linux.
const fSetPipeSz = 1031

// signalHelperScript is the script the helper's bash runs with the process
// group ID as $1 and the signal name as $2.
const signalHelperScript = `kill -s "$2" -- -"$1"`

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
	// Stop is closed when the agent stops; the process group of the command
	// is then ended (SIGTERM, grace, SIGKILL to what is left). A command
	// whose main process was still running gets the result error with the
	// reason interrupted. A nil channel never stops the command.
	Stop <-chan struct{}
	// Logf writes to the agent's log (may be nil).
	Logf func(format string, a ...any)
	// Started, when not nil, is called with the process group of the
	// command once its process runs (not for a command that could not be
	// started).
	Started func(pgid int)
	// Leftover, when not nil, takes the output pipe when the reading ended
	// while processes the command left behind still hold it open (they
	// may write on; whoever holds the pipe reads and drops it). Otherwise
	// the pipe is closed.
	Leftover func(f *os.File)
}

// ExecuteCommand runs one command and returns its result. It never fails:
// whatever goes wrong is an error result with a reason.
func ExecuteCommand(in CommandExecInput) *CommandResultReport {
	p := startCommand(in)
	p.wait()
	return p.result(in.CollectOutput)
}

// rawExit is how the main process ended.
type rawExit struct {
	exited      bool // it exited by itself (exitCode is valid)
	exitCode    int
	signaled    bool // a signal ended it
	timedOut    bool // the time limit passed while it ran
	interrupted bool // the agent stopped while it ran
}

// wrappedOutput is the console output with the marker lines removed.
type wrappedOutput struct {
	started bool // the first marker was seen
	entered bool // the second marker was seen
	output  []byte
	total   int64 // bytes the command (and the wrapper) wrote, without markers
}

// commandProcess is one command from its start to the end of the reading of
// its output. Its fields are the state of the wait, so that wait can be
// called again after it was left by a panic and goes on where it was.
type commandProcess struct {
	in      CommandExecInput
	startAt time.Time
	endAt   time.Time
	// failed is the output of a command whose process could not be
	// started (nothing else is set then).
	failed *wrappedOutput
	marker string
	proc   *os.Process
	r      *os.File
	buf    *limitedBuffer
	// deadline is when the time limit passes (zero: no limit); grace the
	// time between SIGTERM and SIGKILL.
	deadline time.Time
	grace    time.Duration
	sig      *groupSignaler
	// group tells whether the process group still has a process that has
	// not ended.
	group *groupProbe

	raw rawExit
	// reaped: the main process was waited for. terminating: SIGTERM went to
	// the group (at killAt the rest gets SIGKILL; killed when that time
	// came). done: the reading ended and the pipe was handed over or closed.
	reaped      bool
	terminating bool
	killAt      time.Time
	killed      bool
	done        bool
}

// startCommand starts the wrapper of a command. A command that cannot be
// started is returned as ended, with the reason as its output.
func startCommand(in CommandExecInput) *commandProcess {
	p := &commandProcess{in: in, startAt: time.Now()}
	marker, err := RandomHex(16)
	if err != nil {
		return p.failStart(err)
	}
	path, argv, viaSudo, err := commandArgv(in.OSUser, marker, in.WorkDir, in.Command.CommandLine)
	if err != nil {
		return p.failStart(err)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return p.failStart(err)
	}
	defer devnull.Close()
	r, w, err := os.Pipe()
	if err != nil {
		return p.failStart(err)
	}
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
		r.Close()
		// The error names the program's path; the output does not.
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return p.failStart(err)
	}
	p.marker, p.proc, p.r = marker, proc, r
	// The raw buffer also holds the two marker lines and a little of what
	// the wrapper may write before them.
	p.buf = &limitedBuffer{limit: MaxCommandOutputBytes + 2*int64(len(marker)+1) + 4096}
	p.deadline = commandDeadline(p.startAt, in.Command.TimeoutSec)
	p.grace = in.KillGrace
	if p.grace <= 0 {
		p.grace = CommandKillGrace
	}
	p.sig = &groupSignaler{pgid: proc.Pid, logf: in.Logf}
	// The main process leads the group: it is the member looked at first.
	p.group = &groupProbe{pgid: proc.Pid, member: proc.Pid}
	if viaSudo {
		p.sig.osUser = in.OSUser
	}
	if in.Started != nil {
		in.Started(proc.Pid)
	}
	return p
}

// failStart ends p as a command whose process could not be started.
func (p *commandProcess) failStart(err error) *commandProcess {
	msg := []byte(fmt.Sprintf("%s: the command could not be started: %v\n", AppName, err))
	p.failed = &wrappedOutput{output: msg, total: int64(len(msg))}
	p.reaped, p.done, p.endAt = true, true, time.Now()
	return p
}

// commandDeadline returns when the time limit of a command started at start
// passes: timeoutSec seconds later, or the zero time (no limit) when
// timeoutSec is 0.
func commandDeadline(start time.Time, timeoutSec int) time.Time {
	if timeoutSec <= 0 {
		return time.Time{}
	}
	return start.Add(time.Duration(timeoutSec) * time.Second)
}

// wait waits until the main process ended and the reading of the output
// ended (see waitMain and readAfterEnd). It goes on where it was when it
// is called again.
func (p *commandProcess) wait() {
	if !p.reaped {
		p.waitMain()
	}
	if !p.done {
		p.readAfterEnd()
	}
}

// stopping reports whether the agent stops.
func (p *commandProcess) stopping() bool {
	return isClosed(p.in.Stop)
}

// limitPassed reports whether the time limit has passed at now.
func (p *commandProcess) limitPassed(now time.Time) bool {
	return !p.deadline.IsZero() && now.After(p.deadline)
}

// terminate sends SIGTERM to the process group and starts the grace.
func (p *commandProcess) terminate(now time.Time) {
	p.terminating = true
	p.killAt = now.Add(p.grace)
	p.sig.send(syscall.SIGTERM, p.drainPipe)
}

// killRest sends SIGKILL to what is left of the group when the grace is
// over: nothing is sent when the group has no process that has not ended
// (groupProbe).
func (p *commandProcess) killRest() {
	p.killed = true
	if p.group.exists() {
		p.sig.send(syscall.SIGKILL, p.drainPipe)
	}
}

func (p *commandProcess) drainPipe() { drain(p.r, p.buf) }

// waitMain polls the main process until it ends, draining the pipe. A stop,
// or the time limit (a zero deadline never passes), ends the process group:
// SIGTERM, and SIGKILL to what is left of the group when the grace is over.
// When the group could not be signalled (a command of another OS user that
// the helper could not end either) and the agent is stopping, it stops
// waiting one grace after the SIGKILL, so that the agent's stop never hangs
// on such a command; the reading of the output ends with it.
func (p *commandProcess) waitMain() {
	pid := p.proc.Pid
	sleep := 2 * time.Millisecond
	var ws syscall.WaitStatus
	for {
		p.drainPipe()
		wpid, err := wait4(pid, &ws, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			// The process cannot be waited for (it is not a child any
			// more; should not happen): it counts as ended by a signal.
			p.raw.signaled = true
			break
		}
		if wpid == pid {
			switch {
			case ws.Exited():
				p.raw.exited, p.raw.exitCode = true, ws.ExitStatus()
			case ws.Signaled():
				p.raw.signaled = true
			}
			break
		}
		now := time.Now()
		if !p.terminating {
			// A stop during the grace of a time limit keeps the reason (the
			// time limit) and the grace.
			switch {
			case p.stopping():
				p.raw.interrupted = true
			case p.limitPassed(now):
				p.raw.timedOut = true
			}
			if p.raw.interrupted || p.raw.timedOut {
				p.terminate(now)
			}
		}
		if p.terminating && !p.killed && now.After(p.killAt) {
			p.killRest()
		}
		if p.killed && p.stopping() && p.sig.refused && now.After(p.killAt.Add(p.grace)) {
			p.sig.logfSafe("a command run as OS user %q could not be ended; no longer waiting for it", p.sig.osUser)
			_ = p.proc.Release()
			p.reaped = true
			p.finish(false)
			return
		}
		time.Sleep(sleep)
		if sleep < commandPollMax {
			sleep *= 2
		}
	}
	_ = p.proc.Release()
	p.reaped = true
}

// readAfterEnd reads the output after the main process ended, while the
// processes it left behind still write: until end of file (nothing holds
// the pipe open any more) or pipeDrainSilence without output, counted anew
// after every read that returned data, so output that keeps arriving is
// read whatever it takes. The stop and the time limit apply as during the
// run: SIGTERM to the process group (when the main process did not get it
// already), and the reading ends when the group is gone or, after the grace,
// when what is left got SIGKILL. The result of the command stays the one of
// its main process.
func (p *commandProcess) readAfterEnd() {
	buf := make([]byte, 64<<10)
	lastData := time.Now()
	eof := false
	for {
		now := time.Now()
		if p.terminating {
			// The rest of the grace belongs to the whole group, also when
			// nothing of it holds the pipe any more; it ends when no process
			// of the group is left that has not ended.
			if !p.group.exists() {
				break
			}
			if now.After(p.killAt) {
				if !p.killed {
					p.killRest()
				}
				break
			}
		} else {
			if eof || now.Sub(lastData) >= pipeDrainSilence {
				break
			}
			if p.stopping() || p.limitPassed(now) {
				p.terminate(now)
				continue
			}
		}
		if eof {
			time.Sleep(commandPollMax)
			continue
		}
		_ = p.r.SetReadDeadline(time.Now().Add(commandPollMax))
		n, err := p.r.Read(buf)
		if n > 0 {
			p.buf.Write(buf[:n])
			lastData = time.Now()
		}
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			eof = true
		}
	}
	if !eof && p.terminating {
		// What the group wrote before it was gone.
		eof = drainAvailable(p.r, p.buf, pipeSizeCommand)
	}
	p.finish(eof)
}

// finish ends the reading: the pipe is closed when nothing holds it open
// any more (eof), else handed to Leftover when there is one.
func (p *commandProcess) finish(eof bool) {
	p.done = true
	p.endAt = time.Now()
	if !eof && p.in.Leftover != nil {
		_ = p.r.SetReadDeadline(time.Time{})
		p.in.Leftover(p.r)
		return
	}
	p.r.Close()
}

// output returns the console output of the command with the markers
// removed.
func (p *commandProcess) output() wrappedOutput {
	if p.failed != nil {
		return *p.failed
	}
	return splitMarkers(p.buf.Bytes(), p.buf.total, p.marker)
}

// result returns the result of the ended command; with collect, the values
// are extracted from its output.
func (p *commandProcess) result(collect bool) *CommandResultReport {
	c := p.in.Command
	out := p.output()
	res := &CommandResultReport{Position: p.in.Position, CommandLine: c.CommandLine, Note: c.Note, StartedAt: p.startAt.Unix(),
		FinishedAt: p.endAt.Unix()}
	res.Output, res.OutputBytes, res.Truncated = out.output, out.total, out.total > int64(len(out.output))
	raw := p.raw
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
	case !raw.exited:
		// Not waited for to its end (see waitMain).
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
	if collect && out.entered {
		v := ExtractCommandValues(c.Patterns, res.Output, c.LineLimit)
		res.Values = &v
	}
	return res
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

// leftoverPipes holds the output pipes of commands whose reading ended while
// processes they left behind still held the pipe open. Closing such a pipe
// would end those processes at their next write (SIGPIPE), and a pipe that
// nobody reads stops them once it is full. So every pipe has its own reader,
// which reads and drops what arrives as it arrives, whatever the workers and
// the communication flow do, and closes the pipe at its end (every process
// that held it ended). closeAll (the agent ends) closes the pipes that are
// left and returns once their readers ended. It is safe for use by several
// flows (its own mutex); the zero value holds nothing.
type leftoverPipes struct {
	mu    sync.Mutex
	pipes map[*os.File]bool
	// closing is set while closeAll runs: a pipe handed over then is closed
	// at once.
	closing bool
	readers sync.WaitGroup
}

// add takes a pipe (CommandExecInput.Leftover) and starts its reader.
func (l *leftoverPipes) add(f *os.File) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing {
		f.Close()
		return
	}
	if l.pipes == nil {
		l.pipes = map[*os.File]bool{}
	}
	l.pipes[f] = true
	l.readers.Add(1)
	go l.read(f)
}

// read is the reader of one pipe: it reads and drops what arrives (a read
// waits until something arrives) until the pipe reached its end or was
// closed by closeAll, then closes it and lets it go.
func (l *leftoverPipes) read(f *os.File) {
	defer l.readers.Done()
	buf := make([]byte, 64<<10)
	for {
		if _, err := f.Read(buf); err != nil {
			break
		}
	}
	f.Close()
	l.mu.Lock()
	delete(l.pipes, f)
	l.mu.Unlock()
}

// closeAll closes every held pipe and returns once their readers ended (the
// agent ends).
func (l *leftoverPipes) closeAll() {
	l.mu.Lock()
	l.closing = true
	for f := range l.pipes {
		// The reader's read returns at once and the reader ends.
		f.Close()
	}
	l.mu.Unlock()
	l.readers.Wait()
	l.mu.Lock()
	l.closing = false
	l.mu.Unlock()
}

// held returns the number of held pipes (their readers have not ended).
func (l *leftoverPipes) held() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pipes)
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

// killProcessGroup, signalGroupAsUser and wait4 can be replaced by tests (an
// agent that runs as root is never refused).
var (
	killProcessGroup  = syscall.Kill
	signalGroupAsUser = signalGroupAs
	wait4             = syscall.Wait4
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
