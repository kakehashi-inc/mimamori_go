package modules

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mimamori/app/models"
)

// failingAuditctl puts an auditctl first in PATH that fails every call, so
// that every application of audit rules fails before anything is changed.
func failingAuditctl(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "auditctl"), []byte("#!/bin/sh\necho 'auditctl failed' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := auditRulesFile
	auditRulesFile = filepath.Join(bin, "zz-mimamori.rules")
	t.Cleanup(func() { auditRulesFile = prev })
}

// intervalRule is a rule version with the check interval sec and nothing to
// monitor.
func intervalRule(id int64, version, sec int) *RuleAssignment {
	r := &RuleAssignment{ID: id, Version: version, Name: "rule" + string(rune('a'+id))}
	r.Content.CheckIntervalSec = sec
	r.Content.Normalize()
	return r
}

// intervalRuntime returns a runtime on a fresh local database that has
// applied rule (stored as the agent stores an applied rule), with the queue's
// clock at *now and a transport whose sync answer the test sets.
func intervalRuntime(t *testing.T, applied *RuleAssignment, now *time.Time) (*AgentRuntime, *cmdTransport) {
	t.Helper()
	rt := newCommandRuntime(t, &cmdTransport{})
	if applied != nil {
		b, _ := json.Marshal(applied)
		setLocalState(t, rt.DB, stateRule, string(b))
		setLocalState(t, rt.DB, stateApply, `{"rule_id":1,"version":1,"status":"ok"}`)
	}
	tr := &cmdTransport{}
	rt = NewAgentRuntime(rt.DB, tr, nil)
	rt.clock = func() time.Time { return *now }
	rt.sched.now = rt.now
	return rt, tr
}

// monitorStartAt processes the monitoring step as a worker does (it takes the
// entry, processes it at *now) and returns the time the queued monitoring
// entry may start at.
func monitorStartAt(t *testing.T, rt *AgentRuntime) time.Time {
	t.Helper()
	u := pick(rt.sched)
	if u == nil || !u.monitor {
		t.Fatalf("the monitoring step may not start: %+v", u)
	}
	rt.process(u)
	rt.sched.mu.Lock()
	defer rt.sched.mu.Unlock()
	for _, e := range rt.sched.entries {
		if e.monitor {
			return e.startAt
		}
	}
	t.Fatal("no monitoring entry after the step")
	return time.Time{}
}

// TestIntervalOfTheAssignmentWhoseApplicationFails: the check interval of the
// rounds and of the monitoring entry is the one of the assignment the last
// sync brought (the assigned version's), also when that version could not be
// applied and the previous rule stays in effect: the central judges the agent
// by that interval. It is stored, and before the first sync of the next start
// the agent uses the stored interval.
func TestIntervalOfTheAssignmentWhoseApplicationFails(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rt, tr := intervalRuntime(t, intervalRule(1, 1, 3600), &now)
	failingAuditctl(t)
	if d := rt.link.interval(); d != time.Hour {
		t.Fatalf("before the first sync: %s, want the applied rule's 1h", d)
	}

	// The sync assigns version 2 with 20 s; until the step applies it, the
	// rounds already use 20 s.
	assigned := intervalRule(1, 2, 20)
	tr.resp = SyncResponse{Rule: assigned, DefaultIntervalSec: 120}
	rt.sync()
	if d := rt.link.interval(); d != 20*time.Second {
		t.Fatalf("after the sync: %s, want the assigned version's 20s", d)
	}
	if got := models.GetLocalState(rt.DB.Read(), stateCheckInterval); got != "20" {
		t.Fatalf("stored interval %q, want 20", got)
	}

	// The application fails: rule version 1 stays in effect; the next
	// monitoring entry and the next round use 20 s.
	rt.sched.start()
	at := monitorStartAt(t, rt)
	if rt.mon.rule == nil || rt.mon.rule.Version != 1 || rt.mon.lastApply == nil || rt.mon.lastApply.Status != "error" {
		t.Fatalf("the application of version 2 must have failed: %+v %+v", rt.mon.rule, rt.mon.lastApply)
	}
	if !at.Equal(now.Add(20 * time.Second)) {
		t.Fatalf("next monitoring step at %s, want %s (the assigned version's interval)", at, now.Add(20*time.Second))
	}
	if d := rt.link.interval(); d != 20*time.Second {
		t.Fatalf("after the failed application: %s, want 20s", d)
	}
	// Further syncs bring the same assignment (not tried again): 20 s.
	rt.sync()
	if d := rt.link.interval(); d != 20*time.Second {
		t.Fatalf("after the next sync: %s, want 20s", d)
	}

	// Restart: before its first sync the agent uses the stored 20 s, not the
	// interval of the rule it still applies.
	rt2 := NewAgentRuntime(rt.DB, &cmdTransport{}, nil)
	if d := rt2.link.interval(); d != 20*time.Second {
		t.Fatalf("after a restart before the first sync: %s, want the stored 20s", d)
	}
	// A failed sync changes nothing.
	rt2.comm.transport = &fakeTransport{fail: true}
	rt2.sync()
	if d := rt2.link.interval(); d != 20*time.Second {
		t.Fatalf("after a failed sync: %s, want 20s", d)
	}
}

// TestIntervalAfterUnassignmentWhoseRemovalFails: when the sync assigns no
// rule, the interval is the central's default of that answer, also while the
// removal of the audit rules fails and the rule stays in effect.
func TestIntervalAfterUnassignmentWhoseRemovalFails(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rt, tr := intervalRuntime(t, intervalRule(1, 1, 3600), &now)
	removals := 0
	rt.removeAudit = func(*AuditGlobals) error {
		removals++
		return errors.New("auditctl failed")
	}
	tr.resp = SyncResponse{DefaultIntervalSec: 30}
	rt.sync()
	if d := rt.link.interval(); d != 30*time.Second {
		t.Fatalf("after the sync without a rule: %s, want the default 30s", d)
	}
	rt.sched.start()
	// The first step applies the stored rule again only when the assignment
	// is that rule; here none is assigned: the step removes, which fails.
	at := monitorStartAt(t, rt)
	if removals != 1 || rt.mon.rule == nil || rt.mon.lastApply == nil || rt.mon.lastApply.Status != "error" {
		t.Fatalf("the removal must have failed and kept the rule: %d %+v %+v", removals, rt.mon.rule, rt.mon.lastApply)
	}
	if !at.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("next monitoring step at %s, want %s (the default of the sync)", at, now.Add(30*time.Second))
	}
	if d := rt.link.interval(); d != 30*time.Second {
		t.Fatalf("after the failed removal: %s, want 30s", d)
	}
	if got := models.GetLocalState(rt.DB.Read(), stateCheckInterval); got != "30" {
		t.Fatalf("stored interval %q, want 30", got)
	}
}

// TestIntervalBeforeTheFirstSync: without an interval stored from a sync, the
// agent uses the interval of the rule it applies, else the program's
// default, until its first sync.
func TestIntervalBeforeTheFirstSync(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rt, _ := intervalRuntime(t, nil, &now)
	if d := rt.link.interval(); d != DefaultCheckIntervalSec*time.Second {
		t.Fatalf("no rule, nothing stored: %s, want %ds", d, DefaultCheckIntervalSec)
	}
	rt, _ = intervalRuntime(t, intervalRule(1, 1, 600), &now)
	if d := rt.link.interval(); d != 600*time.Second {
		t.Fatalf("applied rule of 600 s, nothing stored: %s", d)
	}
	// "agent init --overwrite" clears the stored interval with the rule.
	setLocalState(t, rt.DB, stateCheckInterval, "20")
	if err := rt.DB.Write(resetStoredRule); err != nil {
		t.Fatal(err)
	}
	if got := models.GetLocalState(rt.DB.Read(), stateCheckInterval); got != "" {
		t.Fatalf("stored interval after agent init --overwrite: %q", got)
	}
}
