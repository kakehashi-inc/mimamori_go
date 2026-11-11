package modules

import (
	"bufio"
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
}

// CollectHostInfo gathers the host information reported to the central.
func CollectHostInfo(in HostInfoInput) *HostInfo {
	hi := &HostInfo{CollectedAt: time.Now().Unix(), AgentVersion: AppVersion, ScanBase: in.ScanBase, Rule: in.Rule, LastApply: in.LastApply}
	hi.Hostname, _ = os.Hostname()
	hi.OSName, hi.OSVersion = ReadOSRelease()
	hi.Kernel = kernelRelease()
	_, users := ReadPasswd()
	groups := ReadGroups()

	if st, err := ReadAuditdStatus(); err == nil {
		hi.Auditd = *st
	}
	if rules, err := ListAuditRules(); err == nil {
		for _, l := range rules {
			if l == neverRuleLine {
				hi.Auditd.NeverRule = true
			}
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
	return hi
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
