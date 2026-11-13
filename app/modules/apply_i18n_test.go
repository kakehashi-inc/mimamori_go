package modules

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mimamori/app/models"
)

// oldJoinMax is how the agent joined the problems of a rejected rule before
// the problems had codes: the first n, then "and N more", joined with "; ".
func oldJoinMax(s []string, n int) string {
	if len(s) > n {
		s = append(s[:n:n], fmt.Sprintf("and %d more", len(s)-n))
	}
	return strings.Join(s, "; ")
}

// checkCoded checks that a coded text has the code, renders as the English
// text and renders the same after a JSON round trip (as the central and the
// browser receive it).
func checkCoded(t *testing.T, name string, m *Msg, code, english string) {
	t.Helper()
	if m == nil {
		t.Fatalf("%s: no coded text", name)
	}
	if m.Code != code || m.Text() != english {
		t.Fatalf("%s: code %q text %q, want %q %q", name, m.Code, m.Text(), code, english)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Msg
	if err := json.Unmarshal(b, &back); err != nil || back.Text() != english {
		t.Fatalf("%s: after JSON %q (%v)", name, back.Text(), err)
	}
}

// A rejected rule keeps the English message and carries the problems with
// their codes: the first five and "and N more".
func TestApplyValidationFailedMessage(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	n := 0
	for i, c := range englishRuleContents() {
		errs := c.Validate()
		if len(errs) == 0 {
			continue
		}
		n++
		rt.applyRule(&RuleAssignment{ID: 1, Version: i + 1, Name: "r", Content: *c}, false)
		want := "validation failed: " + oldJoinMax(errs, 5)
		if rt.lastApply.Status != "error" || rt.lastApply.Message != want {
			t.Fatalf("content %d: %+v, want %q", i, rt.lastApply, want)
		}
		checkCoded(t, fmt.Sprint("content ", i), rt.lastApply.MessageI18n, "apply.validation_failed", want)
		items, _ := rt.lastApply.MessageI18n.Params["problems"].([]any)
		if wantItems := min(len(errs), 5) + min(len(errs)-min(len(errs), 5), 1); len(items) != wantItems {
			t.Fatalf("content %d: %d items, want %d", i, len(items), wantItems)
		}
		if len(errs) > 5 {
			if last, ok := items[len(items)-1].(Msg); !ok || last.Code != "apply.and_more" {
				t.Fatalf("content %d: last item %#v", i, items[len(items)-1])
			}
		}
		// The stored result keeps both.
		var stored ApplyResult
		if err := json.Unmarshal([]byte(models.GetLocalState(rt.DB, stateApply)), &stored); err != nil || stored.Message != want {
			t.Fatalf("content %d: stored %+v (%v)", i, stored, err)
		}
		checkCoded(t, fmt.Sprint("stored content ", i), stored.MessageI18n, "apply.validation_failed", want)
	}
	if n < 3 {
		t.Fatalf("only %d rejected contents", n)
	}
}

// The warnings of an applied rule keep their English and carry their codes
// in the same order; the message counts them.
func TestApplyWarningsCarryCodes(t *testing.T) {
	fakeAuditctl(t, "1", nil)
	rt, done := openAgentDB(t)
	defer done()
	c := RuleContent{
		Users:    []Element{{Value: "no-such-user-mimamori", IfMissing: IfMissingWarn}},
		Read:     ReadSpec{Dirs: []Element{{Value: "/no/such/dir", IfMissing: IfMissingWarn}}},
		Network:  NetworkSpec{Executables: []Element{{Value: "/no/such/exe", IfMissing: IfMissingWarn}}},
		Writable: WritableSpec{Base: "/no/such/base"},
	}
	c.Normalize()
	if errs := c.Validate(); len(errs) > 0 {
		t.Fatalf("content: %v", errs)
	}
	rt.applyRule(&RuleAssignment{ID: 1, Version: 1, Name: "r", Content: c}, false)
	ap := rt.lastApply
	wantWarnings := []string{
		`OS user "no-such-user-mimamori" does not exist`,
		`directory "/no/such/dir" does not exist`,
		`executable "/no/such/exe" does not exist`,
		`scan origin "/no/such/base" does not exist`,
	}
	codes := []string{"apply.warn_user_missing", "apply.warn_dir_missing", "apply.warn_exe_missing", "apply.warn_scan_base_missing"}
	if ap.Status != "warning" || strings.Join(ap.Warnings, "|") != strings.Join(wantWarnings, "|") || len(ap.WarningsI18n) != len(codes) {
		t.Fatalf("result: %+v", ap)
	}
	for i := range codes {
		checkCoded(t, codes[i], &ap.WarningsI18n[i], codes[i], wantWarnings[i])
	}
	checkCoded(t, "message", ap.MessageI18n, "apply.applied_with_warnings", "applied with 4 warning(s)")

	// Without warnings: the count of audit rules.
	c = RuleContent{}
	c.Normalize()
	rt.applyRule(&RuleAssignment{ID: 1, Version: 2, Name: "r", Content: c}, false)
	if rt.lastApply.Status != "ok" || rt.lastApply.Warnings != nil || rt.lastApply.WarningsI18n != nil {
		t.Fatalf("result: %+v", rt.lastApply)
	}
	checkCoded(t, "message", rt.lastApply.MessageI18n, "apply.applied", "applied (0 audit rules)")
}

// The rules file: a missing directory is a warning, a directory in place of
// the file an error, both with their English as before.
func TestRulesFileMessages(t *testing.T) {
	dir := t.TempDir()
	prev := auditRulesFile
	t.Cleanup(func() { auditRulesFile = prev })
	auditRulesFile = filepath.Join(dir, "missing", "zz.rules")
	w, err := writeAuditRulesFile([]byte("x\n"))
	want := filepath.Join(dir, "missing") + " does not exist; the rules are not kept for the next boot (the agent applies them again when it starts)"
	if err != nil {
		t.Fatal(err)
	}
	checkCoded(t, "warning", w, "apply.warn_rules_dir_missing", want)

	auditRulesFile = filepath.Join(dir, "isdir")
	if err := os.Mkdir(auditRulesFile, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = writeAuditRulesFile(nil)
	m := AsMsg(err)
	checkCoded(t, "directory", &m, "apply.rules_file_dir", auditRulesFile+" is a directory")
}

// A failed removal reports the cause nested in the message; the English is
// the same as the joined text before.
func TestRemoveFailureMessage(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	_ = models.SetLocalState(rt.DB, stateApply, `{"rule_id":3,"version":2,"status":"ok"}`)
	rt = NewAgentRuntime(rt.DB, &fakeTransport{}, nil)
	cause := errors.Join(M("apply.immutable"), errors.New("auditctl: exit 1: busy"))
	rt.removeAudit = func(*AuditGlobals) error { return cause }
	rt.removeRule()
	want := "no rule is assigned, but removing the audit rules failed: " + cause.Error()
	if rt.lastApply.Status != "error" || rt.lastApply.Message != want {
		t.Fatalf("result: %+v", rt.lastApply)
	}
	checkCoded(t, "failed removal", rt.lastApply.MessageI18n, "apply.none_remove_failed", want)
	nested, _ := rt.lastApply.MessageI18n.Params["cause"].(Msg)
	if nested.Code != "common.lines" {
		t.Fatalf("cause: %#v", rt.lastApply.MessageI18n.Params["cause"])
	}

	rt.removeAudit = func(*AuditGlobals) error { return nil }
	rt.removeRule()
	checkCoded(t, "removed", rt.lastApply.MessageI18n, "apply.none_removed", "no rule is assigned; the audit rules were removed")
}

// "agent init --overwrite" leaves a pending result with a coded message.
// Its English names no command, because the screens show it.
func TestResetStoredRuleMessage(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	_ = models.SetLocalState(rt.DB, stateRule, `{"id":3,"version":2}`)
	if err := resetStoredRule(rt.DB); err != nil {
		t.Fatal(err)
	}
	var ap ApplyResult
	if err := json.Unmarshal([]byte(models.GetLocalState(rt.DB, stateApply)), &ap); err != nil {
		t.Fatal(err)
	}
	want := "the stored rule was cleared when the agent was initialized again; the next sync decides"
	if ap.Status != "pending" || ap.Message != want {
		t.Fatalf("result: %+v", ap)
	}
	checkCoded(t, "pending", ap.MessageI18n, "apply.cleared_by_init", want)
}

// The errors of the audit functions keep their English and carry codes.
func TestAuditErrorsCarryCodes(t *testing.T) {
	_, err := LookupCommand("no-such-command-mimamori")
	m := AsMsg(err)
	checkCoded(t, "lookup", &m, "common.command_not_in_path", `command "no-such-command-mimamori" not found in PATH`)

	fakeAuditctl(t, "2", nil)
	err = RemoveAuditRules(nil)
	m = AsMsg(err)
	checkCoded(t, "remove immutable", &m, "apply.immutable", "audit rules are immutable (enabled 2); reboot is required to change them")
	_, err = ApplyAuditRules([]string{ownConnectRule}, nil)
	m = AsMsg(err)
	checkCoded(t, "apply immutable", &m, "apply.immutable", "audit rules are immutable (enabled 2); reboot is required to change them")

	// A rule auditctl refuses: the previous state is restored and the
	// error of auditctl is nested as data.
	fakeAuditctl(t, "1", nil)
	_, err = ApplyAuditRules([]string{"-w /x -k " + AuditKey}, nil)
	m = AsMsg(err)
	if err == nil || m.Code != "apply.failed_restored" || m.Text() != err.Error() ||
		!strings.HasPrefix(err.Error(), "applying audit rules failed and the previous state was restored: auditctl -w /x") {
		t.Fatalf("refused rule: %v %#v", err, m)
	}
	if cause, _ := m.Params["cause"].(Msg); cause.Code != "" {
		t.Fatalf("cause of auditctl is data: %#v", m.Params["cause"])
	}

	// The audit tools: without ausearch in PATH.
	auditctl, err := LookupCommand("auditctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(auditctl))
	err = CheckAuditd()
	m = AsMsg(err)
	checkCoded(t, "check", &m, "auditd.tool_missing", `audit tool ausearch is not available: command "ausearch" not found in PATH`)
}

// The problems of a command set the agent refuses are reported in English
// and with their codes in the same order.
func TestCommandApplyProblemsCarryCodes(t *testing.T) {
	rt, done := openAgentDB(t)
	defer done()
	defs := &CommandDefinitions{Signature: "s"}
	for i, s := range englishCommandSets() {
		defs.Sets = append(defs.Sets, CommandSetDef{ID: int64(i + 1), Revision: 1, CommandSetSpec: *s})
	}
	problems := rt.acceptCommandDefinitions(defs)
	if len(problems) < 3 {
		t.Fatalf("problems: %+v", problems)
	}
	for _, p := range problems {
		d := defs.Sets[p.SetID-1]
		want := d.Validate(0)
		if strings.Join(p.Problems, "|") != strings.Join(want, "|") || len(p.ProblemsI18n) != len(want) {
			t.Fatalf("set %d: %+v, want %v", p.SetID, p, want)
		}
		for i, pi := range p.ProblemsI18n {
			if pi.Code == "" || pi.Text() != want[i] {
				t.Fatalf("set %d problem %d: %#v", p.SetID, i, pi)
			}
		}
	}
}
