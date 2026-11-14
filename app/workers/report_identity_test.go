package workers

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// The identity of report batches on the central's side: the last batch
// stored for an agent is remembered (report_id) in the transaction that
// stores it, and the same batch sent again is answered as stored without
// being stored or counted again.

// identityReport is a batch with one item: a detection of count 3, a dropped
// count of 2 and a finished scheduled run of set.
func identityReport(id string, setID int64) *modules.ReportBatch {
	return &modules.ReportBatch{ReportID: id, Reports: []modules.ReportRequest{{
		Detections: []modules.DetectionReport{{Type: "exec", Category: "alert", OSUser: "nginx", Exe: "/usr/bin/curl",
			Target: "twice", Count: 3, FirstAt: 1000, LastAt: 1001}},
		Dropped:     2,
		CommandRuns: []modules.CommandRunReport{runReport("id-run", setID, "s", 0, []modules.CommandResultReport{}, true, modules.ResultSuccess)},
	}}}
}

// postReport sends a report body of an agent as it is and returns the answer.
func postReport(e *testEnv, tok string, body any) *httptest.ResponseRecorder {
	return e.agentCall("report", tok, "m-"+tok[len(tok)-6:], body)
}

// identityAgent issues a token agent with one set and a central log that the
// test can read.
func identityAgent(t *testing.T, e *testEnv, name string) (string, *models.Agent, *modules.DB, int64, func() string) {
	t.Helper()
	var logs bytes.Buffer
	var logMu sync.Mutex
	e.c.logger = log.New(lockedWriter{&logs, &logMu}, "", 0)
	tok, a, tdb := commandAgent(t, e, name)
	body := commandSetBody("s", "collect_output")
	body["os_user"] = "svc"
	setID := int64(e.admin("POST", "/agents/"+itoa64(a.ID)+"/command-sets", body)["id"].(float64))
	return tok, a, tdb, setID, func() string {
		logMu.Lock()
		defer logMu.Unlock()
		return logs.String()
	}
}

// detectionCount returns the count of the one detection of the target "twice".
func detectionCount(t *testing.T, tdb *modules.DB) int64 {
	t.Helper()
	list, _, err := models.ListDetections(tdb.Read(), models.DetectionFilter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, d := range list {
		if d.Target == "twice" {
			n += int64(d.Count)
		}
	}
	return n
}

// droppedHistory counts the history rows "dropped".
func droppedHistory(t *testing.T, e *testEnv) int {
	t.Helper()
	items, _, err := models.ListHistory(e.c.db.Read(), models.HistDetection, "", nil, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, h := range items {
		if h.Action == "dropped" {
			n++
		}
	}
	return n
}

// wantStoredOnce checks what one report of identityReport leaves.
func wantStoredOnce(t *testing.T, e *testEnv, a *models.Agent, tdb *modules.DB, count int64, dropped int64, history int) {
	t.Helper()
	if n := detectionCount(t, tdb); n != count {
		t.Fatalf("detection count %d, want %d", n, count)
	}
	cur, err := models.GetAgent(e.c.db.Read(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.DroppedTotal != dropped {
		t.Fatalf("dropped_total %d, want %d", cur.DroppedTotal, dropped)
	}
	if n := droppedHistory(t, e); n != history {
		t.Fatalf("history rows dropped %d, want %d", n, history)
	}
	var runs int
	if err := tdb.Read().QueryRow(`SELECT count(*) FROM command_runs`).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("runs %d %v", runs, err)
	}
}

const (
	reportID1 = "0123456789abcdef0123456789abcdef"
	reportID2 = "fedcba9876543210fedcba9876543210"
)

// The same batch twice (its answer was lost) counts its detections and its
// dropped count once and writes its history once; the second answer is 200
// {}; a different batch after it is stored.
func TestReportStoredOnce(t *testing.T) {
	e := newTestEnv(t)
	tok, a, tdb, setID, logs := identityAgent(t, e, "host-once")
	rep := identityReport(reportID1, setID)
	if rec := postReport(e, tok, rep); rec.Code != 200 {
		t.Fatalf("first answer: %d %s", rec.Code, rec.Body.String())
	}
	rec := postReport(e, tok, rep)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("second answer: %d %s", rec.Code, rec.Body.String())
	}
	wantStoredOnce(t, e, a, tdb, 3, 2, 1)
	if !strings.Contains(logs(), "report "+reportID1+" of host-once was already stored; answered without storing it again") {
		t.Fatalf("log: %q", logs())
	}
	if got, err := models.LastReceivedReport(tdb.Read()); err != nil || got != reportID1 {
		t.Fatalf("remembered report: %q %v", got, err)
	}

	// A different batch after it is stored and remembered.
	if rec := postReport(e, tok, identityReport(reportID2, setID)); rec.Code != 200 {
		t.Fatalf("answer: %d %s", rec.Code, rec.Body.String())
	}
	wantStoredOnce(t, e, a, tdb, 6, 4, 2)
	if got, _ := models.LastReceivedReport(tdb.Read()); got != reportID2 {
		t.Fatalf("remembered report: %q", got)
	}
}

// Two copies of one batch arriving at once (the agent gave up waiting and
// sent it again) are stored once.
func TestReportStoredOnceConcurrent(t *testing.T) {
	e := newTestEnv(t)
	tok, a, tdb, setID, _ := identityAgent(t, e, "host-conc")
	rep := identityReport(reportID1, setID)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = postReport(e, tok, rep).Code
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("copy %d answered %d", i, c)
		}
	}
	wantStoredOnce(t, e, a, tdb, 3, 2, 1)
}

// The built-in agent's direct call takes the same path.
func TestReportIdentityBuiltin(t *testing.T) {
	e := newTestEnv(t)
	b, err := models.GetBuiltinAgent(e.c.db.Read())
	if err != nil {
		t.Fatal(err)
	}
	tdb, err := e.c.targetDB(b)
	if err != nil {
		t.Fatal(err)
	}
	body := commandSetBody("s", "collect_output")
	body["os_user"] = "svc"
	setID := int64(e.admin("POST", "/agents/"+itoa64(b.ID)+"/command-sets", body)["id"].(float64))
	tr := &inProcessTransport{c: e.c, agentID: b.ID}
	for range 2 {
		if err := tr.Report(identityReport(reportID1, setID)); err != nil {
			t.Fatal(err)
		}
	}
	wantStoredOnce(t, e, b, tdb, 3, 2, 1)
	// The built-in agent's batch without a valid identity is refused too.
	if err := tr.Report(&modules.ReportBatch{Reports: []modules.ReportRequest{}}); !errors.Is(err, errInvalidReport) {
		t.Fatalf("batch without identity: %v", err)
	}
}

// Only a batch with a valid identity is taken: a body without an identity,
// with one not of its form, without the list of items, or in the form of a
// single report is answered 400 and nothing of it is stored.
func TestReportOnlyBatchWithIdentity(t *testing.T) {
	e := newTestEnv(t)
	tok, a, tdb, setID, _ := identityAgent(t, e, "host-noid")
	item := identityReport(reportID1, setID).Reports[0]
	single, _ := json.Marshal(item)
	singleWithID := strings.Replace(string(single), "{", `{"report_id":"`+reportID1+`",`, 1)
	bodies := []any{
		&modules.ReportBatch{Reports: []modules.ReportRequest{item}},
		&modules.ReportBatch{ReportID: strings.ToUpper(reportID1), Reports: []modules.ReportRequest{item}},
		&modules.ReportBatch{ReportID: reportID1[:31], Reports: []modules.ReportRequest{item}},
		&modules.ReportBatch{ReportID: reportID1 + "0", Reports: []modules.ReportRequest{item}},
		json.RawMessage(`{"report_id":"` + reportID1 + `"}`),
		json.RawMessage(`{"report_id":"` + reportID1 + `","reports":null}`),
		json.RawMessage(single),
		json.RawMessage(singleWithID),
		json.RawMessage(`[{"report_id":"` + reportID1 + `","reports":[]}]`),
	}
	for i, body := range bodies {
		rec := postReport(e, tok, body)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"code":"common.invalid_request"`) {
			t.Fatalf("body %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if n := detectionCount(t, tdb); n != 0 {
		t.Fatalf("detections stored: %d", n)
	}
	var runs int
	if err := tdb.Read().QueryRow(`SELECT count(*) FROM command_runs`).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("runs %d %v", runs, err)
	}
	cur, _ := models.GetAgent(e.c.db.Read(), a.ID)
	if cur.DroppedTotal != 0 || cur.LastReportAt != 0 || cur.LastCentralReportID != "" || droppedHistory(t, e) != 0 {
		t.Fatalf("agent row: %+v", cur)
	}
	if got, _ := models.LastReceivedReport(tdb.Read()); got != "" {
		t.Fatalf("remembered report: %q", got)
	}
}

// A batch without items is a report: it is answered 200, remembered in both
// parts and records the time of the report.
func TestReportEmptyBatch(t *testing.T) {
	e := newTestEnv(t)
	tok, a, tdb, _, _ := identityAgent(t, e, "host-empty")
	if rec := postReport(e, tok, json.RawMessage(`{"report_id":"`+reportID1+`","stopping":false,"reports":[]}`)); rec.Code != 200 {
		t.Fatalf("answer: %d %s", rec.Code, rec.Body.String())
	}
	cur, _ := models.GetAgent(e.c.db.Read(), a.ID)
	if cur.LastCentralReportID != reportID1 || cur.LastReportAt == 0 {
		t.Fatalf("agent row: %+v", cur)
	}
	if got, _ := models.LastReceivedReport(tdb.Read()); got != reportID1 {
		t.Fatalf("remembered report: %q", got)
	}
}

// The items of one batch are stored in order, each on what the item before
// it left: two application results each get their history row, detections
// of the same kind are aggregated, the dropped counts add up, and the host
// information of the last item that carries it stays.
func TestReportBatchItemsInOrder(t *testing.T) {
	e := newTestEnv(t)
	tok, a, tdb, _, _ := identityAgent(t, e, "host-order")
	det := modules.DetectionReport{Type: "exec", Category: "alert", OSUser: "nginx", Exe: "/usr/bin/curl", Target: "twice", Count: 1, FirstAt: 1000, LastAt: 1000}
	batch := &modules.ReportBatch{ReportID: reportID1, Reports: []modules.ReportRequest{
		{Detections: []modules.DetectionReport{det}, Dropped: 1, HostInfo: &modules.HostInfo{Hostname: "first"},
			Apply: &modules.ApplyResult{Status: "error", Message: "validation failed: x", At: 100}},
		{Detections: []modules.DetectionReport{det}, Dropped: 2},
		{Detections: []modules.DetectionReport{det}, HostInfo: &modules.HostInfo{Hostname: "last"},
			Apply: &modules.ApplyResult{Status: modules.ApplyStatusNone, Message: "nothing applied", At: 200}},
	}}
	if rec := postReport(e, tok, batch); rec.Code != 200 {
		t.Fatalf("answer: %d %s", rec.Code, rec.Body.String())
	}
	if n := detectionCount(t, tdb); n != 3 {
		t.Fatalf("detection count %d", n)
	}
	cur, _ := models.GetAgent(e.c.db.Read(), a.ID)
	if cur.DroppedTotal != 3 || droppedHistory(t, e) != 2 || cur.Hostname != "last" || cur.ApplyStatus != modules.ApplyStatusNone {
		t.Fatalf("agent row: dropped %d (history %d) host %q apply %q", cur.DroppedTotal, droppedHistory(t, e), cur.Hostname, cur.ApplyStatus)
	}
	items, _, err := models.ListHistory(e.c.db.Read(), models.HistRule, "", nil, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, h := range items {
		actions = append(actions, h.Action)
	}
	slices.Sort(actions)
	if !slices.Equal(actions, []string{"apply_result_error", "apply_result_none"}) {
		t.Fatalf("history of the application results: %v", actions)
	}
}

// The order of failures: the part in the agent's database is committed
// before the part in central.db runs. When the central fails between the
// two, the agent gets no success and sends the report again; the repeat
// does not store the part in the agent's database again, but stores the
// part in central.db that was lost: the transitions of the run requests,
// the dropped count with its history row and the outcome of storing the
// command set definitions. A third copy stores nothing more.
func TestReportRepeatAfterLostCentralStep(t *testing.T) {
	e := newTestEnv(t)
	tok, a, tdb, setID, logs := identityAgent(t, e, "host-lost")
	syncWith(t, e, tok, modules.SyncRequest{})
	reqID, _ := requestRun(t, e, a, setID)
	if got := handedIDs(syncWith(t, e, tok, modules.SyncRequest{})); len(got) != 1 || got[0] != reqID {
		t.Fatalf("handed over: %v", got)
	}
	rep := identityReport(reportID1, setID)
	item := &rep.Reports[0]
	item.CommandRuns[0].RequestID, item.CommandRuns[0].Trigger = reqID, modules.TriggerManual
	item.CommandRuns[0].RunUID = "lost-run"
	item.CommandRequestAcks = []modules.CommandRequestAck{{RequestID: reqID, Status: modules.RequestAckStarted, RunUID: "lost-run", At: 1000}}
	item.CommandApply = &modules.CommandApplyResult{Signature: "sig-lost", At: 1000,
		Problems: []modules.CommandApplyProblem{{SetID: 99, SetName: "broken", Problems: []string{"broken"}}}}

	failed := false
	e.c.reportStored = func(int64) error {
		if !failed {
			failed = true
			return errors.New("central part lost")
		}
		return nil
	}
	if rec := postReport(e, tok, rep); rec.Code != 500 {
		t.Fatalf("first answer: %d %s", rec.Code, rec.Body.String())
	}
	if st := reqState(t, e, a.ID, reqID); st != models.RequestDelivered {
		t.Fatalf("request after the lost central part: %s", st)
	}
	if n := detectionCount(t, tdb); n != 3 {
		t.Fatalf("detections after the lost central part: %d", n)
	}
	if cur, _ := models.GetAgent(e.c.db.Read(), a.ID); cur.DroppedTotal != 0 || cur.CommandProblems != "" || cur.LastCentralReportID != "" {
		t.Fatalf("agent row after the lost central part: %+v", cur)
	}
	if rec := postReport(e, tok, rep); rec.Code != 200 {
		t.Fatalf("repeat: %d %s", rec.Code, rec.Body.String())
	}
	if st := reqState(t, e, a.ID, reqID); st != models.RequestDone {
		t.Fatalf("request after the repeat: %s", st)
	}
	wantStoredOnce(t, e, a, tdb, 3, 2, 1)
	cur, _ := models.GetAgent(e.c.db.Read(), a.ID)
	if cur.CommandSig != "sig-lost" || !strings.Contains(cur.CommandProblems, `"broken"`) || cur.LastCentralReportID != reportID1 {
		t.Fatalf("agent row after the repeat: sig %q problems %q id %q", cur.CommandSig, cur.CommandProblems, cur.LastCentralReportID)
	}
	if !strings.Contains(logs(), "report "+reportID1+" of host-lost was stored in the agent's database only") {
		t.Fatalf("log: %q", logs())
	}
	// A third copy: both parts were stored, nothing is stored again.
	if rec := postReport(e, tok, rep); rec.Code != 200 {
		t.Fatalf("third copy: %d %s", rec.Code, rec.Body.String())
	}
	wantStoredOnce(t, e, a, tdb, 3, 2, 1)
	if !strings.Contains(logs(), "report "+reportID1+" of host-lost was already stored") {
		t.Fatalf("log: %q", logs())
	}
}
