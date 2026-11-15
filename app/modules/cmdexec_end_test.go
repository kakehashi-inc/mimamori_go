package modules

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// processAlive reports whether the process exists and has not ended (a
// zombie has ended).
func processAlive(pid int) bool {
	st, ok := readProcStat(procDir, pid)
	return ok && !st.ended()
}

// setSubreaper makes the test process the reaper of the processes orphaned
// below it (PR_SET_CHILD_SUBREAPER) until the test ends, so that an orphan
// that ended stays a zombie until the test reaps it, whatever the host's
// init does.
func setSubreaper(t *testing.T) {
	t.Helper()
	const prSetChildSubreaper = 36
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); e != 0 {
		t.Skipf("PR_SET_CHILD_SUBREAPER: %v", e)
	}
	t.Cleanup(func() { _, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 0, 0) })
}

// reapLater reaps the process at the end of the test (a zombie the test
// keeps).
func reapLater(t *testing.T, pid int) {
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &ws, 0, nil)
	})
}

// pidFrom waits until the file holds a process ID and returns it.
func pidFrom(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 0 {
				return n
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no process ID in %s", path)
	return 0
}

// endedAt watches a process and returns a function that gives the time it
// was seen gone (zero while it lives).
func endedAt(t *testing.T, pid int) func() time.Time {
	t.Helper()
	var at atomic.Int64
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if !processAlive(pid) {
				at.Store(time.Now().UnixNano())
				return
			}
			select {
			case <-quit:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() {
		close(quit)
		<-done
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
	return func() time.Time {
		if v := at.Load(); v > 0 {
			return time.Unix(0, v)
		}
		return time.Time{}
	}
}

// executeWithin runs ExecuteCommand and fails when it has not returned
// within d.
func executeWithin(t *testing.T, in CommandExecInput, d time.Duration) *CommandResultReport {
	t.Helper()
	res := make(chan *CommandResultReport, 1)
	go func() { res <- ExecuteCommand(in) }()
	select {
	case r := <-res:
		return r
	case <-time.After(d):
		t.Fatalf("the command has not ended %s after its start", d)
		return nil
	}
}

// TestCommandGroupGetsTheGrace (STOP-1): after SIGTERM the grace belongs to
// the whole process group. A member that needs 5 s for its TERM handler
// after the main process ended gets them and its output is read; nothing
// is killed before the grace is over. The stop comes when the member has
// set its handler.
func TestCommandGroupGetsTheGrace(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	done, ready := filepath.Join(dir, "done"), filepath.Join(dir, "ready")
	line := fmt.Sprintf(`( trap 'sleep 5; echo member done; touch %s; exit 0' TERM; touch %s; while :; do sleep 0.1; done ) &
echo started; wait`, done, ready)
	stop, stoppedAt := closeWhen(fileExists(ready))
	r := executeWithin(t, CommandExecInput{OSUser: agentUserName(), WorkDir: dir, Command: testCommand(line),
		KillGrace: 20 * time.Second, Stop: stop}, 40*time.Second)
	took := time.Since(stoppedAt())
	if _, err := os.Stat(done); err != nil {
		t.Fatalf("the member did not get the 5 s it needed after SIGTERM (result %s after the stop): %+v %q", took, r, r.Output)
	}
	if r.Kind != ResultError || r.Reason != ReasonInterrupted || !strings.Contains(string(r.Output), "member done") {
		t.Fatalf("result: %+v %q", r, r.Output)
	}
	// The reading went on until no process of the group was left (the
	// member ended 5 s after the SIGTERM of the stop), not to the end of the
	// grace of 20 s.
	if took < 5*time.Second || took > 10*time.Second {
		t.Fatalf("the command ended %s after the stop", took)
	}
}

// TestCommandGraceEndsWhenOnlyZombiesAreLeft: after SIGTERM the grace of the
// group ends as soon as only processes that ended and were not reaped yet
// (zombies; signal 0 to the group still answers) are left. Nothing is sent
// to them: no SIGKILL at the end of the grace.
func TestCommandGraceEndsWhenOnlyZombiesAreLeft(t *testing.T) {
	requireBash(t)
	setSubreaper(t)
	var kills atomic.Int32
	killProcessGroup = func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGKILL {
			kills.Add(1)
		}
		return syscall.Kill(pid, sig)
	}
	t.Cleanup(func() { killProcessGroup = syscall.Kill })
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	// The member is started by a subshell that ends at once: from then on
	// it is a child of the test (the subreaper), which does not reap it, and
	// the main process (sleep, a child of the test that the command's wait
	// reaps) cannot reap it either. The stop comes when the main process
	// runs as the marker (the member was started before).
	marker := "mimamori-test-" + strconv.Itoa(os.Getpid()) + "-zombie"
	stop, stoppedAt := closeWhen(processNamed(marker))
	r := executeWithin(t, CommandExecInput{OSUser: agentUserName(), WorkDir: dir,
		Command:   testCommand(fmt.Sprintf(`( sleep 30 & echo $! > %s ); exec -a %s sleep 30`, pidFile, marker)),
		KillGrace: 20 * time.Second, Stop: stop}, 40*time.Second)
	took := time.Since(stoppedAt())
	member := pidFrom(t, pidFile)
	reapLater(t, member)
	st, ok := readProcStat(procDir, member)
	if !ok || !st.ended() {
		t.Fatalf("the member left behind is not a zombie: %+v %v", st, ok)
	}
	if err := syscall.Kill(-st.pgrp, 0); err != nil {
		t.Fatalf("signal 0 to the group of the zombie: %v", err)
	}
	if took > 5*time.Second {
		t.Fatalf("the command ended %s after the stop: the grace of 20 s went on while only a zombie was left", took)
	}
	if kills.Load() != 0 {
		t.Fatalf("SIGKILL sent %d time(s) to a group of zombies", kills.Load())
	}
	if r.Kind != ResultError || r.Reason != ReasonInterrupted {
		t.Fatalf("result: %+v", r)
	}
}

// TestCommandGroupKilledAtTheGrace (STOP-1): a group member that ignores
// SIGTERM lives on after the main process ended and gets SIGKILL only when
// the grace is over (here 2 s after the time limit of 1 s).
func TestCommandGroupKilledAtTheGrace(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	cmd := testCommand(fmt.Sprintf(`( trap '' TERM; exec sleep 30 ) & echo $! > %s; wait`, pidFile))
	cmd.TimeoutSec = 1
	start := time.Now()
	type outcome struct {
		r    *CommandResultReport
		took time.Duration
	}
	res := make(chan outcome, 1)
	go func() {
		r := ExecuteCommand(CommandExecInput{OSUser: agentUserName(), WorkDir: dir, Command: cmd, KillGrace: 2 * time.Second})
		res <- outcome{r, time.Since(start)}
	}()
	gone := endedAt(t, pidFrom(t, pidFile))
	o := <-res
	ended := gone()
	if ended.IsZero() {
		time.Sleep(200 * time.Millisecond)
		ended = gone()
	}
	if ended.IsZero() {
		t.Fatal("the member that ignores SIGTERM is still alive after the grace")
	}
	// SIGTERM at about 1 s, SIGKILL at about 3 s.
	if d := ended.Sub(start); d < 2900*time.Millisecond || d > 6*time.Second {
		t.Fatalf("the member ended %s after the start, want about 3 s (time limit 1 s, grace 2 s)", d)
	}
	if o.r.Kind != ResultError || o.r.Reason != ReasonTimeout || o.took > 6*time.Second {
		t.Fatalf("result after %s: %+v", o.took, o.r)
	}
}

// TestCommandReadingAfterEndObeysStop (TO-1): a process left behind that
// keeps writing holds the reading after the main process ended; the stop
// ends it like during the run (SIGTERM to the group) and the command keeps
// the result of its main process. The writer begins once the main process
// is gone (reaped by the command's wait) and the stop comes when it has
// written three lines, so the stop falls into the reading after the end.
func TestCommandReadingAfterEndObeysStop(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	pidFile, three := filepath.Join(dir, "pid"), filepath.Join(dir, "three")
	line := fmt.Sprintf(`( while kill -0 $$ 2>/dev/null; do sleep 0.05; done; i=0
while :; do echo x; i=$((i+1)); [ $i = 3 ] && touch %s; sleep 0.3; done ) & echo $! > %s; echo main done`, three, pidFile)
	stop, stoppedAt := closeWhen(fileExists(three))
	r := executeWithin(t, CommandExecInput{OSUser: agentUserName(), WorkDir: dir, Command: testCommand(line),
		KillGrace: 20 * time.Second, Stop: stop}, 40*time.Second)
	took := time.Since(stoppedAt())
	writer := pidFrom(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(writer, syscall.SIGKILL) })
	// The writer ends at the SIGTERM: the reading ends with it, long before
	// the grace of 20 s is over.
	if took > 5*time.Second {
		t.Fatalf("the reading ended %s after the stop, want soon after it", took)
	}
	// Both the main process and the writer are in the output.
	if out := string(r.Output); r.Kind != ResultSuccess || r.ExitCode == nil || *r.ExitCode != 0 ||
		!strings.Contains(out, "main done\n") || strings.Count(out, "x\n") < 3 {
		t.Fatalf("the result is the main process's: %+v %q", r, r.Output)
	}
	time.Sleep(100 * time.Millisecond)
	if processAlive(writer) {
		t.Fatal("the writer left behind did not get SIGTERM at the stop")
	}
}

// TestCommandReadingAfterEndObeysTimeLimit (TO-1): the time limit applies
// while the output of a process left behind is read; the result stays the
// one of the main process.
func TestCommandReadingAfterEndObeysTimeLimit(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	cmd := testCommand(fmt.Sprintf(`( while :; do echo x; sleep 0.3; done ) & echo $! > %s; exit 0`, pidFile))
	cmd.TimeoutSec = 2
	start := time.Now()
	r := executeWithin(t, CommandExecInput{OSUser: agentUserName(), WorkDir: dir, Command: cmd, KillGrace: time.Second}, 15*time.Second)
	took := time.Since(start)
	writer := pidFrom(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(writer, syscall.SIGKILL) })
	if took < 1900*time.Millisecond || took > 5*time.Second {
		t.Fatalf("the reading ended %s after the start, want soon after the time limit of 2 s", took)
	}
	if r.Kind != ResultSuccess || r.Reason != "" || r.ExitCode == nil || *r.ExitCode != 0 {
		t.Fatalf("result: %+v", r)
	}
}

// TestCommandReadingAfterEndWithoutLimit: without a time limit and without
// a stop, the output of a process left behind is read for as long as it
// keeps arriving (gaps below 2 s).
func TestCommandReadingAfterEndWithoutLimit(t *testing.T) {
	requireBash(t)
	r := runTestCommand(t, testCommand(`( for i in 1 2 3 4 5; do sleep 1; echo line$i; done ) & echo main`), agentUserName(), "/", nil)
	if r.Kind != ResultSuccess || string(r.Output) != "main\nline1\nline2\nline3\nline4\nline5\n" {
		t.Fatalf("result: %+v %q", r, r.Output)
	}
}

// TestCommandLeftoverPipeReadAsItArrives (TO-2): a process left behind that
// is silent for more than 2 s ends the reading, but it is not ended by it:
// the pipe is handed over to its own reader, which reads and drops what
// arrives as it arrives (here 4 MiB, more than the pipe holds, while nothing
// else reads it), so the process is neither blocked on the full pipe nor
// ended by SIGPIPE; the reader closes the pipe and ends when the process
// ended. closeAll closes a pipe that a process still holds and returns once
// its reader ended.
func TestCommandLeftoverPipeReadAsItArrives(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	after := filepath.Join(dir, "after")
	var held leftoverPipes
	line := fmt.Sprintf(`( sleep 3; head -c 4194304 /dev/zero; echo late; touch %s ) & echo early`, after)
	r := ExecuteCommand(CommandExecInput{OSUser: agentUserName(), WorkDir: dir, Command: testCommand(line), Leftover: held.add})
	if r.Kind != ResultSuccess || string(r.Output) != "early\n" {
		t.Fatalf("result: %+v %q", r, r.Output)
	}
	if held.held() != 1 {
		t.Fatalf("pipes held: %d, want the one the process left behind holds", held.held())
	}
	waitFile(t, after)
	waitFor(t, "the reader to end after the process ended", func() bool { return held.held() == 0 })
	// A command whose output ended with it leaves nothing.
	if r := ExecuteCommand(CommandExecInput{OSUser: agentUserName(), WorkDir: dir, Command: testCommand("echo x"), Leftover: held.add}); r.Kind != ResultSuccess || held.held() != 0 {
		t.Fatalf("a command without processes left behind: %+v, %d held", r, held.held())
	}
	// The end of the agent closes a pipe still held open and ends its
	// reader.
	pidFile := filepath.Join(dir, "pid")
	r = ExecuteCommand(CommandExecInput{OSUser: agentUserName(), WorkDir: dir, Command: testCommand(fmt.Sprintf(`sleep 30 & echo $! > %s; echo x`, pidFile)), Leftover: held.add})
	holder := pidFrom(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(holder, syscall.SIGKILL) })
	if r.Kind != ResultSuccess || held.held() != 1 {
		t.Fatalf("a process left behind that holds the pipe: %+v, %d held", r, held.held())
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		held.closeAll()
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("closeAll did not return while a process holds the pipe")
	}
	if held.held() != 0 || !processAlive(holder) {
		t.Fatalf("after closeAll: %d held, the holder alive %v", held.held(), processAlive(holder))
	}
}
