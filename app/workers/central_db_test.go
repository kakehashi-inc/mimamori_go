package workers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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

// Two reports of one agent with the same changed application result are
// processed at the same moment: each decides from the agent row read in its
// own transaction, so the result is recorded in the history once.
func TestReportCentralPartIsOneStep(t *testing.T) {
	e := newTestEnv(t)
	b, _ := models.GetBuiltinAgent(e.c.db.Read())
	tr := &inProcessTransport{c: e.c, agentID: b.ID}
	rep := &modules.ReportRequest{Apply: &modules.ApplyResult{Status: modules.ApplyStatusNone, Message: "nothing applied", At: 1234}}
	const flows = 8
	errs := make([]error, flows)
	var wg sync.WaitGroup
	for i := range flows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = tr.Report(rep)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("report %d: %v", i, err)
		}
	}
	items, _, err := models.ListHistory(e.c.db.Read(), models.HistRule, "", nil, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, h := range items {
		if h.Action == "apply_result_"+modules.ApplyStatusNone {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("application result recorded %d times", n)
	}
}

// syncWriter is a log destination several flows may write to.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// Handlers and periodic steps of every kind run at the same time for a few
// seconds: reports of four agents, acknowledgements, suppressions created
// and deleted, lists, the periodic steps and new tokens. No request fails,
// nothing waits on SQLite's lock (writers of the central take their turns
// at the handle), and every stored report is there afterwards.
func TestHandlersOfDifferentKindsAtOnce(t *testing.T) {
	e := newTestEnv(t)
	logs := &syncWriter{}
	e.c.logger = log.New(logs, "", 0)
	const agents = 4
	toks := make([]string, agents)
	ids := make([]int64, agents)
	for i := range agents {
		issued := e.admin("POST", "/tokens", map[string]any{"name": fmt.Sprintf("host-%d", i)})
		toks[i] = issued["value"].(string)
		a, _ := models.GetAgentByToken(e.c.db.Read(), int64(issued["token"].(map[string]any)["id"].(float64)))
		ids[i] = a.ID
		if rec := e.agentCall("sync", toks[i], fmt.Sprintf("m-%d", i), modules.SyncRequest{}); rec.Code != http.StatusOK {
			t.Fatalf("sync %d: %d", i, rec.Code)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	var mu sync.Mutex
	var failures []string
	stored := make([]int, agents)
	fail := func(what string, code int, body string) {
		mu.Lock()
		failures = append(failures, fmt.Sprintf("%s: %d %s", what, code, body))
		mu.Unlock()
	}
	var wg sync.WaitGroup
	run := func(n int, f func(flow, round int)) {
		for flow := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for round := 0; time.Now().Before(deadline); round++ {
					f(flow, round)
				}
			}()
		}
	}
	run(8, func(flow, round int) {
		i := flow % agents
		now := time.Now().Unix()
		rep := modules.ReportRequest{Detections: []modules.DetectionReport{{Type: "exec", Category: "normal", OSUser: "u", Exe: "/bin/x",
			Target: fmt.Sprintf("/f/%d/%d", flow, round), Count: 1, FirstAt: now, LastAt: now}}}
		rec := e.agentCall("report", toks[i], fmt.Sprintf("m-%d", i), rep)
		if rec.Code != http.StatusOK {
			fail("report", rec.Code, rec.Body.String())
			return
		}
		mu.Lock()
		stored[i]++
		mu.Unlock()
	})
	run(4, func(flow, round int) {
		code, body := e.adminRaw("GET", fmt.Sprintf("/detections?agent_id=%d&limit=5", ids[flow%agents]), nil)
		if code != http.StatusOK {
			fail("list for ack", code, body)
			return
		}
		var out struct {
			Items []struct {
				ID      int64 `json:"id"`
				AgentID int64 `json:"agent_id"`
			} `json:"items"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		if code, body := e.adminRaw("POST", "/detections/ack", map[string]any{"items": out.Items}); code != http.StatusOK {
			fail("ack", code, body)
		}
	})
	run(2, func(flow, round int) {
		code, body := e.adminRaw("POST", "/suppressions", map[string]any{"type": "exec", "target": fmt.Sprintf("/none/%d/%d", flow, round)})
		if code != http.StatusOK {
			fail("create suppression", code, body)
			return
		}
		var s struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal([]byte(body), &s)
		if code, body := e.adminRaw("DELETE", "/suppressions/"+itoa64(s.ID), nil); code != http.StatusOK {
			fail("delete suppression", code, body)
		}
	})
	run(2, func(flow, round int) {
		if code, body := e.adminRaw("GET", "/detections?limit=20", nil); code != http.StatusOK {
			fail("list", code, body)
		}
	})
	run(1, func(flow, round int) {
		now := time.Now()
		e.c.processTokenExpiry(now)
		e.c.runRequestsStep(now)
		e.c.retention(now)
		if err := e.exec(`UPDATE settings SET value = '0' WHERE key = ?`, settingLastRetentionAt); err != nil {
			fail("retention gate: "+err.Error(), 0, "")
		}
	})
	run(1, func(flow, round int) {
		if code, body := e.adminRaw("POST", "/tokens", map[string]any{"name": fmt.Sprintf("new-%d", round)}); code != http.StatusOK {
			fail("issue", code, body)
		}
	})
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("%d failures, first: %s", len(failures), failures[0])
	}
	if l := logs.String(); strings.Contains(l, "locked") || strings.Contains(l, "history:") {
		t.Fatalf("log: %s", l)
	}
	total := 0
	for i := range agents {
		a, _ := models.GetAgent(e.c.db.Read(), ids[i])
		tdb, err := e.c.targetDB(a)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := tdb.Read().QueryRow(`SELECT COUNT(*) FROM detections`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != stored[i] {
			t.Fatalf("agent %d: %d detections stored, %d reports accepted", i, n, stored[i])
		}
		total += n
	}
	if total == 0 {
		t.Fatal("no report was processed")
	}
}
