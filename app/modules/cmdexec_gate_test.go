package modules

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The gate of a command (cmdexec.go): what the released command sees is
// what the program sees when the agent starts it directly.

// gateProbe is a command line that prints what a command sees: its
// environment, argument list and $0, working directory, open descriptors,
// signal mask and dispositions (of its shell and of a child), process group
// and session, umask and limits. It turns a trace the environment asks for
// off for itself (the traces of the members of a pipeline come in any
// order); what the wrapper traced before stays in the output.
const gateProbe = `{ set +x; } 2>/dev/null
echo "--- pid $$"
env | sort
echo "--- argv"; tr '\0' '\n' < /proc/$$/cmdline; echo
echo "--- 0: $0"
echo "--- cwd"; pwd; readlink /proc/$$/cwd
echo "--- fds of the shell"; ls -l /proc/$$/fd | awk 'NR > 1 {print $9, $11}'
echo "--- fds of a child"; ls -l /proc/self/fd | awk 'NR > 1 {print $9, $11}'
echo "--- signals of the shell"; grep -E '^Sig(Blk|Ign|Cgt)' /proc/$$/status
echo "--- signals of a child"; grep -E '^Sig(Blk|Ign|Cgt)' /proc/self/status
echo "--- group and session"; read -r _ _ _ _ pgrp sid _ < <(sed 's/.*) //' /proc/$$/stat); [ "$pgrp" = $$ ] && [ "$sid" = $$ ] && echo leader
echo "--- umask"; umask
echo "--- limits"; ulimit -a
echo "--- end"`

// pipeIno is the inode of a pipe, childProc the /proc directory of a child
// of the probe and wrapperMarker the marker of the wrapper (in its trace and
// in what sudo puts into the environment), which differ from one run to the
// next.
var (
	pipeIno       = regexp.MustCompile(`pipe:\[[0-9]+\]`)
	childProc     = regexp.MustCompile(`/proc/[0-9]+/`)
	wrapperMarker = regexp.MustCompile(`\b[0-9a-f]{32}\b`)
)

// probePid is the line of the probe that names the process ID of its shell.
var probePid = regexp.MustCompile(`--- pid ([0-9]+)`)

// normalized removes from the output of the probe what differs from one run
// to the next: the inodes of pipes, the process IDs and the marker.
func normalized(out string) string {
	out = pipeIno.ReplaceAllString(out, "pipe")
	if m := probePid.FindStringSubmatch(out); m != nil {
		out = regexp.MustCompile(`\b`+m[1]+`\b`).ReplaceAllString(out, "PID")
	}
	out = wrapperMarker.ReplaceAllString(out, "MARKER")
	return childProc.ReplaceAllString(out, "/proc/CHILD/")
}

// runWithoutGate runs a command line as the agent starts the program of a
// command without the gate (its argument list with the wrapper, directly or
// through sudo, the environment env) and returns the output without the
// markers.
func runWithoutGate(t *testing.T, user, dir, line string, env []string) string {
	t.Helper()
	marker, err := RandomHex(16)
	if err != nil {
		t.Fatal(err)
	}
	argv, _, err := commandArgv(user, marker, dir, line)
	if err != nil {
		t.Fatal(err)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	proc, err := os.StartProcess(argv[0], argv, &os.ProcAttr{Dir: "/", Files: []*os.File{devnull, w, w}, Env: env,
		Sys: &syscall.SysProcAttr{Setsid: true}})
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proc.Wait(); err != nil {
		t.Fatal(err)
	}
	return string(splitMarkers(b, int64(len(b)), marker).output)
}

// runWithGate runs a command line through the gate (ExecuteCommand) with the
// environment env and returns its output.
func runWithGate(t *testing.T, user, dir, line string, env []string) string {
	t.Helper()
	commandEnviron = func() []string { return env }
	t.Cleanup(func() { commandEnviron = os.Environ })
	r := ExecuteCommand(CommandExecInput{OSUser: user, WorkDir: dir, Command: testCommand(line)})
	if r.Kind != ResultSuccess {
		t.Fatalf("through the gate: %+v %q", r, r.Output)
	}
	return string(r.Output)
}

// gateEnvironments are the environments of the agent the tests compare: the
// test's own, none at all (a supervisor that passes nothing), one of an
// interactive shell (shell level, a wrong PWD, an exported function), shell
// options exported for debugging and a startup file for non-interactive
// shells.
//
// The startup file appends a line to the file reads at every read.
func gateEnvironments(t *testing.T) (envs map[string][]string, reads func() int) {
	t.Helper()
	dir := t.TempDir()
	startup, count := filepath.Join(dir, "startup"), filepath.Join(dir, "reads")
	if err := os.WriteFile(startup, []byte("echo read >> "+count+"\necho startup file read >&2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reads = func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "\n")
	}
	path := "PATH=" + os.Getenv("PATH")
	return map[string][]string{
		"the test's": os.Environ(),
		"empty":      {},
		"interactive shell": {path, "SHLVL=2", "PWD=/home/nowhere", "OLDPWD=/var", "_=/usr/bin/mimamori",
			"BASH_FUNC_greet%%=() {  echo hi\n}", "LANG=C.UTF-8"},
		"shell options": {path, "SHELLOPTS=braceexpand:hashall:interactive-comments:xtrace", "BASHOPTS=extglob:nullglob",
			"PS4=+trace "},
		"startup file":            {path, "BASH_ENV=" + startup},
		"empty startup file name": {path, "BASH_ENV="},
	}, reads
}

// compareThroughGate runs the probe without and with the gate and fails when
// the command sees something else or the startup file is read a different
// number of times.
func compareThroughGate(t *testing.T, user, dir string, env []string, reads func() int) {
	t.Helper()
	before := reads()
	without := runWithoutGate(t, user, dir, gateProbe, env)
	between := reads()
	with := runWithGate(t, user, dir, gateProbe, env)
	if !strings.Contains(without, "--- end") {
		t.Fatalf("the probe did not run: %q", without)
	}
	if a, b := normalized(without), normalized(with); a != b {
		t.Fatalf("in %s as %s the command sees something else through the gate:\nwithout the gate:\n%s\nwith the gate:\n%s", dir, user, a, b)
	}
	if a, b := between-before, reads()-between; a != b {
		t.Fatalf("in %s as %s the startup file was read %d time(s) without the gate, %d time(s) with it", dir, user, a, b)
	}
}

// TestGateIsTransparent: a command released by its gate sees exactly what it
// sees when the agent starts its program directly: environment (SHLVL, PWD
// and _ included), argument list and $0, working directory, open
// descriptors, signal mask and dispositions, process group and session,
// umask and limits; its output is the same, also with shell options or a
// startup file in the agent's environment (the gate's own trace goes
// nowhere, and the gate reads no startup file).
func TestGateIsTransparent(t *testing.T) {
	requireBash(t)
	envs, reads := gateEnvironments(t)
	for name, env := range envs {
		t.Run(name, func(t *testing.T) {
			for _, dir := range []string{"/", "/tmp"} {
				compareThroughGate(t, agentUserName(), dir, env, reads)
			}
		})
	}
}

// TestGateDropsTheStartWarningsOfBash: a value of the agent's environment
// bash warns about at its start (an unknown name in SHELLOPTS, a shell level
// of 999 or more) is warned about by the gate's bash, whose own output goes
// nowhere: the warning is not in the command's output, and the command sees
// the same as without the gate otherwise.
func TestGateDropsTheStartWarningsOfBash(t *testing.T) {
	requireBash(t)
	path := "PATH=" + os.Getenv("PATH")
	for name, c := range map[string]struct {
		env     []string
		warning string
	}{
		"unknown shell option": {[]string{path, "SHELLOPTS=errexit:bogus"}, "bogus: invalid option name"},
		"shell level 999":      {[]string{path, "SHLVL=999"}, "shell level (1000) too high, resetting to 1"},
		"shell level 1500":     {[]string{path, "SHLVL=1500"}, "shell level (1501) too high, resetting to 1"},
	} {
		t.Run(name, func(t *testing.T) {
			without := runWithoutGate(t, agentUserName(), "/", gateProbe, c.env)
			with := runWithGate(t, agentUserName(), "/", gateProbe, c.env)
			var rest []string
			for _, line := range strings.SplitAfter(without, "\n") {
				if !strings.Contains(line, c.warning) {
					rest = append(rest, line)
				}
			}
			if len(rest) == len(strings.SplitAfter(without, "\n")) {
				t.Fatalf("without the gate there is no warning %q:\n%s", c.warning, without)
			}
			if strings.Contains(with, c.warning) {
				t.Fatalf("through the gate the warning %q is in the output:\n%s", c.warning, with)
			}
			if a, b := normalized(strings.Join(rest, "")), normalized(with); a != b {
				t.Fatalf("the command sees something else through the gate:\nwithout the gate (warning left out):\n%s\nwith the gate:\n%s", a, b)
			}
		})
	}
}

// TestGateIsTransparentThroughSudo: the same for a command of another OS user
// (started through sudo). It needs root, sudo and the user nobody.
func TestGateIsTransparentThroughSudo(t *testing.T) {
	requireBash(t)
	if reason := noSudoToNobody(); reason != "" {
		t.Skip(reason)
	}
	envs, reads := gateEnvironments(t)
	for name, env := range envs {
		t.Run(name, func(t *testing.T) {
			compareThroughGate(t, "nobody", "/", env, reads)
		})
	}
}

// runCheck runs a program and reports whether it exited with 0.
func runCheck(path string, args ...string) error {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	p, err := os.StartProcess(path, append([]string{path}, args...), &os.ProcAttr{Files: []*os.File{devnull, devnull, devnull}})
	if err != nil {
		return err
	}
	st, err := p.Wait()
	if err != nil {
		return err
	}
	if !st.Success() {
		return &os.SyscallError{Syscall: st.String(), Err: syscall.EPERM}
	}
	return nil
}

// noSudoToNobody returns why a command of the user nobody cannot be started
// through sudo here (root, sudo and the user nobody are needed), or "" when
// it can.
func noSudoToNobody() string {
	if os.Geteuid() != 0 {
		return "not running as root: no command of another OS user can be started without a password"
	}
	sudo, err := LookupCommand("sudo")
	if err != nil {
		return "sudo is not available"
	}
	if byName, _ := ReadPasswd(); func() bool { _, ok := byName["nobody"]; return !ok }() {
		return "the user nobody does not exist"
	}
	if err := runCheck(sudo, "-n", "-u", "nobody", "--", "true"); err != nil {
		return fmt.Sprintf("sudo cannot run a command as nobody here: %v", err)
	}
	return ""
}

// duplicateHelperEnv holds the OS users of the helper process of
// TestDuplicateVariableReachesCommandOnce (separated by spaces).
const duplicateHelperEnv = "MIMAMORI_TEST_DUPLICATE_HELPER_USERS"

// duplicateProbe prints the two variables the helper's environment holds
// twice: TERM, which sudo keeps for the other OS user, and one of the test.
const duplicateProbe = `printf '%s|%s\n' "${TERM-unset}" "${MIMAMORI_TEST_DUPLICATE-unset}"`

// TestDuplicateVariableHelper is the agent of
// TestDuplicateVariableReachesCommandOnce: for every OS user the environment
// names it runs the probe through the gate, with the environment of the
// commands as the agent takes it, and without the gate, and prints the lines
// "USER with: OUTPUT" and "USER without: OUTPUT".
func TestDuplicateVariableHelper(t *testing.T) {
	users := os.Getenv(duplicateHelperEnv)
	if users == "" {
		t.Skip("the helper process of TestDuplicateVariableReachesCommandOnce")
	}
	for _, user := range strings.Fields(users) {
		r := ExecuteCommand(CommandExecInput{OSUser: user, WorkDir: "/", Command: testCommand(duplicateProbe)})
		if r.Kind != ResultSuccess {
			t.Fatalf("as %s through the gate: %+v %q", user, r, r.Output)
		}
		fmt.Printf("%s with: %s", user, r.Output)
		fmt.Printf("%s without: %s", user, runWithoutGate(t, user, "/", duplicateProbe, commandEnviron()))
	}
}

// TestDuplicateVariableReachesCommandOnce: of a variable that is twice in the
// environment the agent was started with, a command gets the first value,
// the same through the gate as without it, for the agent's own user and
// through sudo for another OS user (that part needs root, sudo and the user
// nobody and is left out with a message without them).
func TestDuplicateVariableReachesCommandOnce(t *testing.T) {
	requireBash(t)
	users := map[string]string{agentUserName(): "first|first"}
	if reason := noSudoToNobody(); reason != "" {
		t.Logf("the command of another OS user is left out: %s", reason)
	} else {
		users["nobody"] = "first|unset"
	}
	names := make([]string, 0, len(users))
	for user := range users {
		names = append(names, user)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "TERM=first", "MIMAMORI_TEST_DUPLICATE=first", "TERM=second",
		"MIMAMORI_TEST_DUPLICATE=second", duplicateHelperEnv + "=" + strings.Join(names, " ")}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// os.StartProcess hands the list over as it is, duplicates included.
	proc, err := os.StartProcess(os.Args[0], []string{os.Args[0], "-test.run=^TestDuplicateVariableHelper$"},
		&os.ProcAttr{Env: env, Files: []*os.File{devnull, w, w}})
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	st, err := proc.Wait()
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if !st.Success() {
		t.Fatalf("the helper failed (%s):\n%s", st, out)
	}
	for user, want := range users {
		for _, how := range []string{"with", "without"} {
			if line := user + " " + how + ": " + want + "\n"; !strings.Contains(out, line) {
				t.Fatalf("as %s %s the gate the command does not see %q (TERM|MIMAMORI_TEST_DUPLICATE):\n%s", user, how, want, out)
			}
		}
	}
}

// TestGateRunsNothingWithoutRelease: a command given up before its release
// runs nothing, not even the startup file of the agent's environment: its
// gate ends and no process of its group is left; the command never ran.
// Released, the same command runs with the startup file read by its own
// shell as without the gate.
func TestGateRunsNothingWithoutRelease(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	startup, ran, read := filepath.Join(dir, "startup"), filepath.Join(dir, "ran"), filepath.Join(dir, "read")
	if err := os.WriteFile(startup, []byte("touch "+read+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commandEnviron = func() []string { return append(os.Environ(), "BASH_ENV="+startup) }
	t.Cleanup(func() { commandEnviron = os.Environ })
	in := CommandExecInput{OSUser: agentUserName(), WorkDir: "/", Command: testCommand("touch " + ran)}
	p := startCommand(in)
	if p.pgid() == 0 {
		t.Fatalf("not started: %+v", p.failed)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(read); err == nil {
		t.Fatal("the gate read the startup file before its release")
	}
	p.wait()
	if p.ran() || groupExists(p.pgid()) {
		t.Fatalf("given up: ran %v, group left %v", p.ran(), groupExists(p.pgid()))
	}
	for _, f := range []string{ran, read} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("%s exists: something ran without the release", f)
		}
	}
	p = startCommand(in)
	p.release()
	p.wait()
	if r := p.result(false); r.Kind != ResultSuccess || !p.ran() {
		t.Fatalf("released: %+v %q", r, r.Output)
	}
	for _, f := range []string{ran, read} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("released, the command did not run as without the gate: %v", err)
		}
	}
}

// TestGateTimeLimitFromTheRelease: the time limit of a command counts from
// its release, not from the start of its gate.
func TestGateTimeLimitFromTheRelease(t *testing.T) {
	requireBash(t)
	c := testCommand("sleep 30")
	c.TimeoutSec = 1
	p := startCommand(CommandExecInput{OSUser: agentUserName(), WorkDir: "/", Command: c, KillGrace: 300 * time.Millisecond})
	time.Sleep(1500 * time.Millisecond)
	released := time.Now()
	p.release()
	p.wait()
	r := p.result(false)
	if took := time.Since(released); r.Reason != ReasonTimeout || took < time.Second || took > 3*time.Second {
		t.Fatalf("result %+v %s after the release", r, took)
	}
	if r.StartedAt < released.Unix() {
		t.Fatalf("the command's start %d is before its release %d", r.StartedAt, released.Unix())
	}
}
