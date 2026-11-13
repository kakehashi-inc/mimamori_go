package modules

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// DirectoryInfo is one directory of the scan-origin structure.
type DirectoryInfo struct {
	Path  string `json:"path"`
	Owner string `json:"owner"`
	Group string `json:"group"`
	Mode  string `json:"mode"`
	Depth int    `json:"depth"`
}

// TrustedCA is a CA certificate trusted by the agent.
type TrustedCA struct {
	Fingerprint string `json:"fingerprint"`
	NotAfter    int64  `json:"not_after"`
}

// RuleRef identifies an applied rule version.
type RuleRef struct {
	ID      int64 `json:"id"`
	Version int   `json:"version"`
}

// Kinds of IncompleteScan.
const (
	ScanKindWritable  = "writable"
	ScanKindExecFiles = "exec_files"
)

// IncompleteScan is a file scan whose pass is split over several checks
// because one check visits at most Limit entries: the next check continues
// at ResumeAt. StartedAt is the start of the pass (Unix seconds).
type IncompleteScan struct {
	Kind      string `json:"kind"`
	ResumeAt  string `json:"resume_at"`
	Limit     int    `json:"limit"`
	StartedAt int64  `json:"started_at"`
}

// HostInfo is the host information reported every cycle.
type HostInfo struct {
	CollectedAt          int64           `json:"collected_at"`
	Hostname             string          `json:"hostname"`
	OSName               string          `json:"os_name"`
	OSVersion            string          `json:"os_version"`
	Kernel               string          `json:"kernel"`
	AgentVersion         string          `json:"agent_version"`
	Auditd               AuditdStatus    `json:"auditd"`
	ScanBase             string          `json:"scan_base"`
	Directories          []DirectoryInfo `json:"directories"`
	DirectoriesTruncated bool            `json:"directories_truncated"`
	Rule                 *RuleRef        `json:"rule"`
	LastApply            *ApplyResult    `json:"last_apply"`
	TrustedCAs           []TrustedCA     `json:"trusted_cas"`
	// IncompleteScans lists the file scans whose pass is not complete yet.
	IncompleteScans []IncompleteScan `json:"incomplete_scans"`
	// OSUser is the OS user the agent runs as (the default OS user of new
	// command sets). TimeZone and UTCOffsetSec describe the host's local
	// time, in which recurring command set schedules are interpreted.
	OSUser       string `json:"os_user"`
	TimeZone     string `json:"time_zone"`
	UTCOffsetSec int    `json:"utc_offset_sec"`
}

// ReadOSRelease returns NAME and VERSION_ID from /etc/os-release.
func ReadOSRelease() (string, string) {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return "", ""
	}
	defer f.Close()
	var name, ver string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		switch k {
		case "NAME":
			name = v
		case "VERSION_ID":
			ver = v
		}
	}
	return name, ver
}

// kernelRelease returns the kernel release from the uname system call.
func kernelRelease() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return ""
	}
	b := make([]byte, 0, len(u.Release))
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// HostInfoInput carries the agent state that goes into the host information.
type HostInfoInput struct {
	ScanBase      string
	Rule          *RuleRef
	LastApply     *ApplyResult
	TrustedCAPEMs []string
	// IncompleteScans is reported as it is.
	IncompleteScans []IncompleteScan
	// Stop is the agent's stop: closed, it ends the audit tools that are
	// running (nil: never).
	Stop <-chan struct{}
}

// CollectHostInfo gathers the host information reported to the central. When
// the stop ended or kept from starting one of its audit tools, it returns
// ErrToolStopped and no host information (it would claim that auditd is not
// running).
func CollectHostInfo(in HostInfoInput) (*HostInfo, error) {
	hi := &HostInfo{CollectedAt: time.Now().Unix(), AgentVersion: AppVersion, ScanBase: in.ScanBase, Rule: in.Rule, LastApply: in.LastApply,
		IncompleteScans: in.IncompleteScans}
	hi.Hostname, _ = os.Hostname()
	hi.OSName, hi.OSVersion = ReadOSRelease()
	hi.Kernel = kernelRelease()
	_, users := ReadPasswd()
	groups := ReadGroups()
	hi.OSUser = NameOrID(users, os.Geteuid())
	hi.TimeZone, hi.UTCOffsetSec = HostTimeZone(time.Now())

	st, err := ReadAuditdStatus(in.Stop)
	if errors.Is(err, ErrToolStopped) {
		return nil, err
	}
	if err == nil {
		hi.Auditd = *st
	}
	rules, err := ListAuditRules(in.Stop)
	if errors.Is(err, ErrToolStopped) {
		return nil, err
	}
	for _, l := range rules {
		if l == neverRuleLine {
			hi.Auditd.NeverRule = true
		}
	}
	hi.Auditd.HaltActions = HaltActions()

	if in.ScanBase != "" {
		hi.Directories, hi.DirectoriesTruncated = directoryStructure(in.ScanBase, users, groups)
	}
	for _, p := range in.TrustedCAPEMs {
		if c, err := ParseCertPEM(p); err == nil {
			hi.TrustedCAs = append(hi.TrustedCAs, TrustedCA{Fingerprint: Fingerprint(c), NotAfter: c.NotAfter.Unix()})
		}
	}
	return hi, nil
}

// directoryStructure lists the directories under base down to DirectoryDepth
// levels, without following links or leaving the file system.
func directoryStructure(base string, users, groups map[int]string) ([]DirectoryInfo, bool) {
	rootInfo, err := os.Lstat(base)
	if err != nil || !rootInfo.IsDir() {
		return nil, false
	}
	rootDev := statT(rootInfo).Dev
	var out []DirectoryInfo
	truncated := false
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if d != nil && d.IsDir() && p != base {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fs.SkipDir
		}
		st, _ := info.Sys().(*syscall.Stat_t)
		if st == nil || st.Dev != rootDev {
			return fs.SkipDir
		}
		depth := len(relComponents(base, p))
		if len(out) >= MaxDirectoryEntries {
			truncated = true
			return filepath.SkipAll
		}
		out = append(out, DirectoryInfo{
			Path: p, Owner: NameOrID(users, int(st.Uid)), Group: NameOrID(groups, int(st.Gid)),
			Mode: info.Mode().String(), Depth: depth,
		})
		if depth >= DirectoryDepth {
			return fs.SkipDir
		}
		return nil
	})
	return out, truncated
}

// HostTimeZone returns the name (abbreviation, such as "JST") and the UTC
// offset of the host's local time at t.
func HostTimeZone(t time.Time) (string, int) {
	return t.In(time.Local).Zone()
}
