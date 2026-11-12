package modules

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// AuditdStatus is the state of auditd as reported by "auditctl -s" plus the
// suppression-rule check.
type AuditdStatus struct {
	Running         bool     `json:"running"`
	Enabled         int      `json:"enabled"`
	PID             int      `json:"pid"`
	Lost            int64    `json:"lost"`
	Backlog         int64    `json:"backlog"`
	BacklogLimit    int64    `json:"backlog_limit"`
	BacklogWaitTime int64    `json:"backlog_wait_time"`
	Failure         int      `json:"failure"`
	NeverRule       bool     `json:"never_rule"`
	HaltActions     []string `json:"halt_actions"`
}

// neverRuleLine is how auditctl -l prints the rule that disables syscall
// auditing for every task (default on Amazon Linux 2023 and others).
const neverRuleLine = "-a never,task"

// ReadAuditdStatus runs "auditctl -s" and parses it.
func ReadAuditdStatus() (*AuditdStatus, error) {
	res, err := RunSimple("auditctl", "-s")
	if err != nil {
		return nil, err
	}
	st := &AuditdStatus{}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "enabled":
			st.Enabled = int(v)
		case "failure":
			st.Failure = int(v)
		case "pid":
			st.PID = int(v)
		case "lost":
			st.Lost = v
		case "backlog":
			st.Backlog = v
		case "backlog_limit":
			st.BacklogLimit = v
		case "backlog_wait_time":
			st.BacklogWaitTime = v
		}
	}
	// auditctl reports pid 0 when no audit daemon is registered.
	st.Running = st.PID > 0
	return st, nil
}

// CheckAuditd verifies that the audit tools exist and auditd is running and
// enabled. Both the agent and the central's built-in agent call it at start.
func CheckAuditd() error {
	for _, tool := range []string{"auditctl", "ausearch"} {
		if _, err := LookupCommand(tool); err != nil {
			return fmt.Errorf("audit tool %s is not available: %w", tool, err)
		}
	}
	st, err := ReadAuditdStatus()
	if err != nil {
		return fmt.Errorf("auditctl -s failed: %w", err)
	}
	if !st.Running {
		return errors.New("auditd is not running")
	}
	if st.Enabled == 0 {
		return errors.New("auditing is disabled (auditctl -s reports enabled 0)")
	}
	if _, err := RunSimple("auditctl", "-l"); err != nil {
		return fmt.Errorf("auditctl -l failed: %w", err)
	}
	return nil
}

// HaltActions lists auditd.conf settings that would halt or single-user the
// host on a disk or error condition.
func HaltActions() []string {
	f, err := os.Open(AuditdConfFile)
	if err != nil {
		return nil
	}
	defer f.Close()
	keys := map[string]bool{
		"space_left_action": true, "admin_space_left_action": true,
		"disk_full_action": true, "disk_error_action": true,
		"overflow_action": true,
	}
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.ToLower(strings.TrimSpace(v))
		if keys[k] && (v == "halt" || v == "single") {
			out = append(out, k+" = "+v)
		}
	}
	return out
}

// ListAuditRules returns the loaded rules ("auditctl -l").
func ListAuditRules() ([]string, error) {
	res, err := RunSimple("auditctl", "-l")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(res.Stdout), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && l != "No rules" {
			out = append(out, l)
		}
	}
	return out, nil
}

func isOwnRule(line string) bool {
	return strings.Contains(line, "key="+AuditKey) || strings.HasSuffix(line, "-k "+AuditKey)
}

// ResolvedRule is a rule content after resolving names to ids and dropping
// missing elements.
type ResolvedRule struct {
	Content  *RuleContent
	UIDs     []int
	UserByID map[int]string
	Dirs     []string
	Exes     []string
	Warnings []string
}

// ResolveRule resolves users and checks directories and executables. Items
// marked "ignore" are dropped silently, items marked "warn" are dropped with
// a warning.
func ResolveRule(c *RuleContent) *ResolvedRule {
	byName, byUID := ReadPasswd()
	r := &ResolvedRule{Content: c, UserByID: byUID}
	for _, u := range c.Users {
		e, ok := byName[u.Value]
		if !ok {
			if u.IfMissing == IfMissingWarn {
				r.Warnings = append(r.Warnings, fmt.Sprintf("OS user %q does not exist", u.Value))
			}
			continue
		}
		r.UIDs = append(r.UIDs, e.UID)
	}
	sort.Ints(r.UIDs)
	r.UIDs = uniqInts(r.UIDs)
	for _, d := range c.Read.Dirs {
		if fi, err := os.Stat(d.Value); err != nil || !fi.IsDir() {
			if d.IfMissing == IfMissingWarn {
				r.Warnings = append(r.Warnings, fmt.Sprintf("directory %q does not exist", d.Value))
			}
			continue
		}
		r.Dirs = append(r.Dirs, d.Value)
	}
	for _, e := range c.Network.Executables {
		if fi, err := os.Stat(e.Value); err != nil || !fi.Mode().IsRegular() {
			if e.IfMissing == IfMissingWarn {
				r.Warnings = append(r.Warnings, fmt.Sprintf("executable %q does not exist", e.Value))
			}
			continue
		}
		r.Exes = append(r.Exes, e.Value)
	}
	if c.Writable.Base != "" {
		if fi, err := os.Stat(c.Writable.Base); err != nil || !fi.IsDir() {
			r.Warnings = append(r.Warnings, fmt.Sprintf("scan origin %q does not exist", c.Writable.Base))
		}
	}
	return r
}

func uniqInts(in []int) []int {
	out := in[:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// deniedSyscalls are the file operations whose permission failures are
// recorded by the "denied" type.
const deniedSyscalls = "open,openat,openat2,creat,truncate,unlink,unlinkat,rename,renameat,renameat2,mkdir,mkdirat,rmdir,chmod,fchmodat,chown,fchownat,lchown,link,linkat,symlink,symlinkat"

// deniedSyscallsAArch64 are the same operations on aarch64, which has only
// the *at forms (and truncate): every name exists in the aarch64 table of
// the audit package (ausyscall aarch64 <name>).
const deniedSyscallsAArch64 = "openat,openat2,truncate,unlinkat,renameat,renameat2,mkdirat,fchmodat,fchownat,linkat,symlinkat"

// GenerateAuditRules builds the auditd rule lines from the fixed templates
// below for the architecture this program is built for. Only values of the
// rule are substituted; nothing else can be added.
func GenerateAuditRules(r *ResolvedRule) []string {
	return generateAuditRules(r, runtime.GOARCH)
}

// generateAuditRules builds the rules for goarch. On arm64 (aarch64) only
// arch=b64 rules are generated, with the syscall names of aarch64; every
// other value gives the x86_64 rules (with b32 for the execution of 32-bit
// programs).
func generateAuditRules(r *ResolvedRule, goarch string) []string {
	execArchs, denied := []string{"b64", "b32"}, deniedSyscalls
	if goarch == "arm64" {
		execArchs, denied = []string{"b64"}, deniedSyscallsAArch64
	}
	c := r.Content
	var lines []string
	auid := ""
	if c.IsExcludeInteractive() {
		// Processes started by the system have no login uid; logins and cron
		// jobs (pam_loginuid) have one.
		auid = " -F auid=4294967295"
	}
	if len(r.UIDs) > 0 {
		for _, uid := range r.UIDs {
			if c.ExecEnabled() {
				for _, arch := range execArchs {
					lines = append(lines, fmt.Sprintf("-a always,exit -F arch=%s -S execve -S execveat -F euid=%d%s -k %s", arch, uid, auid, AuditKey))
				}
			}
			if c.DeniedEnabled() {
				for _, exit := range []string{"-EACCES", "-EPERM"} {
					lines = append(lines, fmt.Sprintf("-a always,exit -F arch=b64 -S %s -F exit=%s -F euid=%d%s -k %s", denied, exit, uid, auid, AuditKey))
				}
			}
			if c.ReadEnabled() {
				for _, d := range r.Dirs {
					lines = append(lines, fmt.Sprintf("-a always,exit -F arch=b64 -F dir=%s -F perm=r -F success=1 -F euid=%d%s -k %s", d, uid, auid, AuditKey))
				}
			}
		}
	}
	if c.NetworkEnabled() {
		for _, e := range r.Exes {
			lines = append(lines, fmt.Sprintf("-a always,exit -F arch=b64 -S connect -F exe=%s -k %s", e, AuditKey))
		}
	}
	return lines
}

// globalSettingLines override the OS defaults without editing them: never
// make processes wait when the backlog is full and never panic on failure.
func globalSettingLines() []string {
	return []string{
		fmt.Sprintf("-b %d", AuditBacklogLimit),
		"-f 1",
		"--backlog_wait_time 0",
	}
}

// auditRulesFile is AuditRulesFile; tests replace it.
var auditRulesFile = AuditRulesFile

// writeAuditRulesFile keeps the rules for the next boot in AuditRulesFile.
// The directory is never created: without it nothing is kept (the agent
// applies the rules again when it starts) and a warning is returned. An
// existing file is overwritten in place; with no rules the file is removed.
func writeAuditRulesFile(content []byte) (string, error) {
	if content == nil {
		if fi, err := os.Lstat(auditRulesFile); err == nil && fi.IsDir() {
			return "", fmt.Errorf("%s is a directory", auditRulesFile)
		}
		if err := os.Remove(auditRulesFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return "", nil
	}
	if fi, err := os.Stat(filepath.Dir(auditRulesFile)); err != nil || !fi.IsDir() {
		return fmt.Sprintf("%s does not exist; the rules are not kept for the next boot (the agent applies them again when it starts)", filepath.Dir(auditRulesFile)), nil
	}
	return "", os.WriteFile(auditRulesFile, content, 0o600)
}

// neverCancelLine deletes the suppression rule (neverRuleLine).
const neverCancelLine = "-d never,task"

// auditRulesFileContent is the content of the rules file: the global
// settings and the rules, and, when a distribution rule file adds the
// suppression rule, the line that cancels it. That line comes last:
// augenrules loads the files in name order, so at boot the suppression is
// loaded before and cancelled at the end. Without rules there is no file
// (nil).
func auditRulesFileContent(lines []string, cancelNever bool) []byte {
	if len(lines) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("## Generated by " + AppName + ". Do not edit: the agent rewrites this file.\n")
	for _, l := range globalSettingLines() {
		b.WriteString(l + "\n")
	}
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	if cancelNever {
		b.WriteString("## Cancels the distribution's rule that suppresses syscall auditing.\n")
		b.WriteString(neverCancelLine + "\n")
	}
	return []byte(b.String())
}

// neverRuleInRulesDir reports whether another rule file in the directory of
// the rules file (the files augenrules loads) adds the suppression rule
// ("-a never,task", in either order of the list and the action).
func neverRuleInRulesDir() bool {
	dir := filepath.Dir(auditRulesFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".rules") || p == auditRulesFile {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(data), "\n") {
			f := strings.Fields(l)
			if len(f) == 2 && (f[0] == "-a" || f[0] == "-A") && (f[1] == "never,task" || f[1] == "task,never") {
				return true
			}
		}
	}
	return false
}

// RemoveAuditRules removes what the agent applied: the rules file and the
// loaded rules carrying this system's key. Other rules and the global
// settings are not touched, and a suppression rule cancelled by the agent is
// not loaded again: the distribution's rules take effect again at the next
// boot (or reload of the rules).
func RemoveAuditRules() error {
	st, err := ReadAuditdStatus()
	if err != nil {
		return err
	}
	if st.Enabled == 2 {
		return errors.New("audit rules are immutable (enabled 2); reboot is required to change them")
	}
	if _, err := writeAuditRulesFile(nil); err != nil {
		return err
	}
	if err := deleteOwnRules(); err != nil {
		return err
	}
	rules, err := ListAuditRules()
	if err != nil {
		return err
	}
	for _, l := range rules {
		if isOwnRule(l) {
			return fmt.Errorf("a rule with key %s is still loaded: %s", AuditKey, l)
		}
	}
	return nil
}

// ApplyAuditRules loads the generated rules and global settings, replacing
// only rules that carry this system's key. On any failure the previous state
// is restored and an error is returned. The returned warnings do not prevent
// the rules from being applied.
func ApplyAuditRules(lines []string) ([]string, error) {
	before, err := ListAuditRules()
	if err != nil {
		return nil, err
	}
	prevStatus, err := ReadAuditdStatus()
	if err != nil {
		return nil, err
	}
	if prevStatus.Enabled == 2 {
		return nil, errors.New("audit rules are immutable (enabled 2); reboot is required to change them")
	}
	var prevOwn []string
	for _, l := range before {
		if isOwnRule(l) {
			prevOwn = append(prevOwn, l)
		}
	}
	prevFile, prevFileErr := os.ReadFile(auditRulesFile)
	// fileWritten (the rules file was written) and neverRemoved (the
	// suppression rule was deleted) tell the rollback what this run changed.
	fileWritten, neverRemoved := false, false

	rollback := func(cause error) ([]string, error) {
		// failed counts the steps of the restoration that did not succeed,
		// so the message never claims more than what was restored.
		failed := 0
		try := func(args ...string) {
			if _, err := RunSimple("auditctl", args...); err != nil {
				failed++
			}
		}
		_ = deleteOwnRules()
		for _, l := range prevOwn {
			try(splitRule(l)...)
		}
		if neverRemoved {
			try(strings.Fields(neverRuleLine)...)
		}
		try("-b", strconv.FormatInt(prevStatus.BacklogLimit, 10))
		try("-f", strconv.Itoa(prevStatus.Failure))
		try("--backlog_wait_time", strconv.FormatInt(prevStatus.BacklogWaitTime, 10))
		switch {
		case prevFileErr == nil:
			if err := os.WriteFile(auditRulesFile, prevFile, 0o600); err != nil {
				failed++
			}
		case errors.Is(prevFileErr, os.ErrNotExist):
			if err := os.Remove(auditRulesFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				failed++
			}
		case fileWritten:
			// The previous content is unknown, so nothing is deleted or
			// written: the file keeps the new rules.
			return nil, fmt.Errorf("applying audit rules failed and the previous state was restored except %s, which could not be read before the change (%v): %w", auditRulesFile, prevFileErr, cause)
		}
		if failed > 0 {
			return nil, fmt.Errorf("applying audit rules failed and the previous state could not be restored completely (%d steps failed; check the loaded rules with auditctl -l): %w", failed, cause)
		}
		return nil, fmt.Errorf("applying audit rules failed and the previous state was restored: %w", cause)
	}

	// Keep for the next boot (augenrules loads rules.d in name order, so
	// this file comes after the OS defaults and overrides their settings).
	// The cancelling line is written only when a rule file loaded at boot
	// adds the suppression rule: without it the line would fail at boot, and
	// auditctl -R stops at the first failing line (augenrules puts the -e
	// setting after it).
	content := auditRulesFileContent(lines, neverRuleInRulesDir())
	var warnings []string
	if w, err := writeAuditRulesFile(content); err != nil {
		return rollback(err)
	} else if w != "" {
		warnings = append(warnings, w)
	} else {
		fileWritten = true
	}

	// Apply live.
	if err := deleteOwnRules(); err != nil {
		return rollback(err)
	}
	for _, l := range globalSettingLines() {
		if _, err := RunSimple("auditctl", strings.Fields(l)...); err != nil {
			return rollback(err)
		}
	}
	for _, l := range before {
		if l == neverRuleLine {
			if _, err := RunSimple("auditctl", "-d", "never,task"); err != nil {
				return rollback(err)
			}
			neverRemoved = true
		}
	}
	for _, l := range lines {
		if _, err := RunSimple("auditctl", splitRule(l)...); err != nil {
			return rollback(err)
		}
	}

	// Verify.
	after, err := ListAuditRules()
	if err != nil {
		return rollback(err)
	}
	n := 0
	for _, l := range after {
		if isOwnRule(l) {
			n++
		}
		if l == neverRuleLine {
			return rollback(errors.New("the rule suppressing all syscall auditing is still loaded"))
		}
	}
	if n != len(lines) {
		return rollback(fmt.Errorf("expected %d loaded rules with key %s, found %d", len(lines), AuditKey, n))
	}
	stNow, err := ReadAuditdStatus()
	if err != nil {
		return rollback(err)
	}
	if stNow.Failure != 1 || stNow.BacklogWaitTime != 0 {
		return rollback(fmt.Errorf("global settings not effective (failure %d, backlog_wait_time %d)", stNow.Failure, stNow.BacklogWaitTime))
	}
	return warnings, nil
}

// deleteOwnRules removes every loaded rule carrying this system's key, one by
// one, so other rules are never touched.
func deleteOwnRules() error {
	rules, err := ListAuditRules()
	if err != nil {
		return err
	}
	for _, l := range rules {
		if !isOwnRule(l) || !strings.HasPrefix(l, "-a ") {
			continue
		}
		args := splitRule(l)
		args[0] = "-d"
		if _, err := RunSimple("auditctl", args...); err != nil {
			return err
		}
	}
	return nil
}

// splitRule splits a rule line into auditctl arguments.
func splitRule(l string) []string {
	return strings.Fields(l)
}
