package workers

import (
	"testing"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// applyRows returns the history rows of application results (category rule,
// action apply_result_*) of the agent named target, oldest first.
func applyRows(t *testing.T, e *testEnv, target string) []string {
	t.Helper()
	items, _, err := models.ListHistory(e.c.db.Read(), models.HistRule, "", nil, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(items) - 1; i >= 0; i-- {
		if h := items[i]; h.Target == target && len(h.Action) > len("apply_result_") && h.Action[:len("apply_result_")] == "apply_result_" {
			out = append(out, h.Action)
		}
	}
	return out
}

// applyState returns the application status of the agent and the rule
// version it reported as applied.
func applyState(t *testing.T, e *testEnv, id int64) (string, int64, int) {
	t.Helper()
	a, err := models.GetAgent(e.c.db.Read(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a.ApplyStatus, a.ReportedRuleID, a.ReportedRuleVersion
}

// TestApplyResultOfEarlierAssignmentIgnored: the central takes an
// application result only when it is about the agent's current assignment
// (the same rule and version; "none" only while no rule is assigned). A
// result about an earlier assignment, which a report sent before the agent
// applied the change still carries, changes nothing and writes no history
// row; the status stays pending until the matching result arrives. A
// matching result is stored once with one history row.
func TestApplyResultOfEarlierAssignmentIgnored(t *testing.T) {
	e := newTestEnv(t)
	e.admin("POST", "/rules", map[string]any{"name": "web"})
	issued := e.admin("POST", "/tokens", map[string]any{"name": "host-a", "rule_id": 1})
	tok := issued["value"].(string)
	a, err := models.GetAgentByToken(e.c.db.Read(), int64(issued["token"].(map[string]any)["id"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	syncAgent(t, e, tok, "")
	okV1 := modules.ApplyResult{Status: "ok", Message: "applied (0 audit rules)", RuleID: 1, Version: 1, At: 100}
	reportAgent(t, e, tok, modules.ReportRequest{Apply: &okV1})
	if st, id, v := applyState(t, e, a.ID); st != "ok" || id != 1 || v != 1 {
		t.Fatalf("matching result: %s %d %d", st, id, v)
	}
	// The same result again (every periodic report carries it) and the
	// same batch again: nothing new.
	reportAgent(t, e, tok, modules.ReportRequest{Apply: &okV1})
	again := batchOf(modules.ReportRequest{Apply: &okV1})
	for range 2 {
		if rec := e.agentCall("report", tok, "m-"+tok[len(tok)-6:], again); rec.Code != 200 {
			t.Fatalf("report: %d", rec.Code)
		}
	}
	if rows := applyRows(t, e, "host-a"); len(rows) != 1 || rows[0] != "apply_result_ok" {
		t.Fatalf("one row for the matching result: %v", rows)
	}

	// Unassigned: the report sent before the agent removed the rules still
	// says ok for version 1.
	e.admin("POST", "/rules/unassign", map[string]any{"agent_ids": []int64{a.ID}})
	reportAgent(t, e, tok, modules.ReportRequest{Apply: &okV1})
	if st, _, _ := applyState(t, e, a.ID); st != "pending" {
		t.Fatalf("a result of the earlier assignment after the unassign: status %s", st)
	}
	none := modules.ApplyResult{Status: modules.ApplyStatusNone, Message: "no rule is assigned; the audit rules were removed", At: 200}
	reportAgent(t, e, tok, modules.ReportRequest{Apply: &none})
	if st, id, v := applyState(t, e, a.ID); st != modules.ApplyStatusNone || id != 0 || v != 0 {
		t.Fatalf("removed: %s %d %d", st, id, v)
	}
	if rows := applyRows(t, e, "host-a"); len(rows) != 2 || rows[1] != "apply_result_none" {
		t.Fatalf("rows after the removal: %v", rows)
	}

	// Assigned again: the report sent before the agent applied it still
	// says none; then the agent applied it.
	e.admin("POST", "/rules/1/apply", map[string]any{"version": 1, "agent_ids": []int64{a.ID}})
	reportAgent(t, e, tok, modules.ReportRequest{Apply: &none})
	if st, _, _ := applyState(t, e, a.ID); st != "pending" {
		t.Fatalf("\"none\" while a rule is assigned: status %s", st)
	}
	okV1.At = 300
	reportAgent(t, e, tok, modules.ReportRequest{Apply: &okV1})
	if st, _, _ := applyState(t, e, a.ID); st != "ok" {
		t.Fatalf("applied again: status %s", st)
	}

	// A new version: results of version 1 are of the earlier assignment, a
	// failure of version 2 is about the current one. Two items of one batch
	// are taken in order, each with its row.
	e.admin("PUT", "/rules/1", map[string]any{"content": map[string]any{"users": []any{map[string]any{"value": "alice"}}}})
	e.admin("POST", "/rules/1/apply", map[string]any{"version": 2, "agent_ids": []int64{a.ID}})
	failV2 := modules.ApplyResult{Status: "error", Message: "validation failed: x", RuleID: 1, Version: 2, At: 400}
	reportAgent(t, e, tok, modules.ReportRequest{Apply: &okV1})
	if st, _, _ := applyState(t, e, a.ID); st != "pending" {
		t.Fatalf("version 1 while version 2 is assigned: status %s", st)
	}
	okV2 := modules.ApplyResult{Status: "ok", Message: "applied (1 audit rules)", RuleID: 1, Version: 2, At: 500}
	if rec := e.agentCall("report", tok, "m-"+tok[len(tok)-6:], batchOf(modules.ReportRequest{Apply: &failV2}, modules.ReportRequest{Apply: &okV2})); rec.Code != 200 {
		t.Fatalf("report: %d", rec.Code)
	}
	if st, id, v := applyState(t, e, a.ID); st != "ok" || id != 1 || v != 2 {
		t.Fatalf("version 2: %s %d %d", st, id, v)
	}
	want := []string{"apply_result_ok", "apply_result_none", "apply_result_ok", "apply_result_error", "apply_result_ok"}
	if rows := applyRows(t, e, "host-a"); len(rows) != len(want) {
		t.Fatalf("rows %v, want %v", rows, want)
	} else {
		for i := range want {
			if rows[i] != want[i] {
				t.Fatalf("rows %v, want %v", rows, want)
			}
		}
	}

	// The built-in agent's report, handed over in the process, follows the
	// same rule.
	b, err := models.GetBuiltinAgent(e.c.db.Read())
	if err != nil {
		t.Fatal(err)
	}
	e.admin("POST", "/rules/1/apply", map[string]any{"version": 2, "agent_ids": []int64{b.ID}})
	tr := &inProcessTransport{c: e.c, agentID: b.ID}
	if err := tr.Report(batchOf(modules.ReportRequest{Apply: &none})); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := applyState(t, e, b.ID); st != "pending" {
		t.Fatalf("built-in agent, \"none\" while a rule is assigned: status %s", st)
	}
	if err := tr.Report(batchOf(modules.ReportRequest{Apply: &okV2})); err != nil {
		t.Fatal(err)
	}
	e.admin("POST", "/rules/unassign", map[string]any{"agent_ids": []int64{b.ID}})
	if err := tr.Report(batchOf(modules.ReportRequest{Apply: &okV2})); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := applyState(t, e, b.ID); st != "pending" {
		t.Fatalf("built-in agent, version 2 after the unassign: status %s", st)
	}
	if rows := applyRows(t, e, modules.BuiltinAgentName); len(rows) != 1 || rows[0] != "apply_result_ok" {
		t.Fatalf("built-in agent rows: %v", rows)
	}
}
