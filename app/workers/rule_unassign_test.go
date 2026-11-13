package workers

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// An administrator takes the rule away from an agent (management API and
// browser API); the agent's report that nothing is applied clears the
// applied rule on the central.
func TestUnassignRule(t *testing.T) {
	e := newTestEnv(t)
	e.admin("POST", "/rules", map[string]any{"name": "web"})
	issued := e.admin("POST", "/tokens", map[string]any{"name": "host-a", "rule_id": 1})
	tok := issued["value"].(string)
	if rec := e.agentCall("sync", tok, "m1", modules.SyncRequest{AgentVersion: "t"}); rec.Code != 200 {
		t.Fatalf("sync: %d", rec.Code)
	}
	ap := &modules.ApplyResult{Status: "ok", RuleID: 1, Version: 1, At: time.Now().Unix()}
	if rec := e.agentCall("report", tok, "m1", modules.ReportRequest{Apply: ap}); rec.Code != 200 {
		t.Fatalf("report: %d", rec.Code)
	}
	a, _ := models.GetAgentByToken(e.c.db, int64(issued["token"].(map[string]any)["id"].(float64)))

	if rec := e.do(e.agent, "POST", "/admin/v1/rules/unassign", map[string]any{"agent_ids": []int64{}}, map[string]string{"Authorization": "Bearer " + e.cliKey}, "127.0.0.1:1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("no agent: %d", rec.Code)
	}
	if rec := e.do(e.agent, "POST", "/admin/v1/rules/unassign", map[string]any{"agent_ids": []int64{999}}, map[string]string{"Authorization": "Bearer " + e.cliKey}, "127.0.0.1:1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown agent: %d", rec.Code)
	}
	if out := e.admin("POST", "/rules/unassign", map[string]any{"agent_ids": []int64{a.ID, a.ID}}); out["unassigned"].(float64) != 1 {
		t.Fatalf("unassign: %v", out)
	}
	got, _ := models.GetAgent(e.c.db, a.ID)
	if got.AssignedRuleID.Valid || got.ApplyStatus != "pending" {
		t.Fatalf("after unassign: %+v", got)
	}
	// The sync carries no rule.
	rec := e.agentCall("sync", tok, "m1", modules.SyncRequest{AgentVersion: "t", RuleID: 1, RuleVersion: 1})
	if resp := decode[modules.SyncResponse](t, rec); resp.Rule != nil {
		t.Fatalf("sync after unassign: %+v", resp.Rule)
	}
	// The agent removed its audit rules and reports it.
	none := &modules.ApplyResult{Status: modules.ApplyStatusNone, Message: "no rule is assigned; the audit rules were removed", At: time.Now().Unix() + 1}
	if rec := e.agentCall("report", tok, "m1", modules.ReportRequest{Apply: none}); rec.Code != 200 {
		t.Fatalf("report: %d", rec.Code)
	}
	got, _ = models.GetAgent(e.c.db, a.ID)
	if got.ApplyStatus != modules.ApplyStatusNone || got.ReportedRuleID != 0 || got.ReportedRuleVersion != 0 {
		t.Fatalf("after the agent's report: %+v", got)
	}
	list := e.admin("GET", "/agents/"+itoa64(a.ID), nil)
	if list["rule"] != nil || list["apply"].(map[string]any)["status"] != "none" {
		t.Fatalf("agent detail: %v", list)
	}
	// The rule can be deleted now.
	e.admin("DELETE", "/rules/1", nil)
	// An agent without a rule is left as it is.
	if out := e.admin("POST", "/rules/unassign", map[string]any{"agent_ids": []int64{a.ID}}); out["unassigned"].(float64) != 0 {
		t.Fatalf("unassign without a rule: %v", out)
	}
	items, _, _ := models.ListHistory(e.c.db, models.HistRule, "", nil, 20, 0)
	found := false
	for _, h := range items {
		if h.Action == "unassigned" && h.Target == "web" {
			found = true
		}
	}
	if !found {
		t.Fatalf("history: %+v", items)
	}

	// The browser API has the same route for administrators.
	e.admin("POST", "/rules", map[string]any{"name": "r2"})
	b, _ := models.GetBuiltinAgent(e.c.db)
	e.admin("POST", "/rules/2/apply", map[string]any{"version": 1, "agent_ids": []int64{b.ID}})
	cookie := login(t, e, testAdminEmail, "Admin-pass-123")
	rec = e.do(e.web, "POST", "/api/v1/rules/unassign", map[string]any{"agent_ids": []int64{b.ID}}, map[string]string{"Cookie": sessionCookie + "=" + cookie, csrfHeader: "1"}, "192.0.2.1:1")
	if rec.Code != 200 {
		t.Fatalf("browser unassign: %d %s", rec.Code, rec.Body.String())
	}
	got, _ = models.GetAgent(e.c.db, b.ID)
	if got.AssignedRuleID.Valid || got.ApplyStatus != "none" {
		t.Fatalf("built-in agent that never applied the rule: %+v", got)
	}
}

// With the built-in agent disabled, the central touches no audit rules
// when the built-in agent's database is absent or records no rule.
func TestDisabledBuiltinWithoutRecord(t *testing.T) {
	e := newTestEnv(t)
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "auditctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	b, _ := models.GetBuiltinAgent(e.c.db)
	off := false
	cfg := e.c.conf()
	cfg.BuiltinAgent = &off

	e.c.clearDisabledBuiltin(b)
	if _, err := os.Stat(filepath.Join(e.c.dataDir, modules.BuiltinAgentLocalDBName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the built-in agent's database must not be created")
	}
	db, err := modules.OpenSQLite(filepath.Join(e.c.dataDir, modules.BuiltinAgentLocalDBName), modules.SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	e.c.clearDisabledBuiltin(b)
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no audit command may run without a record")
	}
	// With a record the removal is attempted (here it fails, so the record
	// stays for the next start).
	db, _ = modules.OpenSQLite(filepath.Join(e.c.dataDir, modules.BuiltinAgentLocalDBName), modules.SchemaAgentLocal)
	_ = db.Write(func(tx models.Writer) error { return models.SetLocalState(tx, "rule", `{"id":1,"version":1}`) })
	db.Close()
	e.c.clearDisabledBuiltin(b)
	if _, err := os.Stat(calls); err != nil {
		t.Fatal("with a record the audit rules are removed")
	}
	db, _ = modules.OpenSQLite(filepath.Join(e.c.dataDir, modules.BuiltinAgentLocalDBName), modules.SchemaAgentLocal)
	defer db.Close()
	if models.GetLocalState(db.Read(), "rule") == "" {
		t.Fatal("the record must stay after a failed removal")
	}
}
