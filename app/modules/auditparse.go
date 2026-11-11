package modules

import (
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AuditEvent is one parsed audit event (all records with the same serial).
type AuditEvent struct {
	Time     int64
	Serial   string
	Arch     string
	Syscall  int
	Success  bool
	Exit     int64
	EUID     int
	Exe      string
	Comm     string
	CWD      string
	Paths    []auditPath
	Argv     []string
	SockAddr string
	HasKey   bool
}

type auditPath struct {
	Name     string
	NameType string
}

var recordRe = regexp.MustCompile(`^type=(\S+) msg=audit\((\d+)\.(\d+):(\d+)\):\s?(.*)$`)

// parseFields splits "k=v k2="quoted v" ..." into a map, decoding the hex
// encoding auditd uses for values that contain spaces or special characters.
func parseFields(s string) map[string]string {
	out := map[string]string{}
	i := 0
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			break
		}
		key := s[i : i+eq]
		i += eq + 1
		var val string
		if i < len(s) && s[i] == '"' {
			end := strings.IndexByte(s[i+1:], '"')
			if end < 0 {
				val = s[i+1:]
				i = len(s)
			} else {
				val = s[i+1 : i+1+end]
				i += end + 2
			}
		} else {
			end := strings.IndexByte(s[i:], ' ')
			if end < 0 {
				val = s[i:]
				i = len(s)
			} else {
				val = s[i : i+end]
				i += end
			}
			// SYSCALL records may end with an enriched part after \x1d.
			if j := strings.IndexByte(val, 0x1d); j >= 0 {
				val = val[:j]
			}
			if hexEncodedField(key) && isHexString(val) && len(val)%2 == 0 && len(val) >= 2 {
				if b, err := hex.DecodeString(val); err == nil && isMostlyPrintable(b) {
					val = string(b)
				}
			}
		}
		if _, exists := out[key]; !exists {
			out[key] = val
		}
	}
	return out
}

// hexEncodedField reports whether auditd may hex-encode the field (untrusted
// strings). Numeric fields such as syscall=59 must never be decoded.
func hexEncodedField(key string) bool {
	switch key {
	case "exe", "comm", "name", "cwd", "proctitle", "key":
		return true
	}
	if len(key) >= 2 && key[0] == 'a' {
		_, err := strconv.Atoi(key[1:])
		return err == nil
	}
	return false
}

func isHexString(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func isMostlyPrintable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 && c != 0 && c != '\t' && c != 0x01 {
			return false
		}
	}
	return true
}

// ParseAuditRaw parses "ausearch --raw" output into events keyed by serial.
func ParseAuditRaw(data string) []*AuditEvent {
	events := map[string]*AuditEvent{}
	var order []string
	for _, line := range strings.Split(data, "\n") {
		m := recordRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		typ, sec, serial, rest := m[1], m[2], m[4], m[5]
		id := sec + ":" + serial
		ev, ok := events[id]
		if !ok {
			ts, _ := strconv.ParseInt(sec, 10, 64)
			ev = &AuditEvent{Time: ts, Serial: id, EUID: -1}
			events[id] = ev
			order = append(order, id)
		}
		f := parseFields(rest)
		switch typ {
		case "SYSCALL":
			ev.Arch = f["arch"]
			ev.Syscall, _ = strconv.Atoi(f["syscall"])
			ev.Success = f["success"] == "yes"
			ev.Exit, _ = strconv.ParseInt(f["exit"], 10, 64)
			if v, err := strconv.Atoi(f["euid"]); err == nil {
				ev.EUID = v
			}
			ev.Exe = f["exe"]
			ev.Comm = f["comm"]
			for _, k := range strings.Split(f["key"], "\x01") {
				if k == AuditKey {
					ev.HasKey = true
				}
			}
		case "CWD":
			ev.CWD = f["cwd"]
		case "PATH":
			ev.Paths = append(ev.Paths, auditPath{Name: f["name"], NameType: f["nametype"]})
		case "EXECVE":
			argc, _ := strconv.Atoi(f["argc"])
			if argc > 64 {
				argc = 64
			}
			for i := 0; i < argc; i++ {
				if v, ok := f[fmt.Sprintf("a%d", i)]; ok {
					ev.Argv = append(ev.Argv, v)
				}
			}
		case "SOCKADDR":
			ev.SockAddr = f["saddr"]
		}
	}
	out := make([]*AuditEvent, 0, len(order))
	for _, id := range order {
		out = append(out, events[id])
	}
	return out
}

// Syscall numbers needed for classification.
const (
	archX8664   = "c000003e"
	archI386    = "40000003"
	sysExecve64 = 59
	sysExecat64 = 322
	sysConnect  = 42
	sysExecve32 = 11
	sysExecat32 = 358
)

// classifyEvent returns the detection type of an event and its target.
func classifyEvent(ev *AuditEvent) (string, string, bool) {
	isExec := (ev.Arch == archX8664 && (ev.Syscall == sysExecve64 || ev.Syscall == sysExecat64)) ||
		(ev.Arch == archI386 && (ev.Syscall == sysExecve32 || ev.Syscall == sysExecat32))
	switch {
	case isExec:
		target := strings.Join(ev.Argv, " ")
		if target == "" {
			target = ev.Exe
		}
		return TypeExec, truncate(target, MaxTargetLength), true
	case ev.Arch == archX8664 && ev.Syscall == sysConnect:
		addr, ok := decodeSockAddr(ev.SockAddr)
		if !ok {
			return "", "", false
		}
		return TypeNetwork, addr, true
	case !ev.Success && (ev.Exit == -13 || ev.Exit == -1):
		return TypeDenied, truncate(eventPath(ev), MaxTargetLength), true
	case ev.Success:
		return TypeRead, truncate(eventPath(ev), MaxTargetLength), true
	}
	return "", "", false
}

func eventPath(ev *AuditEvent) string {
	name := ""
	for _, p := range ev.Paths {
		if p.NameType != "PARENT" && p.Name != "" && p.Name != "(null)" {
			name = p.Name
		}
	}
	if name == "" && len(ev.Paths) > 0 {
		name = ev.Paths[len(ev.Paths)-1].Name
	}
	if name != "" && !strings.HasPrefix(name, "/") && ev.CWD != "" {
		name = path.Join(ev.CWD, name)
	}
	return name
}

// decodeSockAddr decodes the hex sockaddr of a connect call. Unix sockets and
// other families are ignored (only network peers are reported).
func decodeSockAddr(h string) (string, bool) {
	b, err := hex.DecodeString(h)
	if err != nil || len(b) < 2 {
		return "", false
	}
	family := int(b[0]) | int(b[1])<<8
	switch family {
	case 2: // AF_INET
		if len(b) < 8 {
			return "", false
		}
		port := int(b[2])<<8 | int(b[3])
		return net.JoinHostPort(net.IP(b[4:8]).String(), strconv.Itoa(port)), true
	case 10: // AF_INET6
		if len(b) < 24 {
			return "", false
		}
		port := int(b[2])<<8 | int(b[3])
		return net.JoinHostPort(net.IP(b[8:24]).String(), strconv.Itoa(port)), true
	}
	return "", false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// AggregateAuditEvents groups events by (type, user, executable, target) and
// returns one detection per group with its count and first and last time.
func AggregateAuditEvents(events []*AuditEvent, c *RuleContent, users map[int]string) []DetectionReport {
	cats := map[string]string{
		TypeExec: c.Exec.Category, TypeDenied: c.Denied.Category,
		TypeRead: c.Read.Category, TypeNetwork: c.Network.Category,
	}
	enabled := map[string]bool{
		TypeExec: c.ExecEnabled(), TypeDenied: c.DeniedEnabled(),
		TypeRead: c.ReadEnabled(), TypeNetwork: c.NetworkEnabled(),
	}
	groups := map[string]*DetectionReport{}
	var keys []string
	for _, ev := range events {
		if !ev.HasKey {
			continue
		}
		typ, target, ok := classifyEvent(ev)
		if !ok || !enabled[typ] {
			continue
		}
		user := ""
		if ev.EUID >= 0 {
			user = NameOrID(users, ev.EUID)
		}
		k := typ + "\x00" + user + "\x00" + ev.Exe + "\x00" + target
		g, ok := groups[k]
		if !ok {
			g = &DetectionReport{Type: typ, Category: cats[typ], OSUser: user, Exe: ev.Exe, Target: target, FirstAt: ev.Time, LastAt: ev.Time}
			groups[k] = g
			keys = append(keys, k)
		}
		g.Count++
		if ev.Time < g.FirstAt {
			g.FirstAt = ev.Time
		}
		if ev.Time > g.LastAt {
			g.LastAt = ev.Time
		}
	}
	sort.Strings(keys)
	out := make([]DetectionReport, 0, len(keys))
	for _, k := range keys {
		out = append(out, *groups[k])
	}
	return out
}

// CollectAuditEvents reads the audit log from where the previous call ended
// using an ausearch checkpoint file. When the checkpoint cannot be used (for
// example the log was rotated away) it falls back to events since "since".
func CollectAuditEvents(checkpoint string, since time.Time) ([]*AuditEvent, error) {
	timeout := 120 * time.Second
	base := []string{"--input-logs", "--raw", "-k", AuditKey, "--checkpoint", checkpoint}
	first := false
	if _, err := os.Stat(checkpoint); err != nil {
		first = true
	}
	args := base
	if first {
		args = append(append([]string{}, base...), "-ts", "recent")
	}
	res, err := RunCommand(timeout, 256<<20, "ausearch", args...)
	if err != nil {
		return nil, err
	}
	switch res.ExitCode {
	case 0, 1: // 1: no matches
	case 10, 11, 12:
		// Invalid or unusable checkpoint: start again from "since".
		_ = os.Remove(checkpoint)
		ts := since
		if ts.IsZero() || time.Since(ts) > 24*time.Hour {
			ts = time.Now().Add(-10 * time.Minute)
		}
		lt := ts.Local()
		args = append(append([]string{}, base...), "-ts", lt.Format("01/02/2006"), lt.Format("15:04:05"))
		res, err = RunCommand(timeout, 256<<20, "ausearch", args...)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 && res.ExitCode != 1 {
			return nil, fmt.Errorf("ausearch exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
		}
	default:
		return nil, fmt.Errorf("ausearch exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return ParseAuditRaw(string(res.Stdout)), nil
}
