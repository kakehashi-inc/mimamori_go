package modules

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"mimamori/app/models"
)

var reportIDForm = regexp.MustCompile(`^[0-9a-f]{32}$`)

// storingTransport is a fake central that stores every batch it receives
// and, while failAnswer is set, loses the answer (the batch is stored, the
// agent sees an error).
type storingTransport struct {
	stored     []ReportBatch
	failAnswer bool
}

func (f *storingTransport) Sync(*SyncRequest) (*SyncResponse, error) {
	return &SyncResponse{}, nil
}

func (f *storingTransport) Report(b *ReportBatch) error {
	f.stored = append(f.stored, *b)
	if f.failAnswer {
		return errors.New("the answer was lost")
	}
	return nil
}

// batchIDs returns the identities of the batches a fake central received.
func batchIDs(batches []ReportBatch) []string {
	var ids []string
	for _, b := range batches {
		ids = append(ids, b.ReportID)
	}
	return ids
}

// ackIDs returns the request IDs of the answers the items of a batch carry,
// in order.
func ackIDs(b ReportBatch) []int64 {
	var ids []int64
	for _, r := range b.Reports {
		for _, a := range r.CommandRequestAcks {
			ids = append(ids, a.RequestID)
		}
	}
	return ids
}

// queueAck queues a report about command sets that answers request id.
func queueAck(rt *AgentRuntime, id int64) {
	rt.queueCommandReport(&ReportRequest{CommandRequestAcks: []CommandRequestAck{{RequestID: id, Status: RequestAckExpired}}})
}

// recordedBatch returns the record of the batch being sent (zero when none).
func recordedBatch(t *testing.T, db *DB) outboxBatch {
	t.Helper()
	var rec outboxBatch
	if v := models.GetLocalState(db.Read(), stateOutboxBatch); v != "" {
		if err := json.Unmarshal([]byte(v), &rec); err != nil {
			t.Fatal(err)
		}
	}
	return rec
}

// TestReportBatchOneRequest: whatever number of items is queued, periodic or
// about command sets, a send is one request: one batch with every item,
// oldest first, under one identity of 32 lowercase hexadecimal characters;
// the items carry no identity of their own. After the answer the items are
// deleted and the record of the batch cleared.
func TestReportBatchOneRequest(t *testing.T) {
	tr := &fakeTransport{}
	rt := newCommandRuntime(t, tr)
	for id := int64(1); id <= 30; id++ {
		queueAck(rt, id)
	}
	rt.queueCommandReport(&ReportRequest{CommandRuns: []CommandRunReport{{RunUID: "r", Purpose: PurposeRunOnly}}})
	if err := queueReport(rt, &ReportRequest{Detections: []DetectionReport{{Type: TypeExec, Category: CategoryAlert}},
		HostInfo: &HostInfo{Hostname: "h"}}); err != nil {
		t.Fatal(err)
	}
	rt.flushOutbox()
	if len(tr.batches) != 1 {
		t.Fatalf("report requests: %d, want 1", len(tr.batches))
	}
	b := tr.batches[0]
	if !reportIDForm.MatchString(b.ReportID) || b.Stopping || len(b.Reports) != 32 {
		t.Fatalf("batch %q stopping %v with %d items", b.ReportID, b.Stopping, len(b.Reports))
	}
	if got := ackIDs(*b); len(got) != 30 || got[0] != 1 || got[29] != 30 || b.Reports[31].HostInfo == nil {
		t.Fatalf("items out of order: %v", got)
	}
	body, _ := json.Marshal(b)
	if !strings.HasPrefix(string(body), `{"report_id":"`+b.ReportID+`","stopping":false,"reports":[{`) ||
		strings.Count(string(body), `"report_id"`) != 1 {
		t.Fatalf("body: %.200s", body)
	}
	if n := models.CountOutbox(rt.DB.Read()); n != 0 {
		t.Fatalf("items left: %d", n)
	}
	if rec := recordedBatch(t, rt.DB); rec.ReportID != "" {
		t.Fatalf("record left: %+v", rec)
	}
}

// TestReportBatchEmpty: with nothing queued a send is a batch without items
// (an empty list, not null) under a new identity.
func TestReportBatchEmpty(t *testing.T) {
	tr := &fakeTransport{}
	rt := newCommandRuntime(t, tr)
	rt.flushOutbox()
	rt.flushOutbox()
	if len(tr.batches) != 2 || tr.batches[0].ReportID == tr.batches[1].ReportID {
		t.Fatalf("batches: %+v", tr.batches)
	}
	for _, b := range tr.batches {
		body, _ := json.Marshal(b)
		if !reportIDForm.MatchString(b.ReportID) || !strings.HasSuffix(string(body), `"reports":[]}`) {
			t.Fatalf("body: %s", body)
		}
	}
	if rec := recordedBatch(t, rt.DB); rec.ReportID != "" {
		t.Fatalf("record left: %+v", rec)
	}
}

// TestReportBatchSentAgain: a batch the central stored but whose answer was
// lost stays recorded and is sent again with the same identity and the same
// items, also after the agent was restarted on the same local database;
// items queued meanwhile do not join it and go with the next batch, under a
// new identity.
func TestReportBatchSentAgain(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := OpenSQLite(path, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	tr := &storingTransport{failAnswer: true}
	rt := NewAgentRuntime(db, tr, nil)
	queueAck(rt, 1)
	queueAck(rt, 2)
	rt.flushOutbox()
	queueAck(rt, 3)
	rt.flushOutbox()
	ids := batchIDs(tr.stored)
	if len(ids) != 2 || ids[0] != ids[1] || !reportIDForm.MatchString(ids[0]) {
		t.Fatalf("sent %v", ids)
	}
	for _, b := range tr.stored {
		if got := ackIDs(b); !slices.Equal(got, []int64{1, 2}) {
			t.Fatalf("batch items %v, want [1 2]", got)
		}
	}
	if rec := recordedBatch(t, db); rec.ReportID != ids[0] || len(rec.Items) != 2 {
		t.Fatalf("record: %+v", rec)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: the batch and its items are read back from the file.
	db, err = OpenSQLite(path, SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tr.failAnswer = false
	rt = NewAgentRuntime(db, tr, nil)
	rt.flushOutbox()
	if got := tr.stored[2]; got.ReportID != ids[0] || !slices.Equal(ackIDs(got), []int64{1, 2}) {
		t.Fatalf("after the restart: %s %v", got.ReportID, ackIDs(got))
	}
	if n := models.CountOutbox(db.Read()); n != 1 {
		t.Fatalf("items left: %d, want the one queued later", n)
	}
	rt.flushOutbox()
	if got := tr.stored[3]; got.ReportID == ids[0] || !slices.Equal(ackIDs(got), []int64{3}) {
		t.Fatalf("next batch: %s %v", got.ReportID, ackIDs(got))
	}
	if n := models.CountOutbox(db.Read()); n != 0 || recordedBatch(t, db).ReportID != "" {
		t.Fatalf("items left %d, record %+v", n, recordedBatch(t, db))
	}
}

// TestReportBatchDeleteFails: when the items of an answered batch cannot be
// deleted, the batch stays recorded and is sent again with the same identity
// (the central stores it once), and is deleted after that answer.
func TestReportBatchDeleteFails(t *testing.T) {
	tr := &storingTransport{}
	rt := newCommandRuntime(t, tr)
	queueAck(rt, 1)
	queueAck(rt, 2)
	if err := rt.DB.Write(func(tx models.Writer) error {
		_, err := tx.Exec(`CREATE TRIGGER keep_outbox BEFORE DELETE ON outbox BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rt.flushOutbox()
	if n := models.CountOutbox(rt.DB.Read()); n != 2 {
		t.Fatalf("items after the failed delete: %d", n)
	}
	if err := rt.DB.Write(func(tx models.Writer) error { _, err := tx.Exec(`DROP TRIGGER keep_outbox`); return err }); err != nil {
		t.Fatal(err)
	}
	queueAck(rt, 3)
	rt.flushOutbox()
	ids := batchIDs(tr.stored)
	if len(ids) != 2 || ids[0] != ids[1] || !slices.Equal(ackIDs(tr.stored[1]), []int64{1, 2}) {
		t.Fatalf("sent %v with %v", ids, ackIDs(tr.stored[1]))
	}
	rt.flushOutbox()
	if got := tr.stored[2]; got.ReportID == ids[0] || !slices.Equal(ackIDs(got), []int64{3}) {
		t.Fatalf("next batch: %s %v", got.ReportID, ackIDs(got))
	}
}

// TestReportBatchItemsAsTheyAreNow: the limits and the removal of host
// information rewrite or delete items of a recorded batch; the batch is sent
// again with the same identity and its items as they are now.
func TestReportBatchItemsAsTheyAreNow(t *testing.T) {
	tr := &storingTransport{failAnswer: true}
	rt := newCommandRuntime(t, tr)
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
	// A periodic item with nothing else to deliver: the next periodic report
	// deletes it.
	if err := queueReport(rt, &ReportRequest{HostInfo: &HostInfo{Hostname: "h2"}}); err != nil {
		t.Fatal(err)
	}
	rt.flushOutbox()
	first := tr.stored[0]
	if len(first.Reports) != 3 {
		t.Fatalf("first batch: %d items", len(first.Reports))
	}

	// Over the command limit: the output of the first item is dropped. Over
	// the detection limit, and the host information moves to the new item:
	// the second item loses a detection and its host information; the third
	// is deleted.
	rt.out.limit = models.OutboxCommandBytes(rt.DB.Read()) - 1000
	queueAck(rt, 2)
	if err := queueReport(rt, &ReportRequest{Detections: []DetectionReport{{Type: TypeExec, Category: CategoryAlert}},
		HostInfo: &HostInfo{Hostname: "h3"}}); err != nil {
		t.Fatal(err)
	}
	rt.flushOutbox()
	again := tr.stored[1]
	if again.ReportID != first.ReportID || len(again.Reports) != 2 {
		t.Fatalf("sent again: %s with %d items", again.ReportID, len(again.Reports))
	}
	if out := again.Reports[0].CommandRuns[0].Results[0]; len(out.Output) != 0 || !out.Truncated {
		t.Fatalf("the first item must have lost its output: %+v", out)
	}
	if len(again.Reports[1].Detections) != DefaultOutboxMaxDetection-1 || again.Reports[1].HostInfo != nil {
		t.Fatalf("the second item must have lost a detection and its host information: %d, %v",
			len(again.Reports[1].Detections), again.Reports[1].HostInfo)
	}
}

// TestReportBatchSizeBound: a batch holds the oldest items whose payloads
// together fit in MaxReportBatchBytes; the items beyond wait for the next
// batch. An item larger than the bound goes alone.
func TestReportBatchSizeBound(t *testing.T) {
	tr := &fakeTransport{}
	rt := newCommandRuntime(t, tr)
	rt.out.limit = 1 << 30
	// queueBig queues a run report whose payload is about n bytes (the
	// output is base64 in the payload).
	queueBig := func(uid string, n int) {
		rt.queueCommandReport(&ReportRequest{CommandRuns: []CommandRunReport{{RunUID: uid, Purpose: PurposeRunOnly,
			Results: []CommandResultReport{{Output: make([]byte, n/4*3)}}}}})
	}
	payloadBytes := func() []int64 {
		sizes, err := models.ListOutboxSizes(rt.DB.Read())
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, s := range sizes {
			out = append(out, s.Bytes)
		}
		return out
	}
	runsOf := func(b *ReportBatch) []string {
		var uids []string
		for _, r := range b.Reports {
			for _, run := range r.CommandRuns {
				uids = append(uids, run.RunUID)
			}
		}
		return uids
	}
	queueBig("a", 9<<20)
	queueBig("b", 9<<20)
	queueBig("c", 9<<20)
	sizes := payloadBytes()
	if sizes[0]+sizes[1] > MaxReportBatchBytes || sizes[0]+sizes[1]+sizes[2] <= MaxReportBatchBytes {
		t.Fatalf("payload sizes %v do not test the bound", sizes)
	}
	rt.flushOutbox()
	rt.flushOutbox()
	if len(tr.batches) != 2 || !slices.Equal(runsOf(tr.batches[0]), []string{"a", "b"}) || !slices.Equal(runsOf(tr.batches[1]), []string{"c"}) {
		t.Fatalf("batches: %d", len(tr.batches))
	}

	queueBig("huge", 25<<20)
	queueBig("small", 100)
	rt.flushOutbox()
	rt.flushOutbox()
	if len(tr.batches) != 4 || !slices.Equal(runsOf(tr.batches[2]), []string{"huge"}) || !slices.Equal(runsOf(tr.batches[3]), []string{"small"}) {
		t.Fatalf("batches: %d", len(tr.batches))
	}
	if n := models.CountOutbox(rt.DB.Read()); n != 0 {
		t.Fatalf("items left: %d", n)
	}
}

// TestReportRecoveredRunQueued: the report of a run found unfinished at start
// is queued and goes with the next batch.
func TestReportRecoveredRunQueued(t *testing.T) {
	tr := &fakeTransport{}
	rt := newCommandRuntime(t, tr)
	insertExecution(t, rt.DB, "u1", 7, 0)
	recoverAtStart(t, rt)
	rt.flushOutbox()
	if len(tr.batches) != 1 || len(tr.reports) != 1 || tr.reports[0].CommandRuns[0].RunUID != "u1" {
		t.Fatalf("sent: %+v", tr.reports)
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
	rt.sync()
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
	rt.sync()
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
	rt.link.syncInterval = 1
	rt.Run(stopWhenRunsFinished(t, rt.DB, 1))
	rt.sync()
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

// TestKeptDetectionsBounded: the detections kept for the next periodic
// report are bounded as the outbox is: beyond the limit the oldest normal
// ones go first, then the oldest alerts, and they are counted as dropped for
// the next report.
func TestKeptDetectionsBounded(t *testing.T) {
	rt := newCommandRuntime(t, &cmdTransport{})
	keep := func(category string, n int, first int) {
		ds := make([]DetectionReport, n)
		for i := range ds {
			ds[i] = DetectionReport{Type: TypeExec, Category: category, Target: strconv.Itoa(first + i)}
		}
		if err := rt.DB.Write(func(tx models.Writer) error { return keepDetections(tx, ds) }); err != nil {
			t.Fatal(err)
		}
	}
	keep(CategoryAlert, 10, 0)
	keep(CategoryNormal, DefaultOutboxMaxDetection-15, 100)
	keep(CategoryAlert, 20, 1_000_000)
	var kept []DetectionReport
	loadJSONState(rt.DB.Read(), statePendingDetections, &kept)
	if len(kept) != DefaultOutboxMaxDetection || kept[0].Target != "0" || kept[10].Target != "115" {
		t.Fatalf("kept %d, first %+v, eleventh %+v", len(kept), kept[0], kept[10])
	}
	if got := models.GetLocalState(rt.DB.Read(), stateDroppedPending); got != "15" {
		t.Fatalf("dropped count %q", got)
	}
	if _, err := rt.out.queuePeriodic(&ReportRequest{}); err != nil {
		t.Fatal(err)
	}
	if r := queuedReports(t, rt); len(r) != 1 || len(r[0].Detections) != DefaultOutboxMaxDetection || r[0].Dropped != 15 {
		t.Fatalf("the periodic report: %d items", len(r))
	}
	if v := models.GetLocalState(rt.DB.Read(), statePendingDetections); v != "" {
		t.Fatalf("still kept after the report took them: %d bytes", len(v))
	}
}
