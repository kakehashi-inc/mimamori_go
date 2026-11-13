package workers

import (
	"io"
	"log"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// recordingTransport hands the built-in agent's requests to the central as
// inProcessTransport does and records each request it handed over.
type recordingTransport struct {
	inner   *inProcessTransport
	syncs   []modules.SyncRequest
	reports []modules.ReportRequest
}

func (r *recordingTransport) Sync(req *modules.SyncRequest) (*modules.SyncResponse, error) {
	r.syncs = append(r.syncs, *req)
	return r.inner.Sync(req)
}

func (r *recordingTransport) Report(req *modules.ReportRequest) error {
	r.reports = append(r.reports, *req)
	return r.inner.Report(req)
}

// TestBuiltinAgentSendsIdentityAndRunning: the built-in agent's direct calls
// carry what the agent sends over HTTP: the sync lists the manual runs that
// are started and not finished, every report has its own identity, and the
// central takes both.
func TestBuiltinAgentSendsIdentityAndRunning(t *testing.T) {
	e := newTestEnv(t)
	b, _ := models.GetBuiltinAgent(e.c.db.Read())
	db, err := modules.OpenSQLite(filepath.Join(t.TempDir(), modules.BuiltinAgentLocalDBName), modules.SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A manual run in progress and a finished one.
	now := time.Now().Unix()
	err = db.Write(func(tx models.Writer) error {
		if err := models.InsertCommandExecution(tx, &models.CommandExecution{RunUID: "running", SetID: 1, SetRevision: 1,
			RequestID: 77, Trigger: modules.TriggerManual, StartedAt: now}); err != nil {
			return err
		}
		if err := models.InsertCommandExecution(tx, &models.CommandExecution{RunUID: "done", SetID: 1, SetRevision: 1,
			RequestID: 78, Trigger: modules.TriggerManual, StartedAt: now}); err != nil {
			return err
		}
		return models.FinishCommandExecution(tx, "done", now)
	})
	if err != nil {
		t.Fatal(err)
	}
	tr := &recordingTransport{inner: &inProcessTransport{c: e.c, agentID: b.ID}}
	rt := modules.NewAgentRuntime(db, tr, log.New(io.Discard, "", 0))
	rt.Cycle()
	rt.Cycle()
	if len(tr.syncs) != 2 || !slices.Equal(tr.syncs[0].RunningRequests, []int64{77}) {
		t.Fatalf("syncs: %+v", tr.syncs)
	}
	form := regexp.MustCompile(`^[0-9a-f]{32}$`)
	var ids []string
	for _, r := range tr.reports {
		if !form.MatchString(r.ReportID) || slices.Contains(ids, r.ReportID) {
			t.Fatalf("report ids %v, then %q", ids, r.ReportID)
		}
		ids = append(ids, r.ReportID)
	}
	if len(ids) < 2 {
		t.Fatalf("reports: %d", len(ids))
	}
	// The central answered every report: nothing is left queued.
	if n := models.CountOutbox(db.Read()); n != 0 {
		t.Fatalf("queued after the direct calls: %d", n)
	}
}
