package modules

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// statLine returns a line of /proc/<pid>/stat with the given command name,
// state, process group and number of threads.
func statLine(pid int, comm string, state byte, pgrp, threads int) string {
	return fmt.Sprintf("%d (%s) %c 1 %d %d 0 -1 4194560 119 0 0 0 0 0 0 0 20 0 %d 0 2275 1234567 89\n", pid, comm, state, pgrp, pgrp, threads)
}

// fakeProc writes a process list: one directory per process with its stat.
func fakeProc(t *testing.T, stats map[int]string) string {
	t.Helper()
	dir := t.TempDir()
	for pid, line := range stats {
		d := filepath.Join(dir, strconv.Itoa(pid))
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "stat"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Entries of /proc that are not processes.
	for _, name := range []string{"self", "sys", "0"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestParseProcStat: the state, the process group and the number of threads
// are taken after the last ')' (the command name may hold spaces and
// parentheses); a line that lacks them is not understood.
func TestParseProcStat(t *testing.T) {
	for _, c := range []struct {
		line    string
		ok      bool
		state   byte
		pgrp    int
		threads int
	}{
		{statLine(5041, "sleep", 'Z', 5039, 1), true, 'Z', 5039, 1},
		{statLine(7, "a b", 'S', 7, 3), true, 'S', 7, 3},
		{statLine(8, "x) Z 1 99 99 (y", 'R', 8, 1), true, 'R', 8, 1},
		{statLine(9, "(", 'X', 4, 1), true, 'X', 4, 1},
		{"5041 (sleep) Z 1 5039 5039 0 -1", false, 0, 0, 0},
		{"5041 sleep Z 1 5039", false, 0, 0, 0},
		{"5041 (sleep) ZZ 1 5039 5039 0 -1 4194560 119 0 0 0 0 0 0 0 20 0 1 0", false, 0, 0, 0},
		{"5041 (sleep) Z 1 abc 5039 0 -1 4194560 119 0 0 0 0 0 0 0 20 0 1 0", false, 0, 0, 0},
		{"5041 (sleep) Z 1 5039 5039 0 -1 4194560 119 0 0 0 0 0 0 0 20 0 n 0", false, 0, 0, 0},
		{"", false, 0, 0, 0},
	} {
		st, ok := parseProcStat([]byte(c.line))
		if ok != c.ok || (ok && (st.state != c.state || st.pgrp != c.pgrp || st.threads != c.threads)) {
			t.Errorf("%q: %+v %v", c.line, st, ok)
		}
	}
	// The fields are where the kernel puts them.
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Skip("no /proc")
	}
	st, ok := parseProcStat(b)
	if !ok || st.pgrp != syscall.Getpgrp() || st.threads < 1 || st.ended() {
		t.Fatalf("/proc/self/stat %q: %+v %v (process group %d)", b, st, ok, syscall.Getpgrp())
	}
}

// TestProcStatEnded: a zombie (Z) or a dead process (X, x) has ended; a
// process shown as Z whose other threads still run has not.
func TestProcStatEnded(t *testing.T) {
	for _, c := range []struct {
		state   byte
		threads int
		ended   bool
	}{
		{'Z', 1, true}, {'X', 1, true}, {'x', 1, true}, {'Z', 0, true},
		{'Z', 2, false}, {'R', 1, false}, {'S', 1, false}, {'D', 1, false}, {'T', 1, false}, {'t', 1, false}, {'I', 1, false},
	} {
		if got := (procStat{state: c.state, threads: c.threads}).ended(); got != c.ended {
			t.Errorf("state %c with %d thread(s): ended %v", c.state, c.threads, got)
		}
	}
}

// TestProcGroupState: the members of a group are found by the process group
// of their stat; one that has not ended is returned (the hint first), only
// ended members mean ended, no member or no list means unknown.
func TestProcGroupState(t *testing.T) {
	dir := fakeProc(t, map[int]string{
		100: statLine(100, "bash", 'S', 100, 1),
		101: statLine(101, "sleep", 'Z', 100, 1),
		200: statLine(200, "sleep", 'Z', 200, 1),
		201: statLine(201, "sleep", 'X', 200, 1),
		300: statLine(300, "app", 'Z', 300, 2),
		400: statLine(400, "a) S 1 500 500 (b", 'Z', 400, 1),
		500: "garbage",
		600: statLine(600, "bash", 'S', 600, 1),
		601: statLine(601, "sleep", 'S', 600, 1),
	})
	for _, c := range []struct {
		pgid, hint int
		state      groupState
		member     int
	}{
		{100, 0, groupRunning, 100},
		{200, 0, groupEnded, 0},
		{300, 0, groupRunning, 300},
		{400, 0, groupEnded, 0},
		{500, 0, groupUnknown, 0},
		{700, 0, groupUnknown, 0},
		// The hint is looked at first; a hint that ended or left the group
		// is not taken.
		{600, 601, groupRunning, 601},
		{200, 200, groupEnded, 0},
		{100, 601, groupRunning, 100},
		{100, 999, groupRunning, 100},
	} {
		state, member := procGroupState(dir, c.pgid, c.hint)
		if state != c.state || member != c.member {
			t.Errorf("group %d (hint %d): state %d member %d, want %d %d", c.pgid, c.hint, state, member, c.state, c.member)
		}
	}
	if state, member := procGroupState(filepath.Join(dir, "missing"), 100, 100); state != groupUnknown || member != 0 {
		t.Fatalf("a list that cannot be read: %d %d", state, member)
	}
}

// TestGroupProbeDecides: signal 0 answering ESRCH means no process; else the
// list decides (a member that has not ended: yes; only ended members: no);
// without members in the list the answer of signal 0 counts (answered or
// EPERM: yes).
func TestGroupProbeDecides(t *testing.T) {
	saved := procDir
	t.Cleanup(func() {
		procDir = saved
		killProcessGroup = syscall.Kill
	})
	procDir = fakeProc(t, map[int]string{
		10: statLine(10, "bash", 'S', 10, 1),
		20: statLine(20, "sleep", 'Z', 20, 1),
		21: statLine(21, "sleep", 'Z', 20, 1),
		30: statLine(30, "app", 'Z', 30, 4),
	})
	missing := filepath.Join(procDir, "missing")
	for _, c := range []struct {
		pgid   int
		signal error
		dir    string
		exists bool
	}{
		{10, syscall.ESRCH, procDir, false},
		{10, nil, procDir, true},
		{10, syscall.EPERM, procDir, true},
		{20, nil, procDir, false},
		{20, syscall.EPERM, procDir, false},
		{30, nil, procDir, true},
		{40, nil, procDir, true},
		{40, syscall.EPERM, procDir, true},
		{40, syscall.EINVAL, procDir, false},
		{20, nil, missing, true},
		{20, syscall.ESRCH, missing, false},
	} {
		var asked []int
		killProcessGroup = func(pid int, sig syscall.Signal) error {
			if sig != 0 {
				t.Fatalf("signal %d sent to %d", sig, pid)
			}
			asked = append(asked, pid)
			return c.signal
		}
		dir := procDir
		procDir = c.dir
		got := groupExists(c.pgid)
		procDir = dir
		if got != c.exists || len(asked) != 1 || asked[0] != -c.pgid {
			t.Errorf("group %d, signal 0 %v, list %s: exists %v (asked %v), want %v", c.pgid, c.signal, filepath.Base(c.dir), got, asked, c.exists)
		}
	}
}

// TestGroupProbeRealZombie: a process that ended and was not reaped yet (a
// zombie; here a child of the test, which reaps it at the end) still answers
// signal 0 to its group, but the group has no process any more; once it is
// reaped signal 0 answers ESRCH.
func TestGroupProbeRealZombie(t *testing.T) {
	p := startSurvivor(t)
	g := &groupProbe{pgid: p.Pid}
	if !g.exists() || g.member != p.Pid {
		t.Fatalf("a running group: member %d", g.member)
	}
	if err := syscall.Kill(p.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if st, ok := readProcStat(procDir, p.Pid); ok && st.ended() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the killed child did not become a zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(-p.Pid, 0); err != nil {
		t.Fatalf("signal 0 to a group of a zombie: %v", err)
	}
	if g.exists() || g.member != 0 {
		t.Fatalf("a group of a zombie counts as existing (member %d)", g.member)
	}
	if _, err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	if groupExists(p.Pid) {
		t.Fatal("a group that is gone counts as existing")
	}
}

// TestHostBoot: the boot of the host is the kernel's boot id when it can be
// read, else the boot time; the stored form reads back to the same boot.
// Two boots are the same by their boot ids when both have one (a step of the
// clock does not matter), else by their boot times within BootTimeSlack.
func TestHostBoot(t *testing.T) {
	dir := t.TempDir()
	idFile := filepath.Join(dir, "boot_id")
	if err := os.WriteFile(idFile, []byte("4a00dcb9-ac2b-4353-be44-9968c34afdcb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bootIDFile = idFile
	bootTime = func() (int64, error) { return 1_800_000_000, nil }
	t.Cleanup(func() { bootIDFile, bootTime = "/proc/sys/kernel/random/boot_id", sysBootTime })
	b, err := currentBoot()
	if err != nil || b.id != "4a00dcb9-ac2b-4353-be44-9968c34afdcb" || b.time != 1_800_000_000 {
		t.Fatalf("current boot %+v %v", b, err)
	}
	if s := b.String(); s != "1800000000 4a00dcb9-ac2b-4353-be44-9968c34afdcb" || parseHostBoot(s) != b {
		t.Fatalf("stored form %q reads back as %+v", s, parseHostBoot(s))
	}
	bootIDFile = filepath.Join(dir, "missing")
	if b, err := currentBoot(); err != nil || b.id != "" || b.String() != "1800000000" || parseHostBoot(b.String()) != b {
		t.Fatalf("without a boot id: %+v %v", b, err)
	}
	bootTime = func() (int64, error) { return 0, syscall.ENOSYS }
	if _, err := currentBoot(); err == nil {
		t.Fatal("a boot without id and time")
	}
	id, other := "4a00dcb9-ac2b-4353-be44-9968c34afdcb", "5b11edca-bd3c-5464-cf55-0a79d45bfedc"
	slack := int64(BootTimeSlack / time.Second)
	for _, c := range []struct {
		a, b hostBoot
		same bool
	}{
		{hostBoot{id, 1000}, hostBoot{id, 1000}, true},
		{hostBoot{id, 1000}, hostBoot{id, 1000 + 7200}, true},
		{hostBoot{id, 1000}, hostBoot{other, 1000}, false},
		{hostBoot{id, 1000}, hostBoot{"", 1000 + slack}, true},
		{hostBoot{"", 1000}, hostBoot{other, 1000 - slack - 1}, false},
		{hostBoot{"", 1000}, hostBoot{"", 1000 + slack}, true},
		{hostBoot{"", 1000}, hostBoot{"", 1000 + slack + 1}, false},
		{hostBoot{}, hostBoot{}, false},
		{parseHostBoot(""), hostBoot{"", 1000}, false},
		{parseHostBoot("x y"), hostBoot{"y", 5}, true},
	} {
		if got := sameBoot(c.a, c.b); got != c.same {
			t.Errorf("sameBoot(%+v, %+v) = %v", c.a, c.b, got)
		}
	}
}
