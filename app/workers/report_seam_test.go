package workers

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// The seam between the agent and the central for the identity of reports
// and for running_requests, proved with a real agent runtime against the
// central's handlers (the JSON on the wire included).

// handlerTransport is the transport of a token agent that sends its requests
// through the central's agent handler. It records what the agent sends; it
// can lose the answer of a report the central stored (lose) and stand for an
// agent that is gone (down).
type handlerTransport struct {
	e       *testEnv
	tok     string
	mu      sync.Mutex
	down    bool
	lose    func(req *modules.ReportRequest) bool
	lost    map[string]bool
	reports []string // report ids in the order sent
	syncs   []string // the JSON of every sync request
}

var errAnswerLost = errors.New("the answer did not arrive")

func (h *handlerTransport) machine() string { return "m-" + h.tok[len(h.tok)-6:] }

func (h *handlerTransport) Sync(req *modules.SyncRequest) (*modules.SyncResponse, error) {
	h.mu.Lock()
	down := h.down
	h.mu.Unlock()
	if down {
		return nil, errAnswerLost
	}
	b, _ := json.Marshal(req)
	rec := h.e.agentCall("sync", h.tok, h.machine(), json.RawMessage(b))
	if rec.Code != 200 {
		return nil, errors.New(rec.Body.String())
	}
	var resp modules.SyncResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.syncs = append(h.syncs, string(b))
	h.mu.Unlock()
	return &resp, nil
}

func (h *handlerTransport) Report(req *modules.ReportRequest) error {
	h.mu.Lock()
	down := h.down
	h.mu.Unlock()
	if down {
		return errAnswerLost
	}
	b, _ := json.Marshal(req)
	rec := h.e.agentCall("report", h.tok, h.machine(), json.RawMessage(b))
	if rec.Code != 200 {
		return errors.New(rec.Body.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reports = append(h.reports, req.ReportID)
	if h.lose != nil && !h.lost[req.ReportID] && h.lose(req) {
		h.lost[req.ReportID] = true
		return errAnswerLost
	}
	return nil
}

// sentSyncs returns the sync requests sent so far.
func (h *handlerTransport) sentSyncs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.syncs)
}

// seamSet creates a set of the current OS user running command and returns
// its id.
func seamSet(t *testing.T, e *testEnv, a *models.Agent, command string) int64 {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	set := e.admin("POST", "/agents/"+itoa64(a.ID)+"/command-sets", map[string]any{"name": "seam", "purpose": "collect_output",
		"os_user": me.Username, "commands": []any{map[string]any{"command_line": command, "patterns": []string{`version=(?P<version>\S+)`}}}})
	return int64(set["id"].(float64))
}

// agentLocalDB opens a new local database of an agent.
func agentLocalDB(t *testing.T) *modules.DB {
	t.Helper()
	db, err := modules.OpenSQLite(filepath.Join(t.TempDir(), "agent.db"), modules.SchemaAgentLocal)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A report the central stored but whose answer the agent did not get is sent
// again by the agent with the same id and is not stored or counted again:
// the run is stored once, the request ends, the dropped count is added once.
func TestAgentReportSentAgainEndToEnd(t *testing.T) {
	e := newTestEnv(t)
	var logs strings.Builder
	var logMu sync.Mutex
	e.c.logger = log.New(lockedWriter{&logs, &logMu}, "", 0)
	tok, a, tdb := commandAgent(t, e, "host-seam")
	setID := seamSet(t, e, a, "echo version=7.1")
	reqID, _ := requestRun(t, e, a, setID)

	db := agentLocalDB(t)
	// The agent's own count of dropped detections goes into its next
	// periodic report (the outbox's state key).
	if err := db.Write(func(tx models.Writer) error { return models.SetLocalState(tx, "dropped_pending", "2") }); err != nil {
		t.Fatal(err)
	}
	// The answer of every report that carries something counted is lost
	// the first time.
	tr := &handlerTransport{e: e, tok: tok, lost: map[string]bool{}, lose: func(r *modules.ReportRequest) bool {
		return r.Dropped > 0 || len(r.CommandRuns) > 0 || len(r.CommandRequestAcks) > 0
	}}
	rt := modules.NewAgentRuntime(db, tr, log.New(io.Discard, "", 0))
	stop, quit := make(chan struct{}), make(chan struct{})
	defer close(quit)
	go func() {
		for {
			var finished int
			err := db.Read().QueryRow(`SELECT count(*) FROM command_executions WHERE finished_at > 0`).Scan(&finished)
			if err == nil && finished > 0 {
				close(stop)
				return
			}
			select {
			case <-quit:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	rt.Run(stop)
	for range 20 {
		if items, err := models.ListOutbox(db.Read()); err == nil && len(items) == 0 {
			break
		}
		rt.Cycle()
	}
	if items, _ := models.ListOutbox(db.Read()); len(items) != 0 {
		t.Fatalf("reports left in the outbox: %d", len(items))
	}

	tr.mu.Lock()
	sent, lost := slices.Clone(tr.reports), len(tr.lost)
	tr.mu.Unlock()
	if lost < 2 {
		t.Fatalf("lost answers: %d (sent %v)", lost, sent)
	}
	times := map[string]int{}
	for _, id := range sent {
		if !validReportID(id) {
			t.Fatalf("report id %q", id)
		}
		times[id]++
	}
	for id := range tr.lost {
		if times[id] != 2 {
			t.Fatalf("report %s whose answer was lost was sent %d times", id, times[id])
		}
		if !strings.Contains(func() string { logMu.Lock(); defer logMu.Unlock(); return logs.String() }(),
			"report "+id+" of host-seam was already stored; answered without storing it again") {
			t.Fatalf("no repeat line for %s", id)
		}
	}
	for id, n := range times {
		if !tr.lost[id] && n != 1 {
			t.Fatalf("report %s sent %d times", id, n)
		}
	}

	if st := reqState(t, e, a.ID, reqID); st != models.RequestDone {
		t.Fatalf("request: %s", st)
	}
	var runs, results int
	_ = tdb.Read().QueryRow(`SELECT count(*) FROM command_runs`).Scan(&runs)
	_ = tdb.Read().QueryRow(`SELECT count(*) FROM command_results`).Scan(&results)
	if runs != 1 || results != 1 {
		t.Fatalf("runs %d results %d", runs, results)
	}
	cur, _ := models.GetAgent(e.c.db.Read(), a.ID)
	if cur.DroppedTotal != 2 || droppedHistory(t, e) != 1 {
		t.Fatalf("dropped_total %d, history rows %d", cur.DroppedTotal, droppedHistory(t, e))
	}
	// Every sync carried the list of running requests (a list, never
	// null).
	for _, s := range tr.sentSyncs() {
		if !strings.Contains(s, `"running_requests":[`) {
			t.Fatalf("sync without running_requests: %s", s)
		}
	}
}

// A run that the agent lost (its data directory created again while the run
// was running at the central) is ended on the central by the running_requests
// of the agent's next sync; while the agent still listed it, it stayed
// running.
func TestAgentLostRunEndedBySync(t *testing.T) {
	e := newTestEnv(t)
	// The agent syncs every second, also while the command runs.
	setConfig(e, func(c *modules.CentralConfig) { c.DefaultCheckIntervalSec = 1 })
	tok, a, _ := commandAgent(t, e, "host-lostrun")
	setID := seamSet(t, e, a, "sleep 30")
	reqID, _ := requestRun(t, e, a, setID)

	tr1 := &handlerTransport{e: e, tok: tok}
	rt1 := modules.NewAgentRuntime(agentLocalDB(t), tr1, log.New(io.Discard, "", 0))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt1.Run(stop)
	}()
	stopped := false
	stopAgent := func() {
		if !stopped {
			stopped = true
			close(stop)
			<-done
		}
	}
	defer stopAgent()

	waitFor(t, "the request to run", func() bool { return reqState(t, e, a.ID, reqID) == models.RequestRunning })
	listed := `"running_requests":[` + itoa64(reqID) + `]`
	n := len(tr1.sentSyncs())
	waitFor(t, "a sync listing the request", func() bool {
		syncs := tr1.sentSyncs()
		return len(syncs) > n && strings.Contains(syncs[len(syncs)-1], listed)
	})
	if st := reqState(t, e, a.ID, reqID); st != models.RequestRunning {
		t.Fatalf("request listed by the agent: %s", st)
	}

	// The agent is gone with its data directory; it is started again on a
	// new one and syncs.
	tr1.mu.Lock()
	tr1.down = true
	tr1.mu.Unlock()
	tr2 := &handlerTransport{e: e, tok: tok}
	rt2 := modules.NewAgentRuntime(agentLocalDB(t), tr2, log.New(io.Discard, "", 0))
	rt2.Cycle()
	syncs := tr2.sentSyncs()
	if len(syncs) == 0 || !strings.Contains(syncs[0], `"running_requests":[]`) {
		t.Fatalf("sync of the new agent: %v", syncs)
	}
	r, err := models.GetCommandRequest(e.c.db.Read(), a.ID, reqID)
	if err != nil || r.State != models.RequestDone || r.DoneAt == 0 {
		t.Fatalf("lost run's request: %+v %v", r, err)
	}
	stopAgent()
	if st := reqState(t, e, a.ID, reqID); st != models.RequestDone {
		t.Fatalf("request after the old agent stopped: %s", st)
	}
}
