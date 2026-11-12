package modules

import (
	"os"
	"path/filepath"
	"strings"
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

	// Empty working directory: the home directory of the user.
	home := t.TempDir()
	t.Setenv("HOME", home)
	r = runTestCommand(t, testCommand("pwd"), user, "", CommandExecHooks{})
	if r.Kind != ResultSuccess || strings.TrimSpace(string(r.Output)) != home {
		t.Fatalf("home directory: %+v %q", r, r.Output)
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

	stopAt := time.Now().Add(200 * time.Millisecond)
	hooks = CommandExecHooks{Stopping: func() bool { return time.Now().After(stopAt) }}
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
