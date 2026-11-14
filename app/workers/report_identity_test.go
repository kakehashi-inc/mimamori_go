package workers

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// The identity of reports on the central's side: the last report stored for
// an agent is remembered (report_id) in the transaction that stores it, and
// the same report sent again is answered as stored without being stored or
// counted again.

// identityReport is a report with a detection of count 3, a dropped count of
// 2 and a finished scheduled run of set.
func identityReport(id string, setID int64) modules.ReportRequest {
	return modules.ReportRequest{
		ReportID: id,
		Detections: []modules.DetectionReport{{Type: "exec", Category: "alert", OSUser: "nginx", Exe: "/usr/bin/curl",
			Target: "twice", Count: 3, FirstAt: 1000, LastAt: 1001}},
		Dropped:     2,
		CommandRuns: []modules.CommandRunReport{runReport("id-run", setID, "s", 0, []modules.CommandResultReport{}, true, modules.ResultSuccess)},
	}
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

// The same report twice counts its detections and its dropped count once and
// writes its history once; the second answer is 200 {}; a different report
// after it is stored.
func TestReportStoredOnce(t *testing.T) {
	e := newTestEnv(t)
	tok, a, tdb, setID, logs := identityAgent(t, e, "host-once")
	rep := identityReport(reportID1, setID)
	reportAgent(t, e, tok, rep)
	rec := e.agentCall("report", tok, "m-"+tok[len(tok)-6:], rep)
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

	// A different report after it is stored and remembered.
	reportAgent(t, e, tok, identityReport(reportID2, setID))
	wantStoredOnce(t, e, a, tdb, 6, 4, 2)
	if got, _ := models.LastReceivedReport(tdb.Read()); got != reportID2 {
		t.Fatalf("remembered report: %q", got)
	}
}

// Two copies of one report arriving at once (the agent gave up waiting and
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
			codes[i] = e.agentCall("report", tok, "m-"+tok[len(tok)-6:], rep).Code
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
		rep := identityReport(reportID1, setID)
		if err := tr.Report(&rep); err != nil {
			t.Fatal(err)
		}
	}
	wantStoredOnce(t, e, b, tdb, 3, 2, 1)
}

// A report without an identity, or with one not of its form, is stored every
// time it comes and not remembered.
func TestReportWithoutID(t *testing.T) {
	e := newTestEnv(t)
	tok, a, tdb, setID, _ := identityAgent(t, e, "host-noid")
	for _, id := range []string{"", "", strings.ToUpper(reportID1), strings.ToUpper(reportID1), reportID1[:31]} {
		reportAgent(t, e, tok, identityReport(id, setID))
	}
	wantStoredOnce(t, e, a, tdb, 15, 10, 5)
	if got, _ := models.LastReceivedReport(tdb.Read()); got != "" {
		t.Fatalf("remembered report: %q", got)
	}
}

// The order of failures: the part in the agent's database is committed
// before the part in central.db runs. When the central fails between the
// two, the agent gets no success and sends the report again; the repeat is
// not stored again, but it applies the transitions of the run requests, so
// the request ends. The report's dropped count is lost (it was to be
// recorded in the central part).
func TestReportRepeatAfterLostCentralStep(t *testing.T) {
	e := newTestEnv(t)
	tok, a, tdb, setID, logs := identityAgent(t, e, "host-lost")
	syncWith(t, e, tok, modules.SyncRequest{})
	reqID, _ := requestRun(t, e, a, setID)
	if got := handedIDs(syncWith(t, e, tok, modules.SyncRequest{})); len(got) != 1 || got[0] != reqID {
		t.Fatalf("handed over: %v", got)
	}
	rep := identityReport(reportID1, setID)
	rep.CommandRuns[0].RequestID, rep.CommandRuns[0].Trigger = reqID, modules.TriggerManual
	rep.CommandRuns[0].RunUID = "lost-run"
	rep.CommandRequestAcks = []modules.CommandRequestAck{{RequestID: reqID, Status: modules.RequestAckStarted, RunUID: "lost-run", At: 1000}}

	failed := false
	e.c.reportStored = func(int64) error {
		if !failed {
			failed = true
			return errors.New("central part lost")
		}
		return nil
	}
	if rec := e.agentCall("report", tok, "m-"+tok[len(tok)-6:], rep); rec.Code != 500 {
		t.Fatalf("first answer: %d %s", rec.Code, rec.Body.String())
	}
	if st := reqState(t, e, a.ID, reqID); st != models.RequestDelivered {
		t.Fatalf("request after the lost central part: %s", st)
	}
	if n := detectionCount(t, tdb); n != 3 {
		t.Fatalf("detections after the lost central part: %d", n)
	}
	reportAgent(t, e, tok, rep)
	if st := reqState(t, e, a.ID, reqID); st != models.RequestDone {
		t.Fatalf("request after the repeat: %s", st)
	}
	wantStoredOnce(t, e, a, tdb, 3, 0, 0)
	if !strings.Contains(logs(), "report "+reportID1+" of host-lost was already stored") {
		t.Fatalf("log: %q", logs())
	}
}
