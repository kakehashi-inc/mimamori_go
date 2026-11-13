package workers

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// The first requests of a new agent arrive at the same moment: each reads
// the token, checks it and records its use in one write transaction, so
// every request is accepted (none is refused because the row changed
// between its read and its write) and the token is bound once.
func TestAgentAuthTakesTurns(t *testing.T) {
	e := newTestEnv(t)
	issued := e.admin("POST", "/tokens", map[string]any{"name": "host-turns", "valid_days": 10})
	tok := issued["value"].(string)
	const flows = 16
	codes := make([]int, flows)
	var wg sync.WaitGroup
	for i := range flows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = e.agentCall("sync", tok, "m-turns", modules.SyncRequest{}).Code
		}()
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	items, _, err := models.ListHistory(e.c.db.Read(), models.HistToken, "host-turns", nil, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	bound := 0
	for _, h := range items {
		if h.Action == "bound" {
			bound++
		}
	}
	if bound != 1 {
		t.Fatalf("bound recorded %d times", bound)
	}
}

// failAgentUpdates makes every change of the agent row id fail (a trigger),
// so that a transaction that changes several agents fails in the middle.
func failAgentUpdates(t *testing.T, e *testEnv, id int64) {
	t.Helper()
	err := e.c.db.Write(func(tx models.Writer) error {
		_, err := tx.Exec(`CREATE TRIGGER fail_agent BEFORE UPDATE ON agents WHEN OLD.id = ` + itoa64(id) + ` BEGIN SELECT RAISE(ABORT, 'test'); END`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A rule applied to several agents is applied to all of them or to none:
// when the change of the second agent fails, the first keeps no rule.
func TestApplyRuleAllOrNone(t *testing.T) {
	e := newTestEnv(t)
	e.admin("POST", "/rules", map[string]any{"name": "web"})
	issued := e.admin("POST", "/tokens", map[string]any{"name": "host-all"})
	b, _ := models.GetBuiltinAgent(e.c.db.Read())
	other, _ := models.GetAgentByToken(e.c.db.Read(), int64(issued["token"].(map[string]any)["id"].(float64)))
	if code, body := e.adminRaw("POST", "/rules/1/apply", map[string]any{"agent_ids": []int64{b.ID, 999}}); code != http.StatusBadRequest {
		t.Fatalf("unknown agent: %d %s", code, body)
	}
	failAgentUpdates(t, e, other.ID)
	if code, body := e.adminRaw("POST", "/rules/1/apply", map[string]any{"agent_ids": []int64{b.ID, other.ID}}); code != http.StatusInternalServerError {
		t.Fatalf("apply with a failing agent: %d %s", code, body)
	}
	if got, _ := models.GetAgent(e.c.db.Read(), b.ID); got.AssignedRuleID.Valid {
		t.Fatalf("the first agent kept the rule of a failed apply: %+v", got)
	}
	if out := e.admin("POST", "/rules/1/apply", map[string]any{"agent_ids": []int64{b.ID}}); out["applied"].(float64) != 1 {
		t.Fatalf("apply: %v", out)
	}
	if code, body := e.adminRaw("POST", "/rules/unassign", map[string]any{"agent_ids": []int64{b.ID, other.ID}}); code != http.StatusOK {
		t.Fatalf("unassign: %d %s", code, body)
	}
	e.admin("POST", "/rules/1/apply", map[string]any{"agent_ids": []int64{b.ID}})
	err := e.c.db.Write(func(tx models.Writer) error {
		_, err := tx.Exec(`DROP TRIGGER fail_agent`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	e.admin("POST", "/rules/1/apply", map[string]any{"agent_ids": []int64{other.ID}})
	failAgentUpdates(t, e, other.ID)
	if code, body := e.adminRaw("POST", "/rules/unassign", map[string]any{"agent_ids": []int64{b.ID, other.ID}}); code != http.StatusInternalServerError {
		t.Fatalf("unassign with a failing agent: %d %s", code, body)
	}
	if got, _ := models.GetAgent(e.c.db.Read(), b.ID); !got.AssignedRuleID.Valid {
		t.Fatalf("the first agent lost the rule in a failed unassign: %+v", got)
	}
	items, _, _ := models.ListHistory(e.c.db.Read(), models.HistRule, "", nil, 50, 0)
	for _, h := range items {
		if h.Action == "unassigned" && strings.Contains(h.Detail, "host-all") {
			t.Fatalf("history of a failed unassign: %+v", h)
		}
	}
}

// A rule that an agent uses is not deleted; once no agent uses it, it is.
func TestDeleteRuleInUse(t *testing.T) {
	e := newTestEnv(t)
	e.admin("POST", "/rules", map[string]any{"name": "web"})
	b, _ := models.GetBuiltinAgent(e.c.db.Read())
	e.admin("POST", "/rules/1/apply", map[string]any{"agent_ids": []int64{b.ID}})
	if code, body := e.adminRaw("DELETE", "/rules/1", nil); code != http.StatusConflict || !strings.Contains(body, "rule.in_use") {
		t.Fatalf("delete a rule in use: %d %s", code, body)
	}
	if _, err := models.GetRule(e.c.db.Read(), 1); err != nil {
		t.Fatalf("the rule in use is gone: %v", err)
	}
	e.admin("POST", "/rules/unassign", map[string]any{"agent_ids": []int64{b.ID}})
	if code, body := e.adminRaw("DELETE", "/rules/1", nil); code != http.StatusOK {
		t.Fatalf("delete an unused rule: %d %s", code, body)
	}
}

// The token and its agent row are written in one transaction: when the
// agent row cannot be written, no token is left; a refused issue writes
// nothing.
func TestIssueTokenIsOneStep(t *testing.T) {
	e := newTestEnv(t)
	count := func() (tokens, agents int) {
		t.Helper()
		if err := e.c.db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM tokens), (SELECT COUNT(*) FROM agents)`).Scan(&tokens, &agents); err != nil {
			t.Fatal(err)
		}
		return
	}
	if code, body := e.adminRaw("POST", "/tokens", map[string]any{"name": "host-x", "rule_id": 5}); code != http.StatusBadRequest {
		t.Fatalf("issue with an unknown rule: %d %s", code, body)
	}
	e.admin("POST", "/tokens", map[string]any{"name": "host-x"})
	if code, body := e.adminRaw("POST", "/tokens", map[string]any{"name": "host-x"}); code != http.StatusConflict {
		t.Fatalf("issue with a taken name: %d %s", code, body)
	}
	if tokens, agents := count(); tokens != 1 || agents != 2 {
		t.Fatalf("after the refused issues: %d tokens, %d agents", tokens, agents)
	}
	err := e.c.db.Write(func(tx models.Writer) error {
		_, err := tx.Exec(`CREATE TRIGGER fail_agent_file BEFORE UPDATE OF db_file ON agents BEGIN SELECT RAISE(ABORT, 'test'); END`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := e.adminRaw("POST", "/tokens", map[string]any{"name": "host-y"}); code != http.StatusInternalServerError {
		t.Fatalf("issue with a failing agent row: %d %s", code, body)
	}
	if tokens, agents := count(); tokens != 1 || agents != 2 {
		t.Fatalf("after the failed issue: %d tokens, %d agents", tokens, agents)
	}
}
