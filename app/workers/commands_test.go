package workers

import (
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// cli sends a management API request and returns the response.
func (e *testEnv) cli(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(e.agent, method, "/admin/v1"+path, body, map[string]string{"Authorization": "Bearer " + e.cliKey}, "127.0.0.1:5000")
}

// commandAgent issues a token for a fake agent and returns the token value,
// the agent and its target database.
func commandAgent(t *testing.T, e *testEnv, name string) (string, *models.Agent, *sql.DB) {
	t.Helper()
	issued := e.admin("POST", "/tokens", map[string]any{"name": name})
	tok := issued["value"].(string)
	a, err := models.GetAgentByToken(e.c.db, int64(issued["token"].(map[string]any)["id"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	tdb, err := e.c.targetDB(a)
	if err != nil {
		t.Fatal(err)
	}
	return tok, a, tdb
}

// commandSetBody is a valid definition without an OS user.
func commandSetBody(name, purpose string) map[string]any {
	return map[string]any{
		"name": name, "description": "secret description", "purpose": purpose, "work_dir": "/srv",
		"commands": []map[string]any{
			{"command_line": "cat /etc/hostname", "note": "secret note", "patterns": []string{}},
			{"command_line": "echo version=1.2", "note": "n2", "on_problems": "continue_always"},
		},
	}
}

func syncAgent(t *testing.T, e *testEnv, tok, sig string) modules.SyncResponse {
	t.Helper()
	rec := e.agentCall("sync", tok, "m-"+tok[len(tok)-6:], modules.SyncRequest{AgentVersion: "test", CommandSig: sig})
	if rec.Code != 200 {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}
	return decode[modules.SyncResponse](t, rec)
}

func reportAgent(t *testing.T, e *testEnv, tok string, rep modules.ReportRequest) {
	t.Helper()
	if rec := e.agentCall("report", tok, "m-"+tok[len(tok)-6:], rep); rec.Code != 200 {
		t.Fatalf("report: %d %s", rec.Code, rec.Body.String())
	}
}

func intp(v int) *int { return &v }

// runReport builds a run report of set as the agent sends it.
func runReport(uid string, setID int64, name string, requestID int64, results []modules.CommandResultReport, finished bool, state string) modules.CommandRunReport {
	r := modules.CommandRunReport{RunUID: uid, SetID: setID, SetRevision: 1, SetName: name, Purpose: modules.PurposeCollectOutput,
		Joiner: modules.JoinerComma, OSUser: "svc", WorkDir: "/srv", Trigger: modules.TriggerManual, RequestID: requestID,
		CommandsTotal: 2, StartedAt: 1000, Results: results}
	if requestID == 0 {
		r.Trigger, r.ScheduledFor = modules.TriggerSchedule, 960
	}
	if finished {
		r.Finished, r.FinishedAt, r.State = true, 1010, state
		r.Display = &modules.DisplayValue{Rows: []modules.DisplayRow{{Name: "version", Values: []string{"1.2"}, Value: "1.2"}}}
	}
	return r
}

func result(pos int, kind string, code int, out string) modules.CommandResultReport {
	return modules.CommandResultReport{Position: pos, CommandLine: "cmd " + string(rune('a'+pos)), Note: "note", Kind: kind,
		ExitCode: intp(code), StartedAt: 1000, FinishedAt: 1005, Output: []byte(out), OutputBytes: int64(len(out)),
		Values: &modules.CommandValues{Values: []modules.ExtractedValue{{Name: "version", Value: "1.2"}}}}
}

func TestCommandSetSyncAndHostInfo(t *testing.T) {
	e := newTestEnv(t)
	tok, a, tdb := commandAgent(t, e, "host-c")
	path := "/agents/" + itoa64(a.ID) + "/command-sets"

	// No default OS user before the agent reported one.
	list := e.admin("GET", path, nil)
	if list["default_os_user"] != "" || len(list["sets"].([]any)) != 0 {
		t.Fatalf("empty list: %v", list)
	}
	if rec := e.cli("POST", path, commandSetBody("s1", "collect_output")); rec.Code != 400 || !strings.Contains(rec.Body.String(), "os_user") {
		t.Fatalf("create without a default OS user: %d %s", rec.Code, rec.Body.String())
	}
	reportAgent(t, e, tok, modules.ReportRequest{HostInfo: &modules.HostInfo{Hostname: "h", OSUser: "svc", TimeZone: "JST", UTCOffsetSec: 32400}})
	list = e.admin("GET", path, nil)
	if list["default_os_user"] != "svc" || list["time_zone"] != "JST" || list["utc_offset_sec"].(float64) != 32400 {
		t.Fatalf("host values: %v", list)
	}
	created := e.admin("POST", path, commandSetBody("s1", "collect_output"))
	if created["os_user"] != "svc" || created["revision"].(float64) != 1 || created["work_dir"] != "/srv" {
		t.Fatalf("created: %v", created)
	}
	setID := int64(created["id"].(float64))
	// Invalid input: unknown fields, bad values, clashing names.
	for _, body := range []any{
		map[string]any{"name": "x", "purpose": "run_only", "commands": []any{map[string]any{"command_line": "true"}}, "bogus": 1},
		map[string]any{"name": "x", "purpose": "nope", "commands": []any{map[string]any{"command_line": "true"}}},
		map[string]any{"name": "x", "purpose": "run_only", "commands": []any{map[string]any{"command_line": "true", "timeout_sec": 0.5}}},
		map[string]any{"name": strings.Repeat("n", 101), "purpose": "run_only", "commands": []any{map[string]any{"command_line": "true"}}},
	} {
		if rec := e.cli("POST", path, body); rec.Code != 400 {
			t.Fatalf("invalid %v: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if rec := e.cli("POST", path, commandSetBody("s1", "run_only")); rec.Code != 409 {
		t.Fatalf("same name: %d", rec.Code)
	}
	v := e.admin("POST", path+"/validate", commandSetBody("s1", "collect_output"))
	if errs := v["errors"].([]any); len(errs) != 1 || !strings.Contains(errs[0].(string), "name") {
		t.Fatalf("validate clash: %v", v)
	}
	v = e.admin("POST", path+"/validate?set_id="+itoa64(setID), commandSetBody("s1", "collect_output"))
	if errs := v["errors"].([]any); len(errs) != 0 || v["definition"].(map[string]any)["os_user"] != "svc" {
		t.Fatalf("validate own name: %v", v)
	}
	v = e.admin("POST", path+"/validate", map[string]any{"unknown": true})
	if errs := v["errors"].([]any); len(errs) != 1 {
		t.Fatalf("validate parse error: %v", v)
	}

	// Sync sends the definitions while the signatures differ.
	resp := syncAgent(t, e, tok, "")
	if resp.CommandSets == nil || len(resp.CommandSets.Sets) != 1 || resp.CommandSets.Sets[0].Name != "s1" || resp.CommandSets.Sets[0].Commands[0].Note != "secret note" {
		t.Fatalf("definitions: %+v", resp.CommandSets)
	}
	sig := resp.CommandSets.Signature
	if got := e.admin("GET", path, nil); got["definitions_delivered"] != false {
		t.Fatalf("not yet delivered: %v", got)
	}
	if resp = syncAgent(t, e, tok, sig); resp.CommandSets != nil {
		t.Fatalf("same signature: %+v", resp.CommandSets)
	}
	if got := e.admin("GET", path, nil); got["definitions_delivered"] != true {
		t.Fatalf("delivered: %v", got)
	}
	// The agent's report on the stored definitions is kept and shown.
	reportAgent(t, e, tok, modules.ReportRequest{CommandApply: &modules.CommandApplyResult{Signature: sig, At: 5,
		Problems: []modules.CommandApplyProblem{{SetID: setID, SetName: "s1", Problems: []string{"os_user: bad"}}}}})
	got := e.admin("GET", path+"/"+itoa64(setID), nil)
	if p := got["agent_problems"].([]any); len(p) != 1 || p[0] != "os_user: bad" {
		t.Fatalf("agent problems: %v", got)
	}
	// An update changes the signature; an omitted OS user keeps the set's.
	upd := commandSetBody("s1-renamed", "collect_output")
	upd["schedule"] = map[string]any{"kind": "daily", "time": "03:00"}
	got = e.admin("PUT", path+"/"+itoa64(setID), upd)
	if got["revision"].(float64) != 2 || got["os_user"] != "svc" || !strings.Contains(got["schedule_summary"].(string), "JST, UTC+09:00") {
		t.Fatalf("updated: %v", got)
	}
	if resp = syncAgent(t, e, tok, sig); resp.CommandSets == nil || resp.CommandSets.Sets[0].Revision != 2 {
		t.Fatalf("changed definitions not sent: %+v", resp.CommandSets)
	}
	sig = resp.CommandSets.Signature
	// A recreated database (new random value) is noticed.
	if _, err := tdb.Exec(`UPDATE db_instance SET value = 'recreated'`); err != nil {
		t.Fatal(err)
	}
	if resp = syncAgent(t, e, tok, sig); resp.CommandSets == nil || resp.CommandSets.Signature == sig {
		t.Fatalf("recreated database not noticed: %+v", resp.CommandSets)
	}
	// Deleting removes the set from the definitions.
	e.admin("DELETE", path+"/"+itoa64(setID), nil)
	if rec := e.cli("GET", path+"/"+itoa64(setID), nil); rec.Code != 404 {
		t.Fatalf("deleted set: %d", rec.Code)
	}
	if resp = syncAgent(t, e, tok, sig); resp.CommandSets == nil || len(resp.CommandSets.Sets) != 0 {
		t.Fatalf("deleted set still delivered: %+v", resp.CommandSets)
	}

	// The built-in agent receives its definitions too.
	b, _ := models.GetBuiltinAgent(e.c.db)
	e.admin("POST", "/agents/"+itoa64(b.ID)+"/command-sets", map[string]any{"name": "local", "purpose": "run_only", "os_user": "root",
		"commands": []any{map[string]any{"command_line": "true"}}})
	tr := &inProcessTransport{c: e.c, agentID: b.ID}
	bresp, err := tr.Sync(&modules.SyncRequest{})
	if err != nil || bresp.CommandSets == nil || len(bresp.CommandSets.Sets) != 1 {
		t.Fatalf("built-in sync: %+v %v", bresp, err)
	}

	// The history has the changes, in the command category.
	items, _, _ := models.ListHistory(e.c.db, models.HistCommand, "", nil, 100, 0)
	actions := map[string]int{}
	for _, h := range items {
		actions[h.Action]++
	}
	if actions["created"] != 2 || actions["updated"] != 1 || actions["deleted"] != 1 {
		t.Fatalf("history: %v", actions)
	}
}

func TestCommandRequestLifecycle(t *testing.T) {
	e := newTestEnv(t)
	tok, a, tdb := commandAgent(t, e, "host-r")
	path := "/agents/" + itoa64(a.ID)
	body := commandSetBody("s", "collect_output")
	body["os_user"] = "svc"
	setID := int64(e.admin("POST", path+"/command-sets", body)["id"].(float64))
	sp := path + "/command-sets/" + itoa64(setID)

	// pending, refused twice, discarded
	q := e.admin("POST", sp+"/run", nil)
	if q["state"] != models.RequestPending || q["expires_at"] == nil {
		t.Fatalf("request: %v", q)
	}
	if rec := e.cli("POST", sp+"/run", nil); rec.Code != 409 {
		t.Fatalf("second request: %d", rec.Code)
	}
	if got := e.admin("GET", sp, nil)["request"].(map[string]any); got["id"] != q["id"] {
		t.Fatalf("active request on the set: %v", got)
	}
	qid := itoa64(int64(q["id"].(float64)))
	if d := e.admin("DELETE", path+"/command-requests/"+qid, nil); d["state"] != models.RequestDiscarded {
		t.Fatalf("discard: %v", d)
	}
	if rec := e.cli("DELETE", path+"/command-requests/"+qid, nil); rec.Code != 409 {
		t.Fatalf("discard again: %d", rec.Code)
	}
	if rec := e.cli("DELETE", path+"/command-requests/999", nil); rec.Code != 404 {
		t.Fatalf("discard unknown: %d", rec.Code)
	}

	// pending -> delivered (cannot be discarded, delivered again) -> done
	q = e.admin("POST", sp+"/run", nil)
	qid2 := int64(q["id"].(float64))
	resp := syncAgent(t, e, tok, "")
	sig := resp.CommandSets.Signature
	if len(resp.CommandRequests) != 1 || resp.CommandRequests[0].ID != qid2 || resp.CommandRequests[0].SetID != setID {
		t.Fatalf("delivered requests: %+v", resp.CommandRequests)
	}
	if r, _ := models.GetCommandRequest(tdb, qid2); r.State != models.RequestDelivered || r.DeliveredAt == 0 {
		t.Fatalf("delivered state: %+v", r)
	}
	if rec := e.cli("DELETE", path+"/command-requests/"+itoa64(qid2), nil); rec.Code != 409 {
		t.Fatalf("discard delivered: %d", rec.Code)
	}
	if resp = syncAgent(t, e, tok, sig); len(resp.CommandRequests) != 1 {
		t.Fatalf("delivered request is delivered again until answered: %+v", resp.CommandRequests)
	}
	reportAgent(t, e, tok, modules.ReportRequest{
		CommandRuns:        []modules.CommandRunReport{runReport("run-1", setID, "s", qid2, []modules.CommandResultReport{}, false, "")},
		CommandRequestAcks: []modules.CommandRequestAck{{RequestID: qid2, Status: modules.RequestAckStarted, RunUID: "run-1", At: 1000}},
	})
	if r, _ := models.GetCommandRequest(tdb, qid2); r.State != models.RequestDone || r.RunUID != "run-1" {
		t.Fatalf("done: %+v", r)
	}
	if resp = syncAgent(t, e, tok, sig); len(resp.CommandRequests) != 0 {
		t.Fatalf("answered request delivered: %+v", resp.CommandRequests)
	}

	// answered "expired" and "unknown_set"
	for _, status := range []string{modules.RequestAckExpired, modules.RequestAckUnknownSet} {
		q = e.admin("POST", sp+"/run", nil)
		id := int64(q["id"].(float64))
		syncAgent(t, e, tok, "")
		reportAgent(t, e, tok, modules.ReportRequest{CommandRequestAcks: []modules.CommandRequestAck{{RequestID: id, Status: status, At: 5}}})
		if r, _ := models.GetCommandRequest(tdb, id); r.State != models.RequestExpired {
			t.Fatalf("%s: %+v", status, r)
		}
	}

	// not delivered within 24 hours: expired by the periodic task, never delivered
	q = e.admin("POST", sp+"/run", nil)
	id := int64(q["id"].(float64))
	if _, err := tdb.Exec(`UPDATE command_requests SET expires_at = ? WHERE id = ?`, time.Now().Unix()-1, id); err != nil {
		t.Fatal(err)
	}
	if resp = syncAgent(t, e, tok, ""); len(resp.CommandRequests) != 0 {
		t.Fatalf("expired request delivered: %+v", resp.CommandRequests)
	}
	e.c.expireCommandRequests(time.Now())
	if r, _ := models.GetCommandRequest(tdb, id); r.State != models.RequestExpired {
		t.Fatalf("expired: %+v", r)
	}
	// Deleting a set discards its request that was not delivered.
	q = e.admin("POST", sp+"/run", nil)
	e.admin("DELETE", sp, nil)
	if r, _ := models.GetCommandRequest(tdb, int64(q["id"].(float64))); r.State != models.RequestDiscarded {
		t.Fatalf("request of a deleted set: %+v", r)
	}
	if rec := e.cli("POST", sp+"/run", nil); rec.Code != 404 {
		t.Fatalf("run of a deleted set: %d", rec.Code)
	}

	items, _, _ := models.ListHistory(e.c.db, models.HistCommand, "", nil, 100, 0)
	actions := map[string]int{}
	for _, h := range items {
		actions[h.Action]++
	}
	if actions["run_requested"] != 6 || actions["request_discarded"] != 1 || actions["request_expired"] != 3 {
		t.Fatalf("history: %v", actions)
	}
}

func TestCommandReportIdempotent(t *testing.T) {
	e := newTestEnv(t)
	tok, a, tdb := commandAgent(t, e, "host-i")
	body := commandSetBody("s", "collect_output")
	body["os_user"] = "svc"
	setID := int64(e.admin("POST", "/agents/"+itoa64(a.ID)+"/command-sets", body)["id"].(float64))

	start := runReport("u1", setID, "s", 0, []modules.CommandResultReport{}, false, "")
	r0 := runReport("u1", setID, "s", 0, []modules.CommandResultReport{result(0, modules.ResultSuccess, 0, "first\xff")}, false, "")
	r1 := runReport("u1", setID, "s", 0, []modules.CommandResultReport{result(1, modules.ResultFailure, 1, "second")}, true, modules.ResultFailure)
	for _, rep := range []modules.CommandRunReport{start, r0, r0, start} {
		reportAgent(t, e, tok, modules.ReportRequest{CommandRuns: []modules.CommandRunReport{rep}})
	}
	run, err := models.GetCommandRunByUID(tdb, "u1")
	if err != nil || run.State != models.RunStateRunning || run.Trigger != modules.TriggerSchedule || run.ScheduledFor != 960 {
		t.Fatalf("running run: %+v %v", run, err)
	}
	if res, _ := models.ListCommandResults(tdb, run.ID); len(res) != 1 || string(res[0].Output) != "first\xff" {
		t.Fatalf("results once: %+v", res)
	}
	reportAgent(t, e, tok, modules.ReportRequest{CommandRuns: []modules.CommandRunReport{r1}})
	// A second closing report (an interrupted report after the end) does
	// not change the run.
	again := r1
	again.State, again.Reason, again.Results = modules.ResultError, modules.ReasonInterrupted, nil
	reportAgent(t, e, tok, modules.ReportRequest{CommandRuns: []modules.CommandRunReport{again, r1}})
	run, _ = models.GetCommandRunByUID(tdb, "u1")
	if run.State != modules.ResultFailure || run.Reason != "" || run.FinishedAt != 1010 || !strings.Contains(run.DisplayRows, "1.2") {
		t.Fatalf("finished run: %+v", run)
	}
	if res, _ := models.ListCommandResults(tdb, run.ID); len(res) != 2 {
		t.Fatalf("results: %d", len(res))
	}
	var n int
	_ = tdb.QueryRow(`SELECT COUNT(*) FROM command_runs`).Scan(&n)
	if n != 1 {
		t.Fatalf("runs: %d", n)
	}
	// A run is created from whichever report arrives first; a run of a set
	// the database does not know is ignored.
	only := runReport("u2", setID, "s", 0, []modules.CommandResultReport{result(0, modules.ResultError, 0, "")}, true, "bogus")
	reportAgent(t, e, tok, modules.ReportRequest{CommandRuns: []modules.CommandRunReport{only, runReport("u3", 999, "x", 0, nil, true, "error")}})
	run2, err := models.GetCommandRunByUID(tdb, "u2")
	if err != nil || run2.State != modules.ResultError || run2.FinishedAt == 0 {
		t.Fatalf("run from a final report: %+v %v", run2, err)
	}
	if ok, _ := models.CommandRunExists(tdb, "u3"); ok {
		t.Fatal("run of an unknown set stored")
	}

	// Nothing about command results is mailed.
	setConfig(e, func(c *modules.CentralConfig) { c.SMTP.Host = "smtp.example" })
	e.c.composeNotification(time.Now())
	if list, _ := models.ListUnsentNotifications(e.c.db); len(list) != 0 {
		t.Fatalf("notification about command results: %+v", list)
	}
}

func TestCommandRetention(t *testing.T) {
	e := newTestEnv(t)
	_, a, tdb := commandAgent(t, e, "host-k")
	body := commandSetBody("s", "run_only")
	body["os_user"], body["keep_runs"] = "svc", 3
	setID := int64(e.admin("POST", "/agents/"+itoa64(a.ID)+"/command-sets", body)["id"].(float64))
	body["name"] = "old"
	oldID := int64(e.admin("POST", "/agents/"+itoa64(a.ID)+"/command-sets", body)["id"].(float64))
	now := time.Now().Unix()
	old := now - int64(e.c.conf().RetentionDays+1)*86400
	add := func(uid string, set int64, received, finished int64) {
		t.Helper()
		run, _, err := models.EnsureCommandRun(tdb, &models.CommandRun{RunUID: uid, SetID: set, Trigger: "manual", StartedAt: finished}, received)
		if err != nil {
			t.Fatal(err)
		}
		if finished > 0 {
			if err := models.FinishCommandRun(tdb, run.ID, "success", "", finished, "", false, received); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := int64(1); i <= 5; i++ {
		add("k"+itoa64(i), setID, now, now+i)
	}
	add("running", setID, now, 0)
	add("ancient", oldID, old, old) // the only (last) run of its set
	for _, st := range []string{"done", "expired", "discarded", "pending"} {
		if _, err := tdb.Exec(`INSERT INTO command_requests (set_id, state, requested_at, requested_by, expires_at) VALUES (?, ?, ?, 'x', ?)`,
			oldID, st, old, old+86400); err != nil {
			t.Fatal(err)
		}
	}
	e.c.retention(time.Now())
	var uids []string
	rows, _ := tdb.Query(`SELECT run_uid FROM command_runs ORDER BY run_uid`)
	for rows.Next() {
		var u string
		_ = rows.Scan(&u)
		uids = append(uids, u)
	}
	rows.Close()
	if !slices.Equal(uids, []string{"k3", "k4", "k5", "running"}) {
		t.Fatalf("runs after retention: %v", uids)
	}
	var states []string
	rows, _ = tdb.Query(`SELECT state FROM command_requests`)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		states = append(states, s)
	}
	rows.Close()
	if !slices.Equal(states, []string{"pending"}) {
		t.Fatalf("requests after retention: %v", states)
	}
}

func TestCommandImport(t *testing.T) {
	e := newTestEnv(t)
	_, src, _ := commandAgent(t, e, "src")
	_, dst, _ := commandAgent(t, e, "dst")
	sp, dp := "/agents/"+itoa64(src.ID)+"/command-sets", "/agents/"+itoa64(dst.ID)+"/command-sets"
	b1 := commandSetBody("a", "collect_output")
	b1["os_user"], b1["work_dir"] = "alice", "/home/alice"
	b1["schedule"] = map[string]any{"kind": "every_minutes", "every": 15}
	b2 := commandSetBody("b", "run_only")
	b2["os_user"] = "bob"
	b2["commands"] = []any{map[string]any{"command_line": "true"}}
	a1 := int64(e.admin("POST", sp, b1)["id"].(float64))
	a2 := int64(e.admin("POST", sp, b2)["id"].(float64))
	e.admin("POST", dp, map[string]any{"name": "b", "purpose": "run_only", "os_user": "x", "commands": []any{map[string]any{"command_line": "true"}}})

	conflict := func(path string, body any, want []string) {
		t.Helper()
		rec := e.cli("POST", path, body)
		if rec.Code != 409 {
			t.Fatalf("import %v: %d %s", body, rec.Code, rec.Body.String())
		}
		out := decode[struct {
			Conflicts []string `json:"conflicts"`
		}](t, rec)
		if !slices.Equal(out.Conflicts, want) {
			t.Fatalf("conflicts: %v, want %v", out.Conflicts, want)
		}
	}
	// "b" exists in the destination; nothing is created.
	conflict(dp+"/import", map[string]any{"source_agent_id": src.ID, "items": []any{map[string]any{"set_id": a1}, map[string]any{"set_id": a2}}}, []string{"b"})
	// Names repeated in the import.
	conflict(dp+"/import", map[string]any{"source_agent_id": src.ID, "items": []any{map[string]any{"set_id": a1, "name": "z"}, map[string]any{"set_id": a2, "name": "z"}}}, []string{"z"})
	// The same target without new names clashes with itself.
	conflict(sp+"/import", map[string]any{"source_agent_id": src.ID, "items": []any{map[string]any{"set_id": a1}}}, []string{"a"})
	if n := len(e.admin("GET", dp, nil)["sets"].([]any)); n != 1 {
		t.Fatalf("sets created by a refused import: %d", n)
	}
	if rec := e.cli("POST", dp+"/import", map[string]any{"source_agent_id": src.ID, "items": []any{map[string]any{"set_id": 999}}}); rec.Code != 400 {
		t.Fatalf("unknown source set: %d", rec.Code)
	}
	if rec := e.cli("POST", dp+"/import", map[string]any{"source_agent_id": src.ID, "items": []any{map[string]any{"set_id": a1, "name": "bad\x01"}}}); rec.Code != 400 {
		t.Fatalf("invalid new name: %d", rec.Code)
	}
	out := e.admin("POST", dp+"/import", map[string]any{"source_agent_id": src.ID,
		"items": []any{map[string]any{"set_id": a1}, map[string]any{"set_id": a2, "name": "b2"}}})
	sets := out["sets"].([]any)
	if len(sets) != 2 {
		t.Fatalf("imported: %v", out)
	}
	first := sets[0].(map[string]any)
	if first["name"] != "a" || first["os_user"] != "alice" || first["work_dir"] != "/home/alice" || first["imported_from"] != "src / a" ||
		first["schedule"].(map[string]any)["every"].(float64) != 15 || len(first["commands"].([]any)) != 2 {
		t.Fatalf("copied set: %v", first)
	}
	if sets[1].(map[string]any)["name"] != "b2" {
		t.Fatalf("renamed set: %v", sets[1])
	}
	// The same target with new names.
	out = e.admin("POST", sp+"/import", map[string]any{"source_agent_id": src.ID, "items": []any{map[string]any{"set_id": a1, "name": "a copy"}}})
	if out["sets"].([]any)[0].(map[string]any)["name"] != "a copy" {
		t.Fatalf("same-target import: %v", out)
	}
	items, _, _ := models.ListHistory(e.c.db, models.HistCommand, "", nil, 100, 0)
	n := 0
	for _, h := range items {
		if h.Action == "imported" {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("import history: %d", n)
	}
}

// forbiddenForViewer are the keys a viewer must never receive.
var forbiddenForViewer = []string{"command_line", "note", "os_user", "work_dir", "schedule", "schedule_summary", "purpose", "description",
	"patterns", "success", "failure", "on_problems", "timeout_sec", "line_limit", "joiner", "keep_runs", "set_revision",
	"trigger", "scheduled_for", "request_id", "commands_total"}

// viewerRunKeys are exactly the keys of a run a viewer receives.
var viewerRunKeys = []string{"id", "set_id", "set_name", "started_at", "finished_at", "state", "reason", "display", "received_at"}

// adminRunKeys are the keys of a run only administrators and the CLI receive.
var adminRunKeys = []string{"trigger", "scheduled_for", "request_id", "commands_total", "set_revision", "purpose", "os_user", "work_dir"}

// runObjects returns the runs of a results, run list or run detail response.
func runObjects(t *testing.T, doc map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, k := range []string{"results", "sets"} {
		for _, x := range sliceOf(doc[k]) {
			if lr, ok := x.(map[string]any)["last_run"].(map[string]any); ok {
				out = append(out, lr)
			}
		}
	}
	for _, x := range sliceOf(doc["runs"]) {
		out = append(out, x.(map[string]any))
	}
	if _, ok := doc["commands"]; ok {
		out = append(out, doc)
	}
	if len(out) == 0 {
		t.Fatalf("no run in %v", doc)
	}
	return out
}

func sliceOf(v any) []any {
	s, _ := v.([]any)
	return s
}

// sortedKeys returns the keys of an object in order.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// jsonKeys collects every object key of a JSON document.
func jsonKeys(v any, into map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, sub := range x {
			into[k] = true
			jsonKeys(sub, into)
		}
	case []any:
		for _, sub := range x {
			jsonKeys(sub, into)
		}
	}
}

func TestCommandResultsPermissions(t *testing.T) {
	e := newTestEnv(t)
	tok, a, _ := commandAgent(t, e, "host-v")
	_, a2, _ := commandAgent(t, e, "host-w")
	base := "/agents/" + itoa64(a.ID)
	body := commandSetBody("disk", "collect_output")
	body["os_user"] = "svc"
	setID := int64(e.admin("POST", base+"/command-sets", body)["id"].(float64))
	e.admin("POST", "/agents/"+itoa64(a2.ID)+"/command-sets", body)
	ro := commandSetBody("ro", "run_only")
	ro["os_user"] = "svc"
	ro["commands"] = []any{map[string]any{"command_line": "true"}}
	e.admin("POST", base+"/command-sets", ro)
	reportAgent(t, e, tok, modules.ReportRequest{CommandRuns: []modules.CommandRunReport{
		runReport("v1", setID, "disk", 0, []modules.CommandResultReport{result(0, modules.ResultSuccess, 0, "out0"), result(1, modules.ResultSuccess, 0, "out1")}, true, modules.ResultSuccess),
	}})
	run, _ := models.GetCommandRunByUID(e.mustTDB(a), "v1")
	viewer := newViewer(t, e)
	admin := login(t, e, testAdminEmail, "Admin-pass-123")

	paths := []string{
		"/agents/" + itoa64(a.ID) + "/command-results",
		"/command-results?name=disk",
		"/agents/" + itoa64(a.ID) + "/command-sets/" + itoa64(setID) + "/runs",
		"/agents/" + itoa64(a.ID) + "/command-runs/" + itoa64(run.ID),
	}
	for _, p := range paths {
		vrec := e.do(e.web, "GET", "/api/v1"+p, nil, browserHdr(viewer, false), "x:1")
		if vrec.Code != 200 {
			t.Fatalf("viewer %s: %d %s", p, vrec.Code, vrec.Body.String())
		}
		var doc any
		_ = json.Unmarshal(vrec.Body.Bytes(), &doc)
		keys := map[string]bool{}
		jsonKeys(doc, keys)
		for _, k := range forbiddenForViewer {
			if keys[k] {
				t.Errorf("viewer %s receives %q: %s", p, k, vrec.Body.String())
			}
		}
		if strings.Contains(vrec.Body.String(), "secret") || strings.Contains(vrec.Body.String(), "svc") || strings.Contains(vrec.Body.String(), "/srv") {
			t.Errorf("viewer %s receives definition values: %s", p, vrec.Body.String())
		}
		// The runs carry exactly the viewer's keys (the run detail also
		// the detail keys).
		detailKeys := []string{"agent_id", "agent_name", "not_executed", "commands"}
		for _, run := range runObjects(t, doc.(map[string]any)) {
			want := slices.Clone(viewerRunKeys)
			if _, ok := run["commands"]; ok {
				want = append(want, detailKeys...)
			}
			slices.Sort(want)
			if got := sortedKeys(run); !slices.Equal(got, want) {
				t.Errorf("viewer %s run keys: %v, want %v", p, got, want)
			}
		}
		arec := e.do(e.web, "GET", "/api/v1"+p, nil, browserHdr(admin, false), "x:1")
		crec := e.cli("GET", p, nil)
		if arec.Code != 200 || crec.Code != 200 {
			t.Fatalf("admin %s: %d %d", p, arec.Code, crec.Code)
		}
		for who, rec := range map[string]*httptest.ResponseRecorder{"admin": arec, "cli": crec} {
			for _, run := range runObjects(t, decode[map[string]any](t, rec)) {
				for _, k := range adminRunKeys {
					if _, ok := run[k]; !ok {
						t.Errorf("%s %s run lacks %q: %v", who, p, k, run)
					}
				}
				if run["trigger"] != "schedule" || run["scheduled_for"] == nil || run["commands_total"] != float64(2) {
					t.Errorf("%s %s run: %v", who, p, run)
				}
			}
		}
	}
	// What the viewer does receive (the display value, not the values of
	// each command).
	detail := decode[map[string]any](t, e.do(e.web, "GET", "/api/v1"+paths[3], nil, browserHdr(viewer, false), "x:1"))
	cmds := detail["commands"].([]any)
	c0 := cmds[0].(map[string]any)
	if _, ok := c0["values"]; ok {
		t.Fatalf("viewer receives the values of a command: %v", c0)
	}
	if detail["set_name"] != "disk" || detail["state"] != "success" || detail["display"] == nil || detail["finished_at"] == nil ||
		len(cmds) != 2 || c0["output"] != "out0" || c0["exit_code"].(float64) != 0 || c0["kind"] != "success" || c0["started_at"] == nil {
		t.Fatalf("viewer run detail: %v", detail)
	}
	results := decode[map[string]any](t, e.do(e.web, "GET", "/api/v1"+paths[0], nil, browserHdr(viewer, false), "x:1"))
	if len(results["results"].([]any)) != 1 || len(results["sets"].([]any)) != 2 {
		t.Fatalf("viewer results: %v", results)
	}
	byName := decode[map[string]any](t, e.do(e.web, "GET", "/api/v1"+paths[1], nil, browserHdr(viewer, false), "x:1"))
	if names := byName["names"].([]any); len(names) != 1 || names[0] != "disk" || len(byName["results"].([]any)) != 2 {
		t.Fatalf("viewer results by name: %v", byName)
	}
	// Administrators and the CLI receive the definition fields.
	full := decode[map[string]any](t, e.cli("GET", paths[3], nil))
	fc := full["commands"].([]any)[0].(map[string]any)
	if full["os_user"] != "svc" || full["work_dir"] != "/srv" || full["purpose"] != "collect_output" || fc["command_line"] != "cmd a" ||
		fc["note"] != "note" || fc["values"] == nil {
		t.Fatalf("full run detail: %v", full)
	}
	// Viewers do not receive the command history category.
	h := decode[map[string]any](t, e.do(e.web, "GET", "/api/v1/history?category=command", nil, browserHdr(viewer, false), "x:1"))
	if h["total"].(float64) != 0 {
		t.Fatalf("viewer command history: %v", h)
	}
	h = decode[map[string]any](t, e.do(e.web, "GET", "/api/v1/history?category=command", nil, browserHdr(admin, false), "x:1"))
	if h["total"].(float64) == 0 {
		t.Fatalf("administrator command history: %v", h)
	}
	// Administrator routes through the browser.
	for _, r := range [][2]string{{"GET", base + "/command-sets"}, {"GET", base + "/command-sets/" + itoa64(setID)},
		{"POST", base + "/command-sets/" + itoa64(setID) + "/run"}, {"DELETE", base + "/command-requests/1"}} {
		if rec := e.do(e.web, r[0], "/api/v1"+r[1], nil, browserHdr(admin, true), "x:1"); rec.Code != 200 {
			t.Fatalf("admin %s %s: %d %s", r[0], r[1], rec.Code, rec.Body.String())
		}
	}
}

// TestCommandSetBrowserAdministrator runs the changing routes as an
// administrator through the browser API.
func TestCommandSetBrowserAdministrator(t *testing.T) {
	e := newTestEnv(t)
	_, a, _ := commandAgent(t, e, "host-b")
	admin := login(t, e, testAdminEmail, "Admin-pass-123")
	base := "/api/v1/agents/" + itoa64(a.ID) + "/command-sets"
	call := func(method, path string, body any) map[string]any {
		t.Helper()
		rec := e.do(e.web, method, path, body, browserHdr(admin, true), "x:1")
		if rec.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		return decode[map[string]any](t, rec)
	}
	body := commandSetBody("web", "collect_output")
	body["os_user"] = "www"
	if v := call("POST", base+"/validate", body); len(v["errors"].([]any)) != 0 {
		t.Fatalf("validate: %v", v)
	}
	id := itoa64(int64(call("POST", base, body)["id"].(float64)))
	body["name"] = "web2"
	if got := call("PUT", base+"/"+id, body); got["name"] != "web2" {
		t.Fatalf("update: %v", got)
	}
	call("POST", base+"/import", map[string]any{"source_agent_id": a.ID, "items": []any{map[string]any{"set_id": 1, "name": "web3"}}})
	call("DELETE", base+"/"+id, nil)
	if got := call("GET", base, nil); len(got["sets"].([]any)) != 1 {
		t.Fatalf("list: %v", got)
	}
}

func (e *testEnv) mustTDB(a *models.Agent) *sql.DB {
	tdb, err := e.c.targetDB(a)
	if err != nil {
		e.t.Fatal(err)
	}
	return tdb
}

// TestCommandSetEndToEnd runs a set on the real agent flow of the built-in
// agent: definitions and the request go out with sync, the run and its
// results come back through the outbox.
func TestCommandSetEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	e := newTestEnv(t)
	b, _ := models.GetBuiltinAgent(e.c.db)
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	base := "/agents/" + itoa64(b.ID)
	set := e.admin("POST", base+"/command-sets", map[string]any{"name": "ver", "purpose": "collect_output", "os_user": me.Username,
		"commands": []any{map[string]any{"command_line": "echo version=7.1; echo build=42", "patterns": []string{`version=(?P<version>\S+)`}}}})
	setID := itoa64(int64(set["id"].(float64)))
	e.admin("POST", base+"/command-sets/"+setID+"/run", nil)

	db, err := modules.OpenSQLite(filepath.Join(t.TempDir(), "agent.db"), modules.SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rt := modules.NewAgentRuntime(db, &inProcessTransport{c: e.c, agentID: b.ID}, log.New(io.Discard, "", 0))
	rt.Run(func(time.Duration) bool { return false }, func() bool { return false })
	rt.Cycle() // sends what the run queued

	runs := e.admin("GET", base+"/command-sets/"+setID+"/runs", nil)["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs: %v", runs)
	}
	run := runs[0].(map[string]any)
	if run["state"] != "success" || run["trigger"] != "manual" || run["finished_at"] == nil {
		t.Fatalf("run: %v", run)
	}
	// Administrators receive scheduled_for as null for a manual run.
	if due, ok := run["scheduled_for"]; !ok || due != nil || run["request_id"].(float64) <= 0 || run["commands_total"] != float64(1) {
		t.Fatalf("run: %v", run)
	}
	rows := run["display"].(map[string]any)["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["name"] != "version" || rows[0].(map[string]any)["value"] != "7.1" {
		t.Fatalf("display value: %v", run["display"])
	}
	detail := e.admin("GET", base+"/command-runs/"+itoa64(int64(run["id"].(float64))), nil)
	if c0 := detail["commands"].([]any)[0].(map[string]any); c0["output"] != "version=7.1\nbuild=42\n" || c0["exit_code"].(float64) != 0 {
		t.Fatalf("console output: %v", c0)
	}
	got := e.admin("GET", base+"/command-sets/"+setID, nil)
	if got["request"] != nil {
		t.Fatalf("request still waiting: %v", got["request"])
	}
	if list := e.admin("GET", base+"/command-sets", nil); list["definitions_delivered"] != true {
		t.Fatalf("definitions not reported as delivered: %v", list)
	}
}
