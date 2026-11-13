package modules

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// agentUserName is the name of the user the tests run as.
func agentUserName() string {
	_, byUID := ReadPasswd()
	return NameOrID(byUID, os.Geteuid())
}

func testCommand(line string) *CommandSpec {
	s := &CommandSetSpec{Commands: []CommandSpec{{CommandLine: line}}}
	s.Normalize()
	return &s.Commands[0]
}

func runTestCommand(t *testing.T, c *CommandSpec, user, dir string, hooks CommandExecHooks) *CommandResultReport {
	t.Helper()
	return ExecuteCommand(CommandExecInput{OSUser: user, WorkDir: dir, Command: c, KillGrace: 300 * time.Millisecond}, hooks)
}

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := LookupCommand("bash"); err != nil {
		t.Skip("bash is not available")
	}
}

func TestExecuteCommandMarkersAndCodes(t *testing.T) {
	requireBash(t)
	user := agentUserName()
	if !IsAgentUser(user) {
		t.Fatalf("the agent's own user %q must be recognized", user)
	}
	dir := t.TempDir()

	r := runTestCommand(t, testCommand("echo hello; echo err >&2"), user, dir, CommandExecHooks{})
	if r.Kind != ResultSuccess || r.ExitCode == nil || *r.ExitCode != 0 {
		t.Fatalf("success: %+v", r)
	}
	if string(r.Output) != "hello\nerr\n" || r.OutputBytes != int64(len(r.Output)) || r.Truncated {
		t.Fatalf("output (markers must be removed, both streams in one): %q %d", r.Output, r.OutputBytes)
	}

	cases := []struct {
		line   string
		kind   string
		reason string
		code   int
	}{
		{"exit 1", ResultFailure, "", 1},
		{"exit 7", ResultFailure, "", 7},
		{"true", ResultSuccess, "", 0},
	}
	for _, c := range cases {
		r := runTestCommand(t, testCommand(c.line), user, dir, CommandExecHooks{})
		if r.Kind != c.kind || r.Reason != c.reason || r.ExitCode == nil || *r.ExitCode != c.code {
			t.Errorf("%q: %+v", c.line, r)
		}
	}

	// A code in neither spec is an error with its exit code.
	cmd := testCommand("exit 3")
	cmd.Failure = CodeSpec{Kind: CodeSingle, A: 1}
	r = runTestCommand(t, cmd, user, dir, CommandExecHooks{})
	if r.Kind != ResultError || r.Reason != ReasonUnexpectedCode || r.ExitCode == nil || *r.ExitCode != 3 {
		t.Fatalf("unexpected code: %+v", r)
	}
	// A range spec for success.
	cmd = testCommand("exit 2")
	cmd.Success = CodeSpec{Kind: CodeRange, A: 0, B: 2}
	cmd.Failure = CodeSpec{Kind: CodeMin, A: 3}
	if r = runTestCommand(t, cmd, user, dir, CommandExecHooks{}); r.Kind != ResultSuccess {
		t.Fatalf("range: %+v", r)
	}
	// A signal that ends the command is an error without exit code.
	r = runTestCommand(t, testCommand("kill -KILL $$"), user, dir, CommandExecHooks{})
	if r.Kind != ResultError || r.Reason != ReasonSignaled || r.ExitCode != nil {
		t.Fatalf("signaled: %+v", r)
	}
	// Standard input is /dev/null.
	if r = runTestCommand(t, testCommand("cat; echo done"), user, dir, CommandExecHooks{}); string(r.Output) != "done\n" {
		t.Fatalf("stdin: %+v", r)
	}
}

func TestExecuteCommandDirectories(t *testing.T) {
	requireBash(t)
	user := agentUserName()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := runTestCommand(t, testCommand("pwd"), user, dir, CommandExecHooks{})
	if strings.TrimSpace(string(r.Output)) != dir {
		t.Fatalf("working directory: %q", r.Output)
	}
	// cd inside a command affects only that command.
	r = runTestCommand(t, testCommand("cd sub && pwd"), user, dir, CommandExecHooks{})
	if r.Kind != ResultSuccess || strings.TrimSpace(string(r.Output)) != filepath.Join(dir, "sub") {
		t.Fatalf("cd x && y: %+v %q", r, r.Output)
	}
	r = runTestCommand(t, testCommand("pwd"), user, dir, CommandExecHooks{})
	if strings.TrimSpace(string(r.Output)) != dir {
		t.Fatalf("the next command must start in the working directory again: %q", r.Output)
	}
	// Several lines, passed byte for byte (quotes, $ and a here document).
	line := "x='a b'\necho \"$x\"\ncat <<'EOF'\n$HOME `x`\nEOF\nprintf '%s\\n' \"$0\""
	r = runTestCommand(t, testCommand(line), user, dir, CommandExecHooks{})
	if r.Kind != ResultSuccess || !strings.HasPrefix(string(r.Output), "a b\n$HOME `x`\n") {
		t.Fatalf("several lines: %+v %q", r, r.Output)
	}

	// A working directory that cannot be entered.
	r = runTestCommand(t, testCommand("echo never"), user, filepath.Join(dir, "missing"), CommandExecHooks{})
	if r.Kind != ResultError || r.Reason != ReasonWorkdirFailed || r.ExitCode != nil || strings.Contains(string(r.Output), "never") {
		t.Fatalf("missing working directory: %+v %q", r, r.Output)
	}
	if !strings.Contains(string(r.Output), "missing") {
		t.Fatalf("the message of cd must be kept: %q", r.Output)
	}
}

// withPasswd replaces /etc/passwd for the test with one line for the user
// the test runs as, with the given home directory (no line when home is
// "-").
func withPasswd(t *testing.T, user, home string) {
	t.Helper()
	content := ""
	if home != "-" {
		content = fmt.Sprintf("%s:x:%d:%d::%s:/bin/sh\n", user, os.Geteuid(), os.Getegid(), home)
	}
	p := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	old := passwdFile
	passwdFile = p
	t.Cleanup(func() { passwdFile = old })
}

// unsetEnv removes an environment variable for the test (t.Setenv cannot
// unset one).
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, old)
		}
	})
}

// TestExecuteCommandHomeDirectory runs commands of the agent's own user with
// an empty working directory: they start in the home directory of
// /etc/passwd, also when the agent's environment has no HOME.
func TestExecuteCommandHomeDirectory(t *testing.T) {
	requireBash(t)
	user := agentUserName()
	home := t.TempDir()
	withPasswd(t, user, home)
	if !IsAgentUser(user) {
		t.Fatalf("the agent's own user %q must be recognized", user)
	}
	other := t.TempDir()
	t.Setenv("HOME", other)
	r := runTestCommand(t, testCommand("pwd; echo \"$HOME\""), user, "", CommandExecHooks{})
	if r.Kind != ResultSuccess || string(r.Output) != home+"\n"+other+"\n" {
		t.Fatalf("home directory of /etc/passwd, environment unchanged: %+v %q", r, r.Output)
	}
	unsetEnv(t, "HOME")
	r = runTestCommand(t, testCommand("pwd; echo \"[$HOME]\""), user, "", CommandExecHooks{})
	if r.Kind != ResultSuccess || string(r.Output) != home+"\n[]\n" {
		t.Fatalf("without HOME: %+v %q", r, r.Output)
	}
	// A home directory that cannot be entered.
	withPasswd(t, user, filepath.Join(home, "missing"))
	r = runTestCommand(t, testCommand("echo never"), user, "", CommandExecHooks{})
	if r.Kind != ResultError || r.Reason != ReasonWorkdirFailed {
		t.Fatalf("missing home: %+v %q", r, r.Output)
	}
}

// TestExecuteCommandHomeFallback: without a home directory in /etc/passwd,
// the plain cd uses HOME of the environment.
func TestExecuteCommandHomeFallback(t *testing.T) {
	requireBash(t)
	user := agentUserName()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, passwdHome := range []string{"", "-"} {
		withPasswd(t, user, passwdHome)
		osUser := user
		if passwdHome == "-" {
			// Without an entry the agent's user is known by its number.
			osUser = strconv.Itoa(os.Geteuid())
		}
		if !IsAgentUser(osUser) {
			t.Fatalf("passwd home %q: %q must be the agent's user", passwdHome, osUser)
		}
		r := runTestCommand(t, testCommand("pwd"), osUser, "", CommandExecHooks{})
		if r.Kind != ResultSuccess || strings.TrimSpace(string(r.Output)) != home {
			t.Fatalf("passwd home %q: %+v %q", passwdHome, r, r.Output)
		}
	}
	unsetEnv(t, "HOME")
	r := runTestCommand(t, testCommand("echo never"), strconv.Itoa(os.Geteuid()), "", CommandExecHooks{})
	if r.Kind != ResultError || r.Reason != ReasonWorkdirFailed {
		t.Fatalf("no home at all: %+v %q", r, r.Output)
	}
}

func TestExecuteCommandStartFailed(t *testing.T) {
	requireBash(t)
	t.Setenv("PATH", t.TempDir())
	r := runTestCommand(t, testCommand("echo x"), agentUserName(), "/", CommandExecHooks{})
	if r.Kind != ResultError || r.Reason != ReasonStartFailed || !strings.Contains(string(r.Output), "bash") {
		t.Fatalf("no bash in PATH: %+v %q", r, r.Output)
	}
}

func TestExecuteCommandTimeLimitAndOutputCap(t *testing.T) {
	requireBash(t)
	user := agentUserName()
	dir := t.TempDir()

	cmd := testCommand("echo before; sleep 30")
	cmd.TimeoutSec = 1
	start := time.Now()
	r := runTestCommand(t, cmd, user, dir, CommandExecHooks{})
	if r.Kind != ResultError || r.Reason != ReasonTimeout || string(r.Output) != "before\n" || time.Since(start) > 5*time.Second {
		t.Fatalf("time limit: %+v %q after %s", r, r.Output, time.Since(start))
	}
	// A command that ignores SIGTERM is killed after the grace.
	cmd = testCommand("trap '' TERM; sleep 30")
	cmd.TimeoutSec = 1
	start = time.Now()
	r = runTestCommand(t, cmd, user, dir, CommandExecHooks{})
	if r.Kind != ResultError || r.Reason != ReasonTimeout || time.Since(start) > 5*time.Second {
		t.Fatalf("SIGKILL after the grace: %+v after %s", r, time.Since(start))
	}

	r = runTestCommand(t, testCommand("head -c 2000000 /dev/zero; exit 1"), user, dir, CommandExecHooks{})
	if r.Kind != ResultFailure || len(r.Output) != MaxCommandOutputBytes || r.OutputBytes != 2000000 || !r.Truncated {
		t.Fatalf("output cap: kind %s len %d total %d truncated %v", r.Kind, len(r.Output), r.OutputBytes, r.Truncated)
	}
}

// closedAfter returns a channel that is closed after d (a stop of the agent).
func closedAfter(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	time.AfterFunc(d, func() { close(ch) })
	return ch
}

func TestExecuteCommandHooks(t *testing.T) {
	requireBash(t)
	user := agentUserName()
	monitored := 0
	due := true
	hooks := CommandExecHooks{
		MonitorDue: func() bool { return due },
		Monitor:    func() { monitored++; due = false },
	}
	r := runTestCommand(t, testCommand("sleep 0.3; echo ok"), user, "/", hooks)
	if r.Kind != ResultSuccess || monitored != 1 || string(r.Output) != "ok\n" {
		t.Fatalf("monitoring while the command runs: %+v %d", r, monitored)
	}

	hooks = CommandExecHooks{Stop: closedAfter(200 * time.Millisecond)}
	start := time.Now()
	r = runTestCommand(t, testCommand("echo started; sleep 30"), user, "/", hooks)
	if r.Kind != ResultError || r.Reason != ReasonInterrupted || string(r.Output) != "started\n" || time.Since(start) > 5*time.Second {
		t.Fatalf("stop: %+v %q", r, r.Output)
	}
}

func TestExecuteCommandValues(t *testing.T) {
	requireBash(t)
	cmd := testCommand("printf 'disk / 40%%\\ndisk /var 75%%\\n'; exit 1")
	cmd.Patterns = []string{`^disk (?P<mount>\S+) (?<use>\d+)%$`}
	r := ExecuteCommand(CommandExecInput{OSUser: agentUserName(), WorkDir: "/", Command: cmd, CollectOutput: true}, CommandExecHooks{})
	if r.Kind != ResultFailure || r.Values == nil {
		t.Fatalf("values: %+v", r)
	}
	got := []string{}
	for _, v := range r.Values.Values {
		got = append(got, v.Name+"="+v.Value)
	}
	if strings.Join(got, " ") != "mount=/ use=40 mount=/var use=75" {
		t.Fatalf("values of a failed command: %v", got)
	}
	// Without patterns the output is the value; a run-only set has none.
	r = ExecuteCommand(CommandExecInput{OSUser: agentUserName(), WorkDir: "/", Command: testCommand("echo a; echo b"), CollectOutput: true}, CommandExecHooks{})
	if r.Values == nil || len(r.Values.Values) != 1 || r.Values.Values[0].Value != "a\nb" {
		t.Fatalf("output as value: %+v", r.Values)
	}
	r = ExecuteCommand(CommandExecInput{OSUser: agentUserName(), WorkDir: "/", Command: testCommand("echo a")}, CommandExecHooks{})
	if r.Values != nil {
		t.Fatalf("run only: %+v", r.Values)
	}
}

func TestSplitMarkers(t *testing.T) {
	m := "abc"
	o := splitMarkers([]byte("abc\nabc\nout\n"), 12, m)
	if !o.started || !o.entered || string(o.output) != "out\n" || o.total != 4 {
		t.Fatalf("both markers: %+v", o)
	}
	o = splitMarkers([]byte("abc\ncd: x: No such file\n"), 24, m)
	if !o.started || o.entered || string(o.output) != "cd: x: No such file\n" {
		t.Fatalf("first marker only: %+v", o)
	}
	o = splitMarkers([]byte("sudo: unknown user x\n"), 21, m)
	if o.started || o.entered || string(o.output) != "sudo: unknown user x\n" {
		t.Fatalf("no marker: %+v", o)
	}
	// A marker that does not start a line is not a marker.
	o = splitMarkers([]byte("xabc\n"), 5, m)
	if o.started {
		t.Fatalf("marker inside a line: %+v", o)
	}
}

// TestExecuteCommandSudo runs commands as other users through sudo. It needs
// root (sudo without a password) and the users nobody and www-data.
func TestExecuteCommandSudo(t *testing.T) {
	requireBash(t)
	if os.Geteuid() != 0 {
		t.Skip("not running as root")
	}
	if _, err := LookupCommand("sudo"); err != nil {
		t.Skip("sudo is not available")
	}
	byName, _ := ReadPasswd()
	for _, u := range []string{"nobody", "www-data"} {
		if _, ok := byName[u]; !ok {
			t.Skipf("user %s does not exist", u)
		}
	}
	for _, u := range []string{"nobody", "www-data"} {
		if IsAgentUser(u) {
			t.Fatalf("%s is not the agent's user", u)
		}
		r := runTestCommand(t, testCommand("id -un; pwd"), u, "/", CommandExecHooks{})
		if r.Kind != ResultSuccess || string(r.Output) != u+"\n/\n" {
			t.Fatalf("as %s: %+v %q", u, r, r.Output)
		}
		r = runTestCommand(t, testCommand("exit 1"), u, "/", CommandExecHooks{})
		if r.Kind != ResultFailure || r.ExitCode == nil || *r.ExitCode != 1 {
			t.Fatalf("exit code through sudo: %+v", r)
		}
	}
	// The home directory of nobody usually does not exist.
	if home := "/nonexistent"; func() bool { _, err := os.Stat(home); return err != nil }() {
		r := runTestCommand(t, testCommand("echo x"), "nobody", "", CommandExecHooks{})
		if r.Kind != ResultError || r.Reason != ReasonWorkdirFailed {
			t.Fatalf("missing home: %+v %q", r, r.Output)
		}
	}
	// An unknown user: sudo fails before the wrapper starts.
	r := runTestCommand(t, testCommand("echo x"), "no-such-user-mimamori", "/", CommandExecHooks{})
	if r.Kind != ResultError || r.Reason != ReasonStartFailed || len(r.Output) == 0 || r.ExitCode != nil {
		t.Fatalf("unknown user: %+v %q", r, r.Output)
	}
	// The time limit also ends a command run through sudo.
	cmd := testCommand("sleep 30")
	cmd.TimeoutSec = 1
	start := time.Now()
	r = runTestCommand(t, cmd, "nobody", "/", CommandExecHooks{})
	if r.Kind != ResultError || r.Reason != ReasonTimeout || time.Since(start) > 5*time.Second {
		t.Fatalf("time limit through sudo: %+v after %s", r, time.Since(start))
	}
}

// TestExecuteCommandSignalAsUser: when the agent may not signal the process
// group of a command of another OS user (EPERM, an agent that does not run
// as root), the signal is sent as that user through the switch program. The
// refusal is simulated; it needs root (sudo without a password) and nobody.
func TestExecuteCommandSignalAsUser(t *testing.T) {
	requireBash(t)
	if os.Geteuid() != 0 {
		t.Skip("not running as root")
	}
	if _, err := LookupCommand("sudo"); err != nil {
		t.Skip("sudo is not available")
	}
	if byName, _ := ReadPasswd(); byName["nobody"].Name == "" {
		t.Skip("user nobody does not exist")
	}
	refused := 0
	killProcessGroup = func(pid int, sig syscall.Signal) error {
		refused++
		return syscall.EPERM
	}
	t.Cleanup(func() { killProcessGroup = syscall.Kill })

	// A command that ignores SIGTERM and has a child: ended by the time limit
	// through the helper (SIGTERM, then SIGKILL after the grace).
	marker := "mimamori-test-" + strconv.Itoa(os.Getpid())
	cmd := testCommand("trap '' TERM; sleep 31 & exec -a " + marker + " sleep 32")
	cmd.TimeoutSec = 1
	start := time.Now()
	r := runTestCommand(t, cmd, "nobody", "/", CommandExecHooks{})
	if r.Kind != ResultError || r.Reason != ReasonTimeout || time.Since(start) > 10*time.Second || refused < 2 {
		t.Fatalf("time limit through the helper: %+v after %s (%d refused)", r, time.Since(start), refused)
	}
	if left := processesOf(t, "sleep 31", marker); left != "" {
		t.Fatalf("nothing of the command may be left: %s", left)
	}

	// The helper fails too: a stop ends the wait one grace after SIGKILL,
	// with the result interrupted, and says so in the log.
	signalGroupAsUser = func(string, int, syscall.Signal, func()) error { return syscall.EPERM }
	t.Cleanup(func() { signalGroupAsUser = signalGroupAs })
	var logged []string
	hooks := CommandExecHooks{
		Stop: closedAfter(200 * time.Millisecond),
		Logf: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) },
	}
	cmd = testCommand("trap '' TERM; echo started; exec -a " + marker + " sleep 33")
	start = time.Now()
	r = runTestCommand(t, cmd, "nobody", "/", hooks)
	took := time.Since(start)
	killProcessGroup = syscall.Kill
	// Clean up what the simulated refusal left running.
	if pgid := pgidOf(t, marker); pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	if r.Kind != ResultError || r.Reason != ReasonInterrupted || string(r.Output) != "started\n" || took > 5*time.Second {
		t.Fatalf("stop with a command that cannot be ended: %+v %q after %s", r, r.Output, took)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "could not be ended") {
		t.Fatalf("the log must say that the command could not be ended: %q", logged)
	}
}

// processesOf lists the processes whose arguments contain one of the words
// (ps, empty when none).
func processesOf(t *testing.T, words ...string) string {
	t.Helper()
	res, err := RunCommand(10*time.Second, 1<<20, "ps", "-eo", "pid=,pgid=,user=,args=")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(res.Stdout), "\n") {
		for _, w := range words {
			if strings.Contains(l, w) && !strings.Contains(l, "ps -eo") {
				out = append(out, strings.TrimSpace(l))
				break
			}
		}
	}
	return strings.Join(out, "; ")
}

// pgidOf returns the process group of the process whose arguments contain
// word (0 when there is none).
func pgidOf(t *testing.T, word string) int {
	t.Helper()
	for _, l := range strings.Split(processesOf(t, word), "; ") {
		f := strings.Fields(l)
		if len(f) > 1 {
			if n, err := strconv.Atoi(f[1]); err == nil {
				return n
			}
		}
	}
	return 0
}
