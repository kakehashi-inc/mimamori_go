package modules

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mimamori/app/models"
)

// fakeAuditctl puts a fake auditctl first in PATH. It keeps the loaded rules
// in a file (one rule per line, as "auditctl -l" prints them), reports
// "enabled" as given, keeps the global settings in the file fakeSettings
// names (set with -b, -f and --backlog_wait_time; -b fails while the file
// "fail-b" exists next to it) and records every call. The rules file of this
// system is redirected into the test directory.
func fakeAuditctl(t *testing.T, enabled string, loaded []string) (rulesState, calls string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(filepath.Join(dir, "rules.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	rulesState, calls = filepath.Join(dir, "loaded"), filepath.Join(dir, "calls")
	content := ""
	for _, l := range loaded {
		content += l + "\n"
	}
	if err := os.WriteFile(rulesState, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fakeSettings(rulesState), []byte("failure 2\nbacklog_limit 64\nbacklog_wait_time 15000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
echo "$*" >> 'CALLS'
set_value() {
	if [ "$1" = backlog_limit ] && [ -e 'FAILB' ]; then exit 1; fi
	grep -v "^$1 " 'SETTINGS' > 'SETTINGS.tmp'; echo "$1 $2" >> 'SETTINGS.tmp'; mv 'SETTINGS.tmp' 'SETTINGS'
}
case "$1" in
-s) printf 'enabled ENABLED\npid 123\n'; cat 'SETTINGS' ;;
-l) if grep -q . 'STATE'; then cat 'STATE'; else echo "No rules"; fi ;;
-d) shift; line="-a $*"; grep -vxF -- "$line" 'STATE' > 'STATE.tmp'; mv 'STATE.tmp' 'STATE' ;;
-a) echo "$*" >> 'STATE' ;;
-b) set_value backlog_limit "$2" ;;
-f) set_value failure "$2" ;;
--backlog_wait_time) set_value backlog_wait_time "$2" ;;
*) exit 1 ;;
esac
`
	script = strings.NewReplacer("CALLS", calls, "STATE", rulesState, "ENABLED", enabled,
		"SETTINGS", fakeSettings(rulesState), "FAILB", filepath.Join(dir, "fail-b")).Replace(script)
	if err := os.WriteFile(filepath.Join(bin, "auditctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := auditRulesFile
	auditRulesFile = filepath.Join(dir, "rules.d", "zz-mimamori.rules")
	t.Cleanup(func() { auditRulesFile = prev })
	return rulesState, calls
}

// fakeSettings is the file of the fake auditctl that holds the global
// settings, next to the loaded rules.
func fakeSettings(rulesState string) string {
	return filepath.Join(filepath.Dir(rulesState), "settings")
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Removing the applied rules deletes only the loaded rules with the key and
// the rules file; other rules stay.
func TestRemoveAuditRules(t *testing.T) {
	own1 := "-a always,exit -F arch=b64 -S execve,execveat -F euid=48 -F key=" + AuditKey
	own2 := "-a always,exit -F arch=b64 -S connect -F exe=/usr/bin/curl -F key=" + AuditKey
	other := "-w /etc/passwd -p wa -k identity"
	state, _ := fakeAuditctl(t, "1", []string{own1, other, own2})
	if err := os.WriteFile(auditRulesFile, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAuditRules(nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, state); got != other+"\n" {
		t.Fatalf("loaded rules after removal: %q", got)
	}
	if _, err := os.Stat(auditRulesFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the rules file must be removed: %v", err)
	}
}

// When a rule file loaded at boot adds the suppression rule, which applying
// the rules cancelled, removing the rules loads it again; without such a
// file, or when it is still loaded, it is not added.
func TestRemoveAuditRulesRestoresNeverRule(t *testing.T) {
	own := "-a always,exit -F arch=b64 -S connect -F exe=/usr/bin/curl -F key=" + AuditKey
	other := "-w /etc/passwd -p wa -k identity"

	// No distribution file with the rule: nothing is added.
	state, calls := fakeAuditctl(t, "1", []string{own, other})
	if err := RemoveAuditRules(nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, state); got != other+"\n" || strings.Contains(readFile(t, calls), "never") {
		t.Fatalf("without the distribution rule nothing may be added: %q", got)
	}

	// The distribution file adds it and the agent had cancelled it.
	state, _ = fakeAuditctl(t, "1", []string{own, other})
	dist := filepath.Join(filepath.Dir(auditRulesFile), "10-base-config.rules")
	if err := os.WriteFile(dist, []byte("-D\n-a task,never\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAuditRules(nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, state); got != other+"\n"+neverRuleLine+"\n" {
		t.Fatalf("the suppression rule must be loaded again: %q", got)
	}

	// Still loaded: not added twice.
	state, calls = fakeAuditctl(t, "1", []string{neverRuleLine, own})
	if err := os.WriteFile(filepath.Join(filepath.Dir(auditRulesFile), "10-base-config.rules"), []byte("-a never,task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAuditRules(nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, state); got != neverRuleLine+"\n" || strings.Contains(readFile(t, calls), "-a never") {
		t.Fatalf("a loaded suppression rule must not be added again: %q", got)
	}
}

// Immutable rules cannot be removed: nothing is touched.
func TestRemoveAuditRulesImmutable(t *testing.T) {
	own := "-a always,exit -F arch=b64 -S connect -F exe=/usr/bin/curl -F key=" + AuditKey
	state, _ := fakeAuditctl(t, "2", []string{own})
	if err := os.WriteFile(auditRulesFile, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAuditRules(nil); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("immutable rules: %v", err)
	}
	if readFile(t, state) != own+"\n" || readFile(t, auditRulesFile) != "x\n" {
		t.Fatal("nothing may be touched when the rules are immutable")
	}
}

// A sync without a rule removes what the agent applied, clears the stored
// rule and result and reports "none"; with nothing applied nothing is
// removed.
func TestSyncWithoutRuleRemovesAuditRules(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	rt.Transport = &fakeTransport{}
	removed := 0
	var fail error
	rt.removeAudit = func(*AuditGlobals) error {
		removed++
		return fail
	}

	// Nothing applied: nothing to remove.
	rt.sync()
	if removed != 0 || rt.lastApply != nil {
		t.Fatalf("an agent without a rule must not touch audit rules: %d %+v", removed, rt.lastApply)
	}

	ra := &RuleAssignment{ID: 3, Version: 2, Name: "web"}
	b, _ := json.Marshal(ra)
	_ = models.SetLocalState(rt.DB, stateRule, string(b))
	_ = models.SetLocalState(rt.DB, stateApply, `{"rule_id":3,"version":2,"status":"ok"}`)
	rt = NewAgentRuntime(rt.DB, &fakeTransport{}, nil)
	rt.removeAudit = func(*AuditGlobals) error {
		removed++
		return fail
	}

	// A failure keeps the rule, reports the error once and is tried again.
	fail = errors.New("auditctl failed")
	rt.sync()
	rt.sync()
	if removed != 2 || rt.rule == nil || rt.lastApply.Status != "error" || !strings.Contains(rt.lastApply.Message, "auditctl failed") {
		t.Fatalf("failed removal: %d %+v %+v", removed, rt.rule, rt.lastApply)
	}

	fail = nil
	rt.sync()
	if removed != 3 || rt.rule != nil || rt.lastApply == nil || rt.lastApply.Status != ApplyStatusNone || rt.lastApply.RuleID != 0 {
		t.Fatalf("removal: %d %+v %+v", removed, rt.rule, rt.lastApply)
	}
	if models.GetLocalState(rt.DB, stateRule) != "" || models.GetLocalState(rt.DB, stateApply) != "" {
		t.Fatal("the stored rule and application result must be cleared")
	}
	// The report carries the result, so the central shows nothing applied.
	tr := rt.Transport.(*fakeTransport)
	rt.report(nil)
	if len(tr.reports) != 1 || tr.reports[0].Apply == nil || tr.reports[0].Apply.Status != ApplyStatusNone {
		t.Fatalf("report: %+v", tr.reports)
	}
	// Done once.
	rt.sync()
	if removed != 3 {
		t.Fatalf("removed again: %d", removed)
	}
	// After a restart nothing is held any more.
	rt = NewAgentRuntime(rt.DB, &fakeTransport{}, nil)
	if rt.rule != nil || rt.lastApply != nil {
		t.Fatalf("state after restart: %+v %+v", rt.rule, rt.lastApply)
	}
}

// "agent init --overwrite" clears the stored rule and leaves a pending
// result, so that the next sync without a rule removes the audit rules.
func TestResetStoredRule(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	if err := resetStoredRule(rt.DB); err != nil || models.GetLocalState(rt.DB, stateApply) != "" {
		t.Fatalf("nothing stored: nothing to do (%v)", err)
	}
	_ = models.SetLocalState(rt.DB, stateRule, `{"id":3,"version":2}`)
	_ = models.SetLocalState(rt.DB, stateApply, `{"rule_id":3,"version":2,"status":"ok"}`)
	if err := resetStoredRule(rt.DB); err != nil {
		t.Fatal(err)
	}
	rt = NewAgentRuntime(rt.DB, &fakeTransport{}, nil)
	if rt.rule != nil || rt.lastApply == nil || rt.lastApply.Status != "pending" {
		t.Fatalf("after reset: %+v %+v", rt.rule, rt.lastApply)
	}
	removed := 0
	rt.removeAudit = func(*AuditGlobals) error { removed++; return nil }
	rt.sync()
	if removed != 1 || rt.lastApply.Status != ApplyStatusNone {
		t.Fatalf("the next sync without a rule must remove the audit rules: %d %+v", removed, rt.lastApply)
	}
}

// The central clears the records of a disabled built-in agent only when
// there is one; without it no audit command runs.
func TestClearStoredRule(t *testing.T) {
	own := "-a always,exit -F arch=b64 -S connect -F exe=/usr/bin/curl -F key=" + AuditKey
	state, calls := fakeAuditctl(t, "1", []string{own})
	rt, done := openAgentDB(t)
	defer done()
	if removed, err := ClearStoredRule(rt.DB); removed || err != nil {
		t.Fatalf("no record: %v %v", removed, err)
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("without a record no audit command may run")
	}
	_ = models.SetLocalState(rt.DB, stateRule, `{"id":3,"version":2}`)
	_ = models.SetLocalState(rt.DB, stateApply, `{"rule_id":3,"version":2,"status":"ok"}`)
	if err := os.WriteFile(auditRulesFile, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := ClearStoredRule(rt.DB); !removed || err != nil {
		t.Fatalf("record: %v %v", removed, err)
	}
	if readFile(t, state) != "" {
		t.Fatal("the rules with the key must be removed")
	}
	if _, err := os.Stat(auditRulesFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the rules file must be removed")
	}
	if models.GetLocalState(rt.DB, stateRule) != "" || models.GetLocalState(rt.DB, stateApply) != "" {
		t.Fatal("the record must be cleared")
	}
	// Once: the next call finds no record.
	if removed, _ := ClearStoredRule(rt.DB); removed {
		t.Fatal("cleared twice")
	}
}

// The rules file cancels the suppression rule at its end when it had to be
// cancelled or a distribution file adds it; without rules there is no file.
func TestAuditRulesFileCancelsNeverRule(t *testing.T) {
	lines := []string{"-a always,exit -F arch=b64 -S connect -F exe=/usr/bin/curl -k " + AuditKey}
	plain := string(auditRulesFileContent(lines, false))
	if strings.Contains(plain, "never") {
		t.Fatalf("no cancelling line expected: %q", plain)
	}
	got := strings.Split(strings.TrimSuffix(string(auditRulesFileContent(lines, true)), "\n"), "\n")
	if got[len(got)-1] != "-d never,task" || got[len(got)-3] != lines[0] {
		t.Fatalf("the cancelling line must come last: %q", got)
	}
	if auditRulesFileContent(nil, true) != nil {
		t.Fatal("no rules: no file")
	}

	_, _ = fakeAuditctl(t, "1", nil)
	dir := filepath.Dir(auditRulesFile)
	if neverRuleInRulesDir() {
		t.Fatal("empty directory")
	}
	// Our own file and files that are not rule files do not count.
	_ = os.WriteFile(auditRulesFile, []byte("-a never,task\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "x.rules.bak"), []byte("-a never,task\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "10-base.rules"), []byte("## -a never,task\n-D\n"), 0o600)
	if neverRuleInRulesDir() {
		t.Fatal("only other rule files count")
	}
	for _, l := range []string{"-a never,task", "-a task,never", "  -A  task,never  "} {
		_ = os.WriteFile(filepath.Join(dir, "audit.rules"), []byte("-D\n"+l+"\n"), 0o600)
		if !neverRuleInRulesDir() {
			t.Fatalf("%q in a distribution file", l)
		}
	}
}

// Rules for arm64 name only syscalls of aarch64 and only arch=b64; the
// x86_64 rules are unchanged.
func TestGenerateAuditRulesArm64(t *testing.T) {
	c := &RuleContent{Users: []Element{{Value: "apache"}}, Exec: CategoryOnly{Category: "alert"}, Denied: CategoryOnly{Category: "alert"},
		Read:    ReadSpec{Category: "alert", Dirs: []Element{{Value: "/etc/ssh"}}},
		Network: NetworkSpec{Executables: []Element{{Value: "/usr/bin/curl"}}, Category: "alert"}}
	r := &ResolvedRule{Content: c, UIDs: []int{48}, Dirs: []string{"/etc/ssh"}, Exes: []string{"/usr/bin/curl"}}
	x86 := generateAuditRules(r, "amd64")
	want := []string{
		"-a always,exit -F arch=b64 -S execve -S execveat -F euid=48 -F auid=4294967295 -k mimamori",
		"-a always,exit -F arch=b32 -S execve -S execveat -F euid=48 -F auid=4294967295 -k mimamori",
		"-a always,exit -F arch=b64 -S " + deniedSyscalls + " -F exit=-EACCES -F euid=48 -F auid=4294967295 -k mimamori",
		"-a always,exit -F arch=b64 -S " + deniedSyscalls + " -F exit=-EPERM -F euid=48 -F auid=4294967295 -k mimamori",
		"-a always,exit -F arch=b64 -F dir=/etc/ssh -F perm=r -F success=1 -F euid=48 -F auid=4294967295 -k mimamori",
		"-a always,exit -F arch=b64 -S connect -F exe=/usr/bin/curl -k mimamori",
	}
	if strings.Join(x86, "\n") != strings.Join(want, "\n") {
		t.Fatalf("x86_64 rules changed:\n%s", strings.Join(x86, "\n"))
	}
	arm := generateAuditRules(r, "arm64")
	wantArm := []string{
		"-a always,exit -F arch=b64 -S execve -S execveat -F euid=48 -F auid=4294967295 -k mimamori",
		"-a always,exit -F arch=b64 -S openat,openat2,truncate,unlinkat,renameat,renameat2,mkdirat,fchmodat,fchownat,linkat,symlinkat -F exit=-EACCES -F euid=48 -F auid=4294967295 -k mimamori",
		"-a always,exit -F arch=b64 -S openat,openat2,truncate,unlinkat,renameat,renameat2,mkdirat,fchmodat,fchownat,linkat,symlinkat -F exit=-EPERM -F euid=48 -F auid=4294967295 -k mimamori",
		"-a always,exit -F arch=b64 -F dir=/etc/ssh -F perm=r -F success=1 -F euid=48 -F auid=4294967295 -k mimamori",
		"-a always,exit -F arch=b64 -S connect -F exe=/usr/bin/curl -k mimamori",
	}
	if strings.Join(arm, "\n") != strings.Join(wantArm, "\n") {
		t.Fatalf("arm64 rules:\n%s", strings.Join(arm, "\n"))
	}
	// Names that do not exist on aarch64 (ausyscall aarch64 <name> fails).
	missing := map[string]bool{}
	for _, name := range []string{"open", "creat", "unlink", "rename", "mkdir", "rmdir", "chmod", "chown", "lchown", "link", "symlink"} {
		missing[name] = true
	}
	for _, l := range arm {
		f := strings.Fields(l)
		for i := range f {
			if f[i] == "-S" {
				for _, name := range strings.Split(f[i+1], ",") {
					if missing[name] {
						t.Fatalf("%s does not exist on aarch64: %s", name, l)
					}
				}
			}
			if f[i] == "arch=b32" {
				t.Fatalf("no b32 rules on arm64: %s", l)
			}
		}
	}
}

const sampleAuditAArch64 = `type=SYSCALL msg=audit(1700000000.100:10): arch=c00000b7 syscall=221 success=yes exit=0 a0=55 ppid=1 pid=200 auid=4294967295 uid=48 gid=48 euid=48 comm="curl" exe="/usr/bin/curl" key="mimamori"
type=EXECVE msg=audit(1700000000.100:10): argc=2 a0="curl" a1="http://example.com"
type=PATH msg=audit(1700000000.100:10): item=0 name="/usr/bin/curl" nametype=NORMAL
type=SYSCALL msg=audit(1700000001.100:11): arch=c00000b7 syscall=281 success=yes exit=0 euid=48 comm="curl" exe="/usr/bin/curl" key="mimamori"
type=EXECVE msg=audit(1700000001.100:11): argc=2 a0="curl" a1="http://example.com"
type=SYSCALL msg=audit(1700000002.000:12): arch=c00000b7 syscall=56 success=no exit=-13 euid=48 comm="php-fpm" exe=2F7573722F7362696E2F7068702D66706D key="mimamori"
type=CWD msg=audit(1700000002.000:12): cwd="/var/www"
type=PATH msg=audit(1700000002.000:12): item=0 name="/root/" nametype=PARENT
type=PATH msg=audit(1700000002.000:12): item=1 name="secret.txt" nametype=NORMAL
type=SYSCALL msg=audit(1700000003.000:13): arch=c00000b7 syscall=203 success=no exit=-115 euid=0 comm="ffmpeg" exe="/opt/ffmpeg/bin/ffmpeg" key="mimamori"
type=SOCKADDR msg=audit(1700000003.000:13): saddr=020001BBC00002010000000000000000
type=SYSCALL msg=audit(1700000003.500:15): arch=c00000b7 syscall=221 success=no exit=-2 euid=48 comm="setpriv" exe="/usr/bin/setpriv" key="mimamori"
type=SYSCALL msg=audit(1700000003.600:16): arch=c00000b7 syscall=221 success=no exit=-13 euid=48 comm="sh" exe="/usr/bin/sh" key="mimamori"
type=PATH msg=audit(1700000003.600:16): item=0 name="/opt/secret/tool" nametype=NORMAL
`

// aarch64 records are classified like the x86_64 ones.
func TestAuditParsingAArch64(t *testing.T) {
	events := ParseAuditRaw(sampleAuditAArch64)
	if len(events) != 6 {
		t.Fatalf("events: %d", len(events))
	}
	c := &RuleContent{Users: []Element{{Value: "apache"}}, Exec: CategoryOnly{Category: "alert"}, Denied: CategoryOnly{Category: "normal"},
		Network: NetworkSpec{Executables: []Element{{Value: "/opt/ffmpeg/bin/ffmpeg"}}, Category: "alert"}}
	got := AggregateAuditEvents(events, c, map[int]string{48: "apache", 0: "root"})
	byTarget := map[string]DetectionReport{}
	for _, d := range got {
		byTarget[d.Type+" "+d.Target] = d
	}
	if e := byTarget[TypeExec+" curl http://example.com"]; e.Count != 2 || e.OSUser != "apache" || e.FirstAt != 1700000000 || e.LastAt != 1700000001 {
		t.Fatalf("execve and execveat: %+v", got)
	}
	if d := byTarget[TypeDenied+" /var/www/secret.txt"]; d.Exe != "/usr/sbin/php-fpm" || d.Category != "normal" {
		t.Fatalf("denied openat: %+v", got)
	}
	if _, ok := byTarget[TypeDenied+" /opt/secret/tool"]; !ok {
		t.Fatalf("an execution refused by permissions is a denied operation: %+v", got)
	}
	if _, ok := byTarget[TypeNetwork+" 192.0.2.1:443"]; !ok {
		t.Fatalf("connect: %+v", got)
	}
	if len(got) != 4 {
		t.Fatalf("the failed execve (not found) must be dropped: %+v", got)
	}
}
