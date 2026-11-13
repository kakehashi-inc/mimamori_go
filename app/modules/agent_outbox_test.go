package modules

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"mimamori/app/models"
)

var reportIDForm = regexp.MustCompile(`^[0-9a-f]{32}$`)

// queuedIDs returns the report IDs of the queued rows, oldest first.
func queuedIDs(t *testing.T, rt *AgentRuntime) []string {
	t.Helper()
	var ids []string
	for _, r := range queuedReports(t, rt) {
		ids = append(ids, r.ReportID)
	}
	return ids
}

// sentIDs returns the report IDs a fake central received, in order.
func sentIDs(reports []*ReportRequest) []string {
	var ids []string
	for _, r := range reports {
		ids = append(ids, r.ReportID)
	}
	return ids
}

// storingTransport is a fake central that stores every report it receives
// and, while failAnswer is set, loses the answer (the report is stored, the
// agent sees an error).
type storingTransport struct {
	stored     []*ReportRequest
	failAnswer bool
}

func (f *storingTransport) Sync(*SyncRequest) (*SyncResponse, error) {
	return &SyncResponse{}, nil
}

func (f *storingTransport) Report(r *ReportRequest) error {
	c := *r
	f.stored = append(f.stored, &c)
	if f.failAnswer {
		return errors.New("the answer was lost")
	}
	return nil
}

// TestReportIDOnEveryRow: every queued report, periodic or about command
// sets, gets its own identity of 32 lowercase hexadecimal characters, and
// the central receives it.
func TestReportIDOnEveryRow(t *testing.T) {
	tr := &fakeTransport{fail: true}
	rt := newCommandRuntime(t, tr)
	rt.queueCommandReport(&ReportRequest{CommandRequestAcks: []CommandRequestAck{{RequestID: 1, Status: RequestAckExpired}}})
	rt.queueCommandReport(&ReportRequest{CommandRuns: []CommandRunReport{{RunUID: "r", Purpose: PurposeRunOnly}}})
	if err := queueReport(rt, &ReportRequest{Detections: []DetectionReport{{Type: TypeExec, Category: CategoryAlert}}}); err != nil {
		t.Fatal(err)
	}
	if err := queueReport(rt, &ReportRequest{HostInfo: &HostInfo{Hostname: "h"}}); err != nil {
		t.Fatal(err)
	}
	ids := queuedIDs(t, rt)
	if len(ids) != 4 {
		t.Fatalf("rows: %v", ids)
	}
	for i, id := range ids {
		if !reportIDForm.MatchString(id) {
			t.Fatalf("row %d: report id %q", i, id)
		}
		if slices.Index(ids, id) != i {
			t.Fatalf("report ids must differ: %v", ids)
		}
	}
	tr.fail = false
	rt.flushOutbox()
	if got := sentIDs(tr.reports); !slices.Equal(got, ids) {
		t.Fatalf("sent %v, queued %v", got, ids)
	}
	// The identity is part of the request body.
	b, _ := json.Marshal(tr.reports[0])
	if !strings.Contains(string(b), `"report_id":"`+ids[0]+`"`) {
		t.Fatalf("body: %s", b)
	}
}

// TestReportIDKeptWhenShrunk: the limits and the removal of host information
// rewrite queued rows; the identity of each row stays.
func TestReportIDKeptWhenShrunk(t *testing.T) {
	rt := newCommandRuntime(t, &fakeTransport{fail: true})
	big := strings.Repeat("x", 4000)
	rt.out.limit = 1 << 30
	rt.queueCommandReport(&ReportRequest{CommandRuns: []CommandRunReport{{RunUID: "a", Purpose: PurposeRunOnly,
		Results: []CommandResultReport{{Output: []byte(big), OutputBytes: 4000}}}}})
	var ds []DetectionReport
	for i := 0; i < DefaultOutboxMaxDetection; i++ {
		ds = append(ds, DetectionReport{Type: TypeExec, Category: CategoryNormal, Target: "n"})
	}
	if err := queueReport(rt, &ReportRequest{Detections: ds, HostInfo: &HostInfo{Hostname: "h"}}); err != nil {
		t.Fatal(err)
	}
	before := queuedIDs(t, rt)

	// Over the command limit: the output of the first row is dropped.
	rt.out.limit = models.OutboxCommandBytes(rt.DB.Read()) - 1000
	rt.queueCommandReport(&ReportRequest{CommandRequestAcks: []CommandRequestAck{{RequestID: 2, Status: RequestAckExpired}}})
	// Over the detection limit, and the host information moves to the new
	// row: the second row loses detections and its host information.
	if err := queueReport(rt, &ReportRequest{Detections: []DetectionReport{{Type: TypeExec, Category: CategoryAlert}}}); err != nil {
		t.Fatal(err)
	}
	reports := queuedReports(t, rt)
	if len(reports) != 4 {
		t.Fatalf("rows: %d", len(reports))
	}
	if out := reports[0].CommandRuns[0].Results[0]; len(out.Output) != 0 || !out.Truncated {
		t.Fatalf("the first row must have lost its output: %+v", out)
	}
	if len(reports[1].Detections) != DefaultOutboxMaxDetection-1 || reports[1].HostInfo != nil {
		t.Fatalf("the second row must have lost a detection and its host information: %d, %v",
			len(reports[1].Detections), reports[1].HostInfo)
	}
	if got := queuedIDs(t, rt)[:2]; !slices.Equal(got, before) {
		t.Fatalf("identities after the rewrite %v, before %v", got, before)
	}
}

// TestReportIDKeptWhenSentAgain: a report the central stored but whose
// answer was lost stays queued and is sent again with the same identity,
// also after the agent was restarted on the same local database.
func TestReportIDKeptWhenSentAgain(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := OpenSQLite(path, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	tr := &storingTransport{failAnswer: true}
	rt := NewAgentRuntime(db, tr, nil)
	rt.queueCommandReport(&ReportRequest{CommandRequestAcks: []CommandRequestAck{{RequestID: 1, Status: RequestAckExpired}}})
	rt.queueCommandReport(&ReportRequest{CommandRequestAcks: []CommandRequestAck{{RequestID: 2, Status: RequestAckExpired}}})
	queued := queuedIDs(t, rt)
	rt.flushOutbox()
	rt.flushOutbox()
	// Each attempt stops at the first row, whose answer was lost.
	if got := sentIDs(tr.stored); !slices.Equal(got, []string{queued[0], queued[0]}) {
		t.Fatalf("sent %v, queued %v", got, queued)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: the rows and their identities are read back from the file.
	db, err = OpenSQLite(path, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tr.failAnswer = false
	rt = NewAgentRuntime(db, tr, nil)
	rt.flushOutbox()
	if got := sentIDs(tr.stored); !slices.Equal(got, []string{queued[0], queued[0], queued[0], queued[1]}) {
		t.Fatalf("sent %v, queued %v", got, queued)
	}
	if n := models.CountOutbox(db.Read()); n != 0 {
		t.Fatalf("answered rows must be deleted: %d left", n)
	}
	// A new report gets a new identity.
	rt.queueCommandReport(&ReportRequest{CommandRequestAcks: []CommandRequestAck{{RequestID: 3, Status: RequestAckExpired}}})
	if id := queuedIDs(t, rt)[0]; slices.Contains(queued, id) {
		t.Fatalf("a new row reused the identity %s", id)
	}
}

// TestReportIDRecoveredRun: the report of a run found unfinished at start
// carries an identity too.
func TestReportIDRecoveredRun(t *testing.T) {
	rt := newCommandRuntime(t, &fakeTransport{fail: true})
	insertExecution(t, rt.DB, "u1", 7, 0)
	rt.recoverInterruptedRuns()
	reports := queuedReports(t, rt)
	if len(reports) != 1 || !reportIDForm.MatchString(reports[0].ReportID) || reports[0].CommandRuns[0].RunUID != "u1" {
		t.Fatalf("queued: %+v", reports)
	}
}

// insertExecution records a started run; finishedAt 0 leaves it running.
func insertExecution(t *testing.T, db *DB, uid string, requestID, finishedAt int64) {
	t.Helper()
	trigger := TriggerManual
	if requestID == 0 {
		trigger = TriggerSchedule
	}
	err := db.Write(func(tx models.Writer) error {
		if err := models.InsertCommandExecution(tx, &models.CommandExecution{RunUID: uid, SetID: 1, SetRevision: 1,
			RequestID: requestID, Trigger: trigger, StartedAt: time.Now().Unix() - 10}); err != nil {
			return err
		}
		if finishedAt > 0 {
			return models.FinishCommandExecution(tx, uid, finishedAt)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestSyncRunningRequests: the sync lists the manual runs that are started
// and not finished, read from the local database at the sync, and sends an
// empty list (never null) when none runs.
func TestSyncRunningRequests(t *testing.T) {
	tr := &cmdTransport{}
	rt := newCommandRuntime(t, tr)
	rt.Cycle()
	if got := tr.syncReqs[0].RunningRequests; got == nil || len(got) != 0 {
		t.Fatalf("nothing runs: %#v", got)
	}
	b, _ := json.Marshal(&tr.syncReqs[0])
	if !strings.Contains(string(b), `"running_requests":[]`) {
		t.Fatalf("body: %s", b)
	}
	insertExecution(t, rt.DB, "running", 5, 0)
	insertExecution(t, rt.DB, "finished", 6, time.Now().Unix())
	insertExecution(t, rt.DB, "scheduled", 0, 0)
	insertExecution(t, rt.DB, "running2", 9, 0)
	rt.Cycle()
	got := slices.Clone(tr.syncReqs[1].RunningRequests)
	slices.Sort(got)
	if !slices.Equal(got, []int64{5, 9}) {
		t.Fatalf("running requests: %v", got)
	}
}

// TestSyncRunningRequestsDuringRun runs the whole flow: the syncs made while
// a manual run is going on list its request, those before and after do not.
func TestSyncRunningRequestsDuringRun(t *testing.T) {
	requireBash(t)
	tr := &cmdTransport{}
	rt := newCommandRuntime(t, tr)
	now := time.Now().Unix()
	tr.resp.DefaultIntervalSec = 1
	tr.resp.CommandSets = defsOf(testSet(1, PurposeRunOnly, "sleep 2.5"))
	tr.resp.CommandRequests = []CommandRunRequest{{ID: 41, SetID: 1, RequestedAt: now, ExpiresAt: now + 3600}}
	rt.defaultInterval = 1
	rt.lastMonitor = time.Time{}
	rt.Run(stopWhenRunsFinished(t, rt.DB, 1))
	rt.Cycle()
	n := len(tr.syncReqs)
	if n < 3 {
		t.Fatalf("syncs: %d", n)
	}
	if first := tr.syncReqs[0].RunningRequests; first == nil || len(first) != 0 {
		t.Fatalf("the first sync comes before the run: %#v", first)
	}
	during := 0
	for _, s := range tr.syncReqs[1 : n-1] {
		if slices.Equal(s.RunningRequests, []int64{41}) {
			during++
		}
	}
	if during == 0 {
		t.Fatalf("no sync during the run listed the request: %+v", tr.syncReqs)
	}
	if last := tr.syncReqs[n-1].RunningRequests; last == nil || len(last) != 0 {
		t.Fatalf("the sync after the run: %#v", last)
	}
}

// TestHostInfoSentFromItsRow: the host information stays in the newest
// periodic row also when command set reports were queued after it, and is
// sent with that row.
func TestHostInfoSentFromItsRow(t *testing.T) {
	tr := &fakeTransport{fail: true}
	rt := newCommandRuntime(t, tr)
	if err := queueReport(rt, &ReportRequest{HostInfo: &HostInfo{Hostname: "old"},
		Detections: []DetectionReport{{Type: TypeExec, Category: CategoryAlert}}}); err != nil {
		t.Fatal(err)
	}
	if err := queueReport(rt, &ReportRequest{HostInfo: &HostInfo{Hostname: "new"}}); err != nil {
		t.Fatal(err)
	}
	rt.queueCommandReport(&ReportRequest{CommandRequestAcks: []CommandRequestAck{{RequestID: 1, Status: RequestAckExpired}}})
	tr.fail = false
	rt.flushOutbox()
	var hosts []string
	for _, r := range tr.reports {
		if r.HostInfo != nil {
			hosts = append(hosts, r.HostInfo.Hostname)
		}
	}
	if len(tr.reports) != 3 || !slices.Equal(hosts, []string{"new"}) {
		t.Fatalf("%d reports, host information %v", len(tr.reports), hosts)
	}
}
