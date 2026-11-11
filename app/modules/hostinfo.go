package modules

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ServiceInfo is a running web server or PHP-FPM process group.
type ServiceInfo struct {
	Software  string   `json:"software"`
	Process   string   `json:"process"`
	Users     []string `json:"users"`
	Processes int      `json:"processes"`
}

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
	Services             []ServiceInfo   `json:"services"`
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

// ReadMachineID returns the host's machine id.
func ReadMachineID() (string, error) {
	b, err := os.ReadFile(MachineIDFile)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", fmt.Errorf("%s is empty", MachineIDFile)
	}
	return id, nil
}

func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// procInfo is the part of /proc/<pid> the agent needs.
type procInfo struct {
	PID       int
	Comm      string
	EUID      int
	StartedAt int64
}

func bootTime() int64 {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

// listProcesses reads every process from /proc.
func listProcesses() []procInfo {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	btime := bootTime()
	const clkTck = 100 // USER_HZ on all supported platforms
	var out []procInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := "/proc/" + e.Name()
		comm, err := os.ReadFile(dir + "/comm")
		if err != nil {
			continue
		}
		p := procInfo{PID: pid, Comm: strings.TrimSpace(string(comm)), EUID: -1}
		if st, err := os.ReadFile(dir + "/status"); err == nil {
			for _, line := range strings.Split(string(st), "\n") {
				if v, ok := strings.CutPrefix(line, "Uid:"); ok {
					f := strings.Fields(v)
					if len(f) >= 2 {
						p.EUID, _ = strconv.Atoi(f[1])
					}
				}
			}
		}
		if stat, err := os.ReadFile(dir + "/stat"); err == nil {
			s := string(stat)
			if i := strings.LastIndexByte(s, ')'); i >= 0 {
				f := strings.Fields(s[i+1:])
				// Field 22 of stat is starttime; after the comm it is index 19.
				if len(f) > 19 {
					ticks, _ := strconv.ParseInt(f[19], 10, 64)
					p.StartedAt = btime + ticks/clkTck
				}
			}
		}
		out = append(out, p)
	}
	return out
}

func serviceSoftware(comm string) string {
	switch {
	case comm == "httpd" || comm == "apache2":
		return "apache"
	case comm == "nginx":
		return "nginx"
	case strings.HasPrefix(comm, "php-fpm"):
		return "php-fpm"
	case comm == "lighttpd":
		return "lighttpd"
	case comm == "caddy":
		return "caddy"
	case comm == "litespeed" || comm == "lshttpd" || comm == "openlitespeed":
		return "litespeed"
	}
	return ""
}

// HostInfoInput carries the agent state that goes into the host information.
type HostInfoInput struct {
	ScanBase       string
	Rule           *RuleRef
	LastApply      *ApplyResult
	TrustedCAPEMs  []string
	MonitoredUIDs  map[int]bool
	NeverRemovedAt int64
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

	procs := listProcesses()
	svc := map[string]*ServiceInfo{}
	svcUsers := map[string]map[string]bool{}
	for _, p := range procs {
		sw := serviceSoftware(p.Comm)
		if sw == "" {
			continue
		}
		k := sw + "\x00" + p.Comm
		s, ok := svc[k]
		if !ok {
			s = &ServiceInfo{Software: sw, Process: p.Comm}
			svc[k] = s
			svcUsers[k] = map[string]bool{}
		}
		s.Processes++
		if p.EUID >= 0 {
			svcUsers[k][NameOrID(users, p.EUID)] = true
		}
	}
	for k, s := range svc {
		for u := range svcUsers[k] {
			s.Users = append(s.Users, u)
		}
		sort.Strings(s.Users)
		hi.Services = append(hi.Services, *s)
	}
	sort.Slice(hi.Services, func(i, j int) bool {
		return hi.Services[i].Software+hi.Services[i].Process < hi.Services[j].Software+hi.Services[j].Process
	})

	// Processes not audited because they started while the suppressing rule
	// was in effect: service processes and processes of monitored users.
	for _, p := range procs {
		relevant := serviceSoftware(p.Comm) != "" || in.MonitoredUIDs[p.EUID]
		if !relevant {
			continue
		}
		if hi.Auditd.NeverRule || (in.NeverRemovedAt > 0 && p.StartedAt > 0 && p.StartedAt < in.NeverRemovedAt) {
			hi.Auditd.AffectedProcesses = append(hi.Auditd.AffectedProcesses, AffectedProcess{
				PID: p.PID, Comm: p.Comm, User: NameOrID(users, p.EUID), StartedAt: p.StartedAt,
			})
			if len(hi.Auditd.AffectedProcesses) >= 50 {
				break
			}
		}
	}

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
