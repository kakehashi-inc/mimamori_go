package modules

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// LockHolder reports whether a process holds the lock file at path and, if
// so, its process id. It never modifies the file.
func LockHolder(path string) (pid int, running bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return 0, false, nil
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		return 0, false, err
	}
	b := make([]byte, 32)
	n, _ := f.Read(b)
	pid, err = strconv.Atoi(strings.TrimSpace(string(b[:n])))
	if err != nil || pid <= 0 {
		return 0, true, fmt.Errorf("lock %s is held but holds no process id", path)
	}
	return pid, true, nil
}

// StopProcess sends SIGTERM to the process holding the lock and waits until
// the lock is released (the process completes its current processing and
// saves its state first), at most timeout. It returns false when no process
// was running.
func StopProcess(lockPath string, timeout time.Duration) (bool, error) {
	pid, running, err := LockHolder(lockPath)
	if err != nil {
		return false, err
	}
	if !running {
		return false, nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.EPERM) {
			return true, NewExitErrorf(ExitAuth, "not permitted to signal process %d (run as the user running it)", pid)
		}
		return true, fmt.Errorf("sending SIGTERM to process %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if _, still, err := LockHolder(lockPath); err == nil && !still {
			return true, nil
		}
	}
	return true, NewExitErrorf(ExitTimeout, "process %d did not stop within %s", pid, timeout)
}

// procDir lists the processes of the host (replaced by tests).
var procDir = "/proc"

// groupProbe tells whether a process group still has a process that has not
// ended (exists). It remembers the member it last found running and looks at
// that one first, so that looking again and again at a group that keeps
// running reads one file of /proc each time.
type groupProbe struct {
	pgid   int
	member int // a member found running, 0 when none is known
}

// groupExists reports whether the process group pgid still has a process
// that has not ended (see groupProbe.exists).
func groupExists(pgid int) bool {
	return (&groupProbe{pgid: pgid}).exists()
}

// exists reports whether the group still has a process that has not ended.
// Signal 0 to the group answers ESRCH: it has none. Otherwise its members
// are looked for in /proc (procGroupState): a member that has not ended
// means yes; members found that all ended (zombies nobody reaped yet) mean
// no; when no member is found or /proc cannot be read, the answer of signal
// 0 counts (answered, or EPERM: the group belongs to another OS user). A
// zombie is never signalled nor waited for.
func (g *groupProbe) exists() bool {
	err := killProcessGroup(-g.pgid, 0)
	if errors.Is(err, syscall.ESRCH) {
		g.member = 0
		return false
	}
	state, member := procGroupState(procDir, g.pgid, g.member)
	g.member = member
	switch state {
	case groupRunning:
		return true
	case groupEnded:
		return false
	}
	return err == nil || errors.Is(err, syscall.EPERM)
}

// groupState is what the process list says about the members of a process
// group.
type groupState int

const (
	// groupUnknown: no member found, or the list could not be read.
	groupUnknown groupState = iota
	// groupRunning: a member has not ended.
	groupRunning
	// groupEnded: members found, and every one of them has ended.
	groupEnded
)

// procGroupState looks for the members of the process group pgid among the
// processes listed in dir (/proc): the state and the process group of each
// come from dir/<pid>/stat. The process hint (0: none) is looked at first.
// It returns the state of the group and a member that has not ended (0 when
// there is none). A process whose stat cannot be read or understood (it
// ended meanwhile) is not counted.
func procGroupState(dir string, pgid, hint int) (groupState, int) {
	if hint > 0 {
		if st, ok := readProcStat(dir, hint); ok && st.pgrp == pgid && !st.ended() {
			return groupRunning, hint
		}
	}
	d, err := os.Open(dir)
	if err != nil {
		return groupUnknown, 0
	}
	defer d.Close()
	found := false
	for {
		names, err := d.Readdirnames(256)
		for _, name := range names {
			pid, err := strconv.Atoi(name)
			if err != nil || pid <= 0 {
				continue
			}
			st, ok := readProcStat(dir, pid)
			if !ok || st.pgrp != pgid {
				continue
			}
			if !st.ended() {
				return groupRunning, pid
			}
			found = true
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A list read only in part says nothing about the rest.
			return groupUnknown, 0
		}
	}
	if found {
		return groupEnded, 0
	}
	return groupUnknown, 0
}

// procStat is what the program reads of /proc/<pid>/stat.
type procStat struct {
	state   byte
	pgrp    int
	threads int
}

// ended reports whether the process has ended: it is a zombie (Z) or dead
// (X; x on old kernels) and has no thread left that runs. (A process whose
// first thread ended while other threads run is shown as Z with more than
// one thread.)
func (s procStat) ended() bool {
	return (s.state == 'Z' || s.state == 'X' || s.state == 'x') && s.threads <= 1
}

// readProcStat reads and parses dir/<pid>/stat.
func readProcStat(dir string, pid int) (procStat, bool) {
	b, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, false
	}
	return parseProcStat(b)
}

// parseProcStat parses a line of /proc/<pid>/stat. The command name, the
// second field, is in parentheses and may itself hold spaces and
// parentheses, so the fields are counted after the last ')': the state (the
// third field of the line), the process group (the fifth) and the number of
// threads (the twentieth).
func parseProcStat(b []byte) (procStat, bool) {
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return procStat{}, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 18 || len(f[0]) != 1 {
		return procStat{}, false
	}
	pgrp, err := strconv.Atoi(f[2])
	if err != nil {
		return procStat{}, false
	}
	threads, err := strconv.Atoi(f[17])
	if err != nil {
		return procStat{}, false
	}
	return procStat{state: f[0][0], pgrp: pgrp, threads: threads}, true
}

// bootTime returns when the host booted (Unix seconds; sysBootTime, replaced
// by tests).
var bootTime = sysBootTime

// bootIDFile holds the kernel's identity of the current boot (replaced by
// tests).
var bootIDFile = "/proc/sys/kernel/random/boot_id"

// sysBootTime returns when the host booted (Unix seconds): the current time
// less the uptime of the kernel (sysinfo). Successive calls may differ by a
// second, and a step of the clock moves it.
func sysBootTime() (int64, error) {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		return 0, err
	}
	return time.Now().Unix() - int64(info.Uptime), nil
}

// hostBoot names one boot of the host: the boot id of the kernel (a new
// random value at every boot, which setting the clock does not change; empty
// when it cannot be read) and the boot time (bootTime; 0 when it cannot be
// found).
type hostBoot struct {
	id   string
	time int64
}

// currentBoot returns the current boot of the host. It fails only when
// neither the boot id nor the boot time can be found.
func currentBoot() (hostBoot, error) {
	var b hostBoot
	if raw, err := os.ReadFile(bootIDFile); err == nil {
		if f := strings.Fields(string(raw)); len(f) > 0 {
			b.id = f[0]
		}
	}
	t, err := bootTime()
	if err == nil {
		b.time = t
	}
	if b.id == "" && b.time <= 0 {
		return hostBoot{}, fmt.Errorf("the boot of the host cannot be found: %w", err)
	}
	return b, nil
}

// String is the stored form of the boot (command_executions.running_boot):
// the boot time in decimal, then a space and the boot id when it is known.
func (b hostBoot) String() string {
	s := strconv.FormatInt(b.time, 10)
	if b.id != "" {
		s += " " + b.id
	}
	return s
}

// parseHostBoot reads the stored form of a boot (the zero hostBoot for an
// empty or unreadable value).
func parseHostBoot(s string) hostBoot {
	f := strings.Fields(s)
	if len(f) == 0 {
		return hostBoot{}
	}
	var b hostBoot
	if t, err := strconv.ParseInt(f[0], 10, 64); err == nil && t > 0 {
		b.time = t
	}
	if len(f) > 1 {
		b.id = f[1]
	}
	return b
}

// sameBoot reports whether two boots are the same boot of the host: by the
// boot id when both have one, else by the boot time, which may differ by at
// most BootTimeSlack (the clock may have been set in between; a new boot
// lies at least the whole uptime of the previous one later).
func sameBoot(a, b hostBoot) bool {
	if a.id != "" && b.id != "" {
		return a.id == b.id
	}
	d := a.time - b.time
	if d < 0 {
		d = -d
	}
	return a.time > 0 && b.time > 0 && time.Duration(d)*time.Second <= BootTimeSlack
}
