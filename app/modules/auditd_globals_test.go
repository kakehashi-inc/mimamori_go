package modules

import (
	"errors"
	"os"
	"strings"
	"testing"

	"mimamori/app/models"
)

// globalsFromFake reads the global settings of the fake auditctl as
// ReadAuditdStatus reports them.
func globalsFromFake(t *testing.T) AuditGlobals {
	t.Helper()
	st, err := ReadAuditdStatus(nil)
	if err != nil {
		t.Fatal(err)
	}
	return globalsOf(st)
}

var fakeOriginalGlobals = AuditGlobals{BacklogLimit: 64, BacklogWaitTime: 15000, Failure: 2}

var appliedGlobals = AuditGlobals{BacklogLimit: AuditBacklogLimit, BacklogWaitTime: 0, Failure: 1}

const ownConnectRule = "-a always,exit -F arch=b64 -S connect -F exe=/usr/bin/curl -k " + AuditKey

// The first application records the global settings it changes, a later
// one keeps that record, and removing the rules when no rule is assigned
// sets them back and forgets the record; applying again records again.
func TestAuditGlobalsRecordedAndRestored(t *testing.T) {
	state, _ := fakeAuditctl(t, "1", nil)
	rt, done := openAgentDB(t)
	defer done()
	calls := 0
	record := func(g AuditGlobals) error {
		calls++
		return recordAuditGlobals(rt.DB, g)
	}

	if _, err := ApplyAuditRules([]string{ownConnectRule}, record); err != nil {
		t.Fatal(err)
	}
	if got := globalsFromFake(t); got != appliedGlobals {
		t.Fatalf("settings after apply: %+v", got)
	}
	if g := storedAuditGlobals(rt.DB.Read()); calls != 1 || g == nil || *g != fakeOriginalGlobals {
		t.Fatalf("record after the first application: %d %+v", calls, g)
	}
	// Applied again (rules of this system are loaded): no new record.
	if _, err := ApplyAuditRules([]string{ownConnectRule}, record); err != nil {
		t.Fatal(err)
	}
	if g := storedAuditGlobals(rt.DB.Read()); calls != 1 || *g != fakeOriginalGlobals {
		t.Fatalf("a later application must keep the record: %d %+v", calls, g)
	}

	// No rule assigned any more.
	rt.rule = &RuleAssignment{ID: 3, Version: 2, Name: "web"}
	rt.lastApply = &ApplyResult{RuleID: 3, Version: 2, Status: "ok"}
	rt.removeRule()
	if rt.lastApply.Status != ApplyStatusNone {
		t.Fatalf("removal: %+v", rt.lastApply)
	}
	if got := globalsFromFake(t); got != fakeOriginalGlobals {
		t.Fatalf("settings after removal: %+v", got)
	}
	if readFile(t, state) != "" || models.GetLocalState(rt.DB.Read(), stateAuditGlobals) != "" {
		t.Fatal("the rules and the record must be gone")
	}

	// Applying again later records again (the values of that time).
	if err := os.WriteFile(fakeSettings(state), []byte("failure 1\nbacklog_limit 320\nbacklog_wait_time 60000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyAuditRules([]string{ownConnectRule}, record); err != nil {
		t.Fatal(err)
	}
	want := AuditGlobals{BacklogLimit: 320, BacklogWaitTime: 60000, Failure: 1}
	if g := storedAuditGlobals(rt.DB.Read()); calls != 2 || g == nil || *g != want {
		t.Fatalf("record after applying again: %d %+v", calls, g)
	}
}

// A failure to set the settings back does not stop the removal of the
// rules; it is reported like other removal failures, the record stays and
// the next check tries again.
func TestAuditGlobalsRestoreFailure(t *testing.T) {
	state, _ := fakeAuditctl(t, "1", nil)
	rt, done := openAgentDB(t)
	defer done()
	if _, err := ApplyAuditRules([]string{ownConnectRule}, func(g AuditGlobals) error { return recordAuditGlobals(rt.DB, g) }); err != nil {
		t.Fatal(err)
	}
	failB := strings.TrimSuffix(fakeSettings(state), "settings") + "fail-b"
	if err := os.WriteFile(failB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rt.rule = &RuleAssignment{ID: 3, Version: 2, Name: "web"}
	rt.lastApply = &ApplyResult{RuleID: 3, Version: 2, Status: "ok"}
	rt.removeRule()
	if readFile(t, state) != "" {
		t.Fatal("the rules must be removed although the settings could not be set back")
	}
	if rt.lastApply.Status != "error" || !strings.Contains(rt.lastApply.Message, "setting the global audit settings back") {
		t.Fatalf("the failure must be reported: %+v", rt.lastApply)
	}
	if g := storedAuditGlobals(rt.DB.Read()); g == nil || *g != fakeOriginalGlobals || rt.rule == nil {
		t.Fatalf("the record must stay for the next try: %+v %+v", g, rt.rule)
	}
	// The other settings were set back all the same.
	if got := globalsFromFake(t); got.Failure != 2 || got.BacklogWaitTime != 15000 {
		t.Fatalf("the settings that could be set must be set back: %+v", got)
	}

	if err := os.Remove(failB); err != nil {
		t.Fatal(err)
	}
	rt.removeRule()
	if rt.lastApply.Status != ApplyStatusNone || storedAuditGlobals(rt.DB.Read()) != nil {
		t.Fatalf("the next try: %+v", rt.lastApply)
	}
	if got := globalsFromFake(t); got != fakeOriginalGlobals {
		t.Fatalf("settings after the next try: %+v", got)
	}
}

// Nothing is recorded when rules of this system are already applied (the
// values in effect are then its own), a failing record changes nothing, and
// without a record the removal leaves the settings alone.
func TestAuditGlobalsNotRecorded(t *testing.T) {
	_, _ = fakeAuditctl(t, "1", []string{ownConnectRule})
	recorded := false
	record := func(AuditGlobals) error { recorded = true; return nil }
	if _, err := ApplyAuditRules([]string{ownConnectRule}, record); err != nil || recorded {
		t.Fatalf("rules of this system loaded: %v %v", err, recorded)
	}

	// Only the rules file of this system exists.
	_, _ = fakeAuditctl(t, "1", nil)
	if err := os.WriteFile(auditRulesFile, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyAuditRules([]string{ownConnectRule}, record); err != nil || recorded {
		t.Fatalf("rules file of this system present: %v %v", err, recorded)
	}

	// A failing record: nothing is changed.
	state, calls := fakeAuditctl(t, "1", nil)
	_, err := ApplyAuditRules([]string{ownConnectRule}, func(AuditGlobals) error { return errors.New("disk full") })
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("a failing record must stop the application: %v", err)
	}
	if readFile(t, state) != "" || globalsFromFake(t) != fakeOriginalGlobals {
		t.Fatal("nothing may change when the record fails")
	}
	if _, err := os.Stat(auditRulesFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no rules file may be written when the record fails")
	}
	for _, l := range strings.Split(readFile(t, calls), "\n") {
		if l != "" && l != "-l" && l != "-s" {
			t.Fatalf("only reads are allowed when the record fails: %q", l)
		}
	}

	// Removal without a record: the settings stay.
	state, _ = fakeAuditctl(t, "1", []string{ownConnectRule})
	if err := os.WriteFile(fakeSettings(state), []byte("failure 1\nbacklog_limit 8192\nbacklog_wait_time 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAuditRules(nil); err != nil {
		t.Fatal(err)
	}
	if got := globalsFromFake(t); got != appliedGlobals {
		t.Fatalf("without a record the settings must stay: %+v", got)
	}
}

// The central's cleanup of the disabled built-in agent sets the recorded
// settings back and clears the record, also when only the record is left.
func TestClearStoredRuleRestoresGlobals(t *testing.T) {
	state, _ := fakeAuditctl(t, "1", nil)
	rt, done := openAgentDB(t)
	defer done()
	if _, err := ApplyAuditRules([]string{ownConnectRule}, func(g AuditGlobals) error { return recordAuditGlobals(rt.DB, g) }); err != nil {
		t.Fatal(err)
	}
	setLocalState(t, rt.DB, stateRule, `{"id":3,"version":2}`)
	setLocalState(t, rt.DB, stateApply, `{"rule_id":3,"version":2,"status":"ok"}`)
	if removed, err := ClearStoredRule(rt.DB); !removed || err != nil {
		t.Fatalf("clear: %v %v", removed, err)
	}
	if got := globalsFromFake(t); got != fakeOriginalGlobals || readFile(t, state) != "" {
		t.Fatalf("settings after the cleanup: %+v", got)
	}
	if models.GetLocalState(rt.DB.Read(), stateAuditGlobals) != "" {
		t.Fatal("the record must be cleared")
	}

	// Only the record is left (an earlier run removed the rest).
	_ = recordAuditGlobals(rt.DB, AuditGlobals{BacklogLimit: 320, BacklogWaitTime: 60000, Failure: 1})
	if removed, err := ClearStoredRule(rt.DB); !removed || err != nil {
		t.Fatalf("clear with only the record: %v %v", removed, err)
	}
	if got := globalsFromFake(t); got != (AuditGlobals{BacklogLimit: 320, BacklogWaitTime: 60000, Failure: 1}) {
		t.Fatalf("settings: %+v", got)
	}
	if removed, _ := ClearStoredRule(rt.DB); removed {
		t.Fatal("cleared twice")
	}
}

// The record survives what happens between the first application and the
// removal: a later application while nothing of this system is loaded (for
// example after "auditctl -D") does not replace it with the values this
// system set, and "agent init --overwrite" keeps it, so the removal after
// it still sets back the values from before the first application, once.
func TestAuditGlobalsKeptAcrossReapplyAndReinit(t *testing.T) {
	state, _ := fakeAuditctl(t, "1", nil)
	rt, done := openAgentDB(t)
	defer done()
	record := func(g AuditGlobals) error { return recordAuditGlobals(rt.DB, g) }
	if _, err := ApplyAuditRules([]string{ownConnectRule}, record); err != nil {
		t.Fatal(err)
	}
	// The loaded rules and the rules file disappear; the settings stay.
	if err := os.WriteFile(state, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(auditRulesFile); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyAuditRules([]string{ownConnectRule}, record); err != nil {
		t.Fatal(err)
	}
	if g := storedAuditGlobals(rt.DB.Read()); g == nil || *g != fakeOriginalGlobals {
		t.Fatalf("the record must keep the values from before the first application: %+v", g)
	}

	// agent init --overwrite between the application and the removal.
	setLocalState(t, rt.DB, stateRule, `{"id":3,"version":2}`)
	setLocalState(t, rt.DB, stateApply, `{"rule_id":3,"version":2,"status":"ok"}`)
	if err := rt.DB.Write(resetStoredRule); err != nil {
		t.Fatal(err)
	}
	if g := storedAuditGlobals(rt.DB.Read()); g == nil || *g != fakeOriginalGlobals {
		t.Fatalf("agent init must keep the record: %+v", g)
	}

	// The next sync has no rule: the runtime as it starts after the init.
	rt2 := NewAgentRuntime(rt.DB, &fakeTransport{fail: true}, nil)
	rt2.removeRule()
	if got := globalsFromFake(t); got != fakeOriginalGlobals || readFile(t, state) != "" {
		t.Fatalf("settings after the removal: %+v", got)
	}
	if storedAuditGlobals(rt.DB.Read()) != nil {
		t.Fatal("the record must be forgotten")
	}
	// Set back once: a later check changes nothing.
	if err := os.WriteFile(fakeSettings(state), []byte("failure 1\nbacklog_limit 999\nbacklog_wait_time 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt2.removeRule()
	if got := globalsFromFake(t); got != (AuditGlobals{BacklogLimit: 999, BacklogWaitTime: 1, Failure: 1}) {
		t.Fatalf("the settings were set back twice: %+v", got)
	}
}
