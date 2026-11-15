package modules

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A command of another OS user is started through sudo, which keeps the
// agent's user as its real user: an agent that does not run as root reaches
// sudo with a signal to the command's process group, but not the processes
// of that user. These tests run as root and give the product the view of an
// agent that does not: a signal to the group reaches its leader (sudo) only.

// leaderOnly replaces killProcessGroup with one that delivers a signal for a
// process group to the group's leader only (sudo, the process a non-root
// agent may signal) and reports success, as the kernel does when one member
// was reached. Signal 0 and signals to single processes are the real ones.
// It returns the process group seen.
func leaderOnly(t *testing.T) func() int {
	t.Helper()
	var mu sync.Mutex
	pgid := 0
	killProcessGroup = func(pid int, sig syscall.Signal) error {
		if pid >= 0 || sig == 0 {
			return syscall.Kill(pid, sig)
		}
		mu.Lock()
		pgid = -pid
		mu.Unlock()
		_ = syscall.Kill(-pid, sig)
		return nil
	}
	t.Cleanup(func() { killProcessGroup = syscall.Kill })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return pgid
	}
}

// requireSudoNobody skips unless the test runs as root with sudo and the
// user nobody (sudo starts commands as nobody without a password).
func requireSudoNobody(t *testing.T) {
	t.Helper()
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
}

// TestExecuteCommandNonRootAgentSignalsAsUser: an agent that does not run as
// root sends every signal to the process group of a command of another OS
// user also through the helper as that user, although the signal to the
// group reached sudo and did not fail. The time limit then ends the
// command's own processes (SIGTERM, then SIGKILL to what is left).
func TestExecuteCommandNonRootAgentSignalsAsUser(t *testing.T) {
	requireSudoNobody(t)
	agentIsRoot = func() bool { return false }
	t.Cleanup(func() { agentIsRoot = func() bool { return os.Geteuid() == 0 } })
	group := leaderOnly(t)
	var mu sync.Mutex
	var sent []syscall.Signal
	signalGroupAsUser = func(user string, pgid int, sig syscall.Signal, drain func()) error {
		mu.Lock()
		sent = append(sent, sig)
		mu.Unlock()
		return signalGroupAs(user, pgid, sig, drain)
	}
	t.Cleanup(func() { signalGroupAsUser = signalGroupAs })

	marker := "mimamori-test-" + strconv.Itoa(os.Getpid()) + "-nonroot"
	child := marker + "-child"
	cmd := testCommand("trap '' TERM; (exec -a " + child + " sleep 31) & exec -a " + marker + " sleep 32")
	cmd.TimeoutSec = 1
	var logged []string
	r := ExecuteCommand(CommandExecInput{OSUser: "nobody", WorkDir: "/", Command: cmd, KillGrace: 300 * time.Millisecond,
		Logf: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }})
	left := processesLeft(group(), marker, child)
	for limit := time.Now().Add(5 * time.Second); len(left) > 0 && time.Now().Before(limit); left = processesLeft(group(), marker, child) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(left) > 0 {
		_ = syscall.Kill(-group(), syscall.SIGKILL)
		t.Fatalf("the command of the other user must be ended by the time limit: left %v (helper signals %v, log %q)", left, sent, logged)
	}
	if r.Kind != ResultError || r.Reason != ReasonTimeout {
		t.Fatalf("result: %+v %q", r, r.Output)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 || sent[0] != syscall.SIGTERM || sent[1] != syscall.SIGKILL {
		t.Fatalf("the helper must send SIGTERM and then SIGKILL: %v", sent)
	}
}

// TestExecuteCommandNotEndedAfterKill: when the processes of a command are
// still there one grace after SIGKILL (the helper could not reach them
// either), the run is not closed as if nothing were left: the result's
// output and the log say that the command could not be ended. This holds for
// the time limit (not only for the stop).
func TestExecuteCommandNotEndedAfterKill(t *testing.T) {
	requireSudoNobody(t)
	agentIsRoot = func() bool { return false }
	t.Cleanup(func() { agentIsRoot = func() bool { return os.Geteuid() == 0 } })
	group := leaderOnly(t)
	signalGroupAsUser = func(string, int, syscall.Signal, func()) error { return syscall.EPERM }
	t.Cleanup(func() { signalGroupAsUser = signalGroupAs })

	marker := "mimamori-test-" + strconv.Itoa(os.Getpid()) + "-notended"
	cmd := testCommand("trap '' TERM; echo started; (exec -a " + marker + " sleep 33) & wait")
	cmd.TimeoutSec = 1
	var logged []string
	start := time.Now()
	r := ExecuteCommand(CommandExecInput{OSUser: "nobody", WorkDir: "/", Command: cmd, KillGrace: 300 * time.Millisecond,
		Logf: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }})
	took := time.Since(start)
	pgid := group()
	left := processesLeft(pgid, marker)
	if pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	if len(left) == 0 {
		t.Fatalf("the simulated refusal must leave the command running")
	}
	if r.Kind != ResultError || r.Reason != ReasonTimeout || !strings.HasPrefix(string(r.Output), "started\n") {
		t.Fatalf("result: %+v %q", r, r.Output)
	}
	note := fmt.Sprintf("%s: the command could not be ended: its process group %d still had processes after SIGKILL\n", AppName, pgid)
	if !strings.HasSuffix(string(r.Output), note) || r.OutputBytes != int64(len(r.Output)) {
		t.Fatalf("the output must end with the note %q: %q (%d bytes)", note, r.Output, r.OutputBytes)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "could not be ended") {
		t.Fatalf("the log must say that the command could not be ended: %q", logged)
	}
	// SIGTERM after 1 s, SIGKILL one grace later, given up one more grace
	// later: well within 5 s, and not before the second grace.
	if took < time.Second+600*time.Millisecond || took > 5*time.Second {
		t.Fatalf("the wait took %s", took)
	}
}

// asUserEnv names the agent's user and the command's user of
// TestExecuteCommandAsOtherUserFromNonRootAgent: "AGENT:USER", where AGENT
// may run bash as USER through sudo without a password (a sudoers rule such
// as "AGENT ALL=(USER) NOPASSWD: /usr/bin/bash").
const asUserEnv = "MIMAMORI_TEST_SUDO_USERS"

// asUserChildEnv is set in the copy of the test binary that runs as the
// agent's user.
const asUserChildEnv = "MIMAMORI_TEST_SUDO_CHILD"

// TestExecuteCommandAsOtherUserFromNonRootAgent runs the product as an agent
// that really does not run as root: the test binary runs itself again as the
// agent's user, which runs a command as another user through sudo with a
// time limit, and one without a limit that the stop ends. Nothing of the
// command's processes may be left. It needs root, two OS users and the
// sudoers rule named by MIMAMORI_TEST_SUDO_USERS.
func TestExecuteCommandAsOtherUserFromNonRootAgent(t *testing.T) {
	if spec := os.Getenv(asUserChildEnv); spec != "" {
		asUserChild(spec)
		return
	}
	spec := os.Getenv(asUserEnv)
	if spec == "" {
		t.Skip("set " + asUserEnv + "=AGENT:USER (AGENT may run bash as USER through sudo without a password) to run it")
	}
	requireBash(t)
	if os.Geteuid() != 0 {
		t.Skip("not running as root")
	}
	agentName, osUser, ok := strings.Cut(spec, ":")
	byName, _ := ReadPasswd()
	agent, known := byName[agentName]
	if !ok || !known || byName[osUser].Name == "" || agent.UID == 0 {
		t.Fatalf("%s=%q must name two existing users, the first one not root", asUserEnv, spec)
	}
	// The copy of the test binary must be reachable for the agent's user.
	dir, err := os.MkdirTemp("", "mimamori-asuser-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "modules.test")
	if err := copyExecutable(self, bin); err != nil {
		t.Fatal(err)
	}
	marker := "mimamori-test-" + strconv.Itoa(os.Getpid()) + "-asuser"
	for _, mode := range []string{"limit", "stop"} {
		c := exec.Command(bin, "-test.run=^TestExecuteCommandAsOtherUserFromNonRootAgent$", "-test.count=1")
		c.Dir = "/"
		c.Env = append(os.Environ(), asUserChildEnv+"="+osUser+":"+mode+":"+marker+"-"+mode)
		c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(agent.UID), Gid: uint32(agent.GID)}}
		out, err := c.CombinedOutput()
		left := processesLeft(0, marker+"-"+mode, marker+"-"+mode+"-child")
		for limit := time.Now().Add(5 * time.Second); len(left) > 0 && time.Now().Before(limit); left = processesLeft(0, marker+"-"+mode, marker+"-"+mode+"-child") {
			time.Sleep(10 * time.Millisecond)
		}
		for _, p := range left {
			_ = syscall.Kill(p.pid, syscall.SIGKILL)
		}
		var res struct {
			Result *CommandResultReport
			Log    []string
		}
		line := ""
		for _, l := range strings.Split(string(out), "\n") {
			if rest, found := strings.CutPrefix(l, "ASUSER-RESULT "); found {
				line = rest
			}
		}
		if err != nil || line == "" || json.Unmarshal([]byte(line), &res) != nil || res.Result == nil {
			t.Fatalf("%s: the run as %s failed: %v\n%s", mode, agentName, err, out)
		}
		if len(left) > 0 {
			t.Fatalf("%s: nothing of the command of %s may be left: %v (result %+v, log %q)", mode, osUser, left, res.Result, res.Log)
		}
		want := ReasonTimeout
		if mode == "stop" {
			want = ReasonInterrupted
		}
		if res.Result.Kind != ResultError || res.Result.Reason != want || !strings.HasPrefix(string(res.Result.Output), osUser+"\n") {
			t.Fatalf("%s: result %+v %q (log %q)", mode, res.Result, res.Result.Output, res.Log)
		}
	}
}

// asUserChild is the part of TestExecuteCommandAsOtherUserFromNonRootAgent
// that runs as the agent's user: spec is "USER:MODE:NAME". It prints the
// result and the log on a line of its own.
func asUserChild(spec string) {
	parts := strings.SplitN(spec, ":", 3)
	osUser, mode, name := parts[0], parts[1], parts[2]
	cmd := testCommand("id -un; trap '' TERM; (exec -a " + name + "-child sleep 41) & exec -a " + name + " sleep 42")
	var stop chan struct{}
	if mode == "limit" {
		cmd.TimeoutSec = 1
	} else {
		stop, _ = closeWhen(processNamed(name))
	}
	var logged []string
	r := ExecuteCommand(CommandExecInput{OSUser: osUser, WorkDir: "/", Command: cmd, KillGrace: 300 * time.Millisecond, Stop: stop,
		Logf: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }})
	b, _ := json.Marshal(struct {
		Result *CommandResultReport
		Log    []string
	}{r, logged})
	fmt.Printf("ASUSER-RESULT %s\n", b)
}

// copyExecutable copies the file src to dst with mode 0755.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
