package workers

import (
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// The run requests of the agents: a request waits queued until a sync of its
// agent that says free_worker hands it over (one per sync); requests of
// other agents play no role.

// queueAgent is a token agent of a queue test with its sets.
type queueAgent struct {
	tok  string
	a    *models.Agent
	sets []int64
}

// newQueueAgent issues a token agent with n sets.
func newQueueAgent(t *testing.T, e *testEnv, name string, n int) *queueAgent {
	t.Helper()
	tok, a, _ := commandAgent(t, e, name)
	return &queueAgent{tok: tok, a: a, sets: addQueueSets(t, e, a, n)}
}

// addQueueSets creates n sets on an agent.
func addQueueSets(t *testing.T, e *testEnv, a *models.Agent, n int) []int64 {
	t.Helper()
	var ids []int64
	for i := range n {
		body := commandSetBody(fmt.Sprintf("s%d", i), "run_only")
		body["os_user"] = "svc"
		ids = append(ids, int64(e.admin("POST", "/agents/"+itoa64(a.ID)+"/command-sets", body)["id"].(float64)))
	}
	return ids
}

// requestRun requests a run of a set and returns the request's id and the
// state it was answered with.
func requestRun(t *testing.T, e *testEnv, a *models.Agent, setID int64) (int64, string) {
	t.Helper()
	q := e.admin("POST", "/agents/"+itoa64(a.ID)+"/command-sets/"+itoa64(setID)+"/run", nil)
	return int64(q["id"].(float64)), q["state"].(string)
}

// reqState returns the stored state of a request.
func reqState(t *testing.T, e *testEnv, agentID, id int64) string {
	t.Helper()
	r, err := models.GetCommandRequest(e.c.db.Read(), agentID, id)
	if err != nil {
		t.Fatalf("request %d: %v", id, err)
	}
	return r.State
}

// wantStates checks the states of requests (agent, id, state triples).
func wantStates(t *testing.T, e *testEnv, what string, want ...any) {
	t.Helper()
	for i := 0; i+2 < len(want); i += 3 {
		agentID, id, st := want[i].(int64), want[i+1].(int64), want[i+2].(string)
		if got := reqState(t, e, agentID, id); got != st {
			t.Fatalf("%s: request %d is %s, want %s", what, id, got, st)
		}
	}
}

// syncWith sends a sync request of a token agent through the handler.
func syncWith(t *testing.T, e *testEnv, tok string, req modules.SyncRequest) modules.SyncResponse {
	t.Helper()
	if req.AgentVersion == "" {
		req.AgentVersion = "test"
	}
	rec := e.agentCall("sync", tok, "m-"+tok[len(tok)-6:], req)
	if rec.Code != 200 {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}
	return decode[modules.SyncResponse](t, rec)
}

// syncFree sends a sync of a token agent that runs nothing and can take a
// run request (free_worker).
func syncFree(t *testing.T, e *testEnv, tok string) modules.SyncResponse {
	t.Helper()
	return syncWith(t, e, tok, modules.SyncRequest{RunningRequests: []int64{}, FreeWorker: true})
}

// handedIDs returns the ids of the requests of a sync answer.
func handedIDs(resp modules.SyncResponse) []int64 {
	var ids []int64
	for _, r := range resp.CommandRequests {
		ids = append(ids, r.ID)
	}
	return ids
}

// finishRun reports the end of the run of a request.
func finishRun(t *testing.T, e *testEnv, tok string, setID, id int64) {
	t.Helper()
	reportAgent(t, e, tok, modules.ReportRequest{CommandRuns: []modules.CommandRunReport{
		runReport("run-"+itoa64(id), setID, "s", id, []modules.CommandResultReport{}, true, modules.ResultSuccess)}})
}

// setLastSync moves an agent's last sync to the given time.
func setLastSync(t *testing.T, e *testEnv, agentID, at int64) {
	t.Helper()
	if err := e.exec(`UPDATE agents SET last_sync_at = ? WHERE id = ?`, at, agentID); err != nil {
		t.Fatal(err)
	}
}

// lockedWriter serialises writes to a buffer shared with the test.
type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// One request per sync and only with free_worker: a request is created
// queued; a sync without free_worker hands nothing over; each sync with
// free_worker hands over the oldest queued request of the agent whose expiry
// has not passed; every answer carries all delivered requests of the agent.
func TestRequestOnePerSyncWithFreeWorker(t *testing.T) {
	e := newTestEnv(t)
	A := newQueueAgent(t, e, "one-a", 3)
	syncFree(t, e, A.tok)
	var ids []int64
	for _, s := range A.sets {
		id, st := requestRun(t, e, A.a, s)
		if st != models.RequestQueued {
			t.Fatalf("new request answered %s", st)
		}
		ids = append(ids, id)
	}
	for _, req := range []modules.SyncRequest{{RunningRequests: []int64{}}, {}} {
		if got := handedIDs(syncWith(t, e, A.tok, req)); len(got) != 0 {
			t.Fatalf("handed over without free_worker: %v", got)
		}
	}
	wantStates(t, e, "without free_worker", A.a.ID, ids[0], models.RequestQueued, A.a.ID, ids[1], models.RequestQueued, A.a.ID, ids[2], models.RequestQueued)
	for i := range ids {
		if got := handedIDs(syncFree(t, e, A.tok)); !slices.Equal(got, ids[:i+1]) {
			t.Fatalf("sync %d with free_worker answered %v, want %v", i+1, got, ids[:i+1])
		}
		for j, id := range ids {
			want := models.RequestQueued
			if j <= i {
				want = models.RequestDelivered
			}
			wantStates(t, e, fmt.Sprintf("after sync %d", i+1), A.a.ID, id, want)
		}
	}
	// The delivered requests come again with every sync until the agent
	// answers them, whether it says free_worker or not.
	if got := handedIDs(syncWith(t, e, A.tok, modules.SyncRequest{RunningRequests: []int64{}})); !slices.Equal(got, ids) {
		t.Fatalf("answer without free_worker: %v", got)
	}
	if got := handedIDs(syncFree(t, e, A.tok)); !slices.Equal(got, ids) {
		t.Fatalf("answer with free_worker and nothing queued: %v", got)
	}
	for i, id := range ids {
		finishRun(t, e, A.tok, A.sets[i], id)
	}

	// A queued request whose expiry passed is not handed over (the
	// periodic step ends it); the next one is.
	late, _ := requestRun(t, e, A.a, A.sets[0])
	next, _ := requestRun(t, e, A.a, A.sets[1])
	if err := e.exec(`UPDATE command_requests SET expires_at = ? WHERE id = ?`, time.Now().Unix()-1, late); err != nil {
		t.Fatal(err)
	}
	if got := handedIDs(syncFree(t, e, A.tok)); !slices.Equal(got, []int64{next}) {
		t.Fatalf("answer with a queued request past its expiry: %v", got)
	}
	wantStates(t, e, "past its expiry", A.a.ID, late, models.RequestQueued)
	r, _ := models.GetCommandRequest(e.c.db.Read(), A.a.ID, next)
	if r.DeliveredAt == 0 {
		t.Fatalf("delivered_at of a handed request: %+v", r)
	}
}

// Requests of other agents play no role: an agent's request is handed over at
// its first sync with free_worker, whatever other agents have queued or
// delivered and whether they sync at all (one keeps five requests delivered
// without answering them, one went silent, one never synced). The built-in
// agent is handed its requests the same way. Nothing of the other agents
// changes.
func TestRequestsOfOtherAgentsDoNotMatter(t *testing.T) {
	e := newTestEnv(t)
	busy := newQueueAgent(t, e, "busy", 5)
	silent := newQueueAgent(t, e, "silent", 2)
	never := newQueueAgent(t, e, "never", 2)
	B := newQueueAgent(t, e, "b", 1)
	for _, q := range []*queueAgent{busy, silent, B} {
		syncFree(t, e, q.tok)
	}
	var held []int64
	for _, s := range busy.sets {
		id, _ := requestRun(t, e, busy.a, s)
		held = append(held, id)
		syncFree(t, e, busy.tok)
	}
	var waiting []int64
	for _, q := range []*queueAgent{silent, never} {
		for _, s := range q.sets {
			id, _ := requestRun(t, e, q.a, s)
			waiting = append(waiting, id)
		}
	}
	setLastSync(t, e, silent.a.ID, time.Now().Unix()-100000)
	b1, _ := requestRun(t, e, B.a, B.sets[0])
	if got := handedIDs(syncFree(t, e, B.tok)); !slices.Equal(got, []int64{b1}) {
		t.Fatalf("B's first sync with free_worker: %v", got)
	}

	b, err := models.GetBuiltinAgent(e.c.db.Read())
	if err != nil {
		t.Fatal(err)
	}
	bsets := addQueueSets(t, e, b, 1)
	bt := &inProcessTransport{c: e.c, agentID: b.ID}
	r1, _ := requestRun(t, e, b, bsets[0])
	resp, err := bt.Sync(&modules.SyncRequest{AgentVersion: "test", RunningRequests: []int64{}})
	if err != nil || len(resp.CommandRequests) != 0 {
		t.Fatalf("built-in sync without free_worker: %v %+v", err, resp.CommandRequests)
	}
	resp, err = bt.Sync(&modules.SyncRequest{AgentVersion: "test", RunningRequests: []int64{}, FreeWorker: true})
	if err != nil || len(resp.CommandRequests) != 1 || resp.CommandRequests[0].ID != r1 {
		t.Fatalf("built-in sync with free_worker: %v %+v", err, resp.CommandRequests)
	}

	for _, id := range held {
		wantStates(t, e, "busy agent", busy.a.ID, id, models.RequestDelivered)
	}
	for i, id := range waiting {
		q := silent
		if i >= 2 {
			q = never
		}
		wantStates(t, e, "agents that do not sync", q.a.ID, id, models.RequestQueued)
	}
}

// An agent that never synced is not responding: its request waits queued;
// its first sync with free_worker makes it responding and hands the request
// over.
func TestQueueNeverSyncedAgent(t *testing.T) {
	e := newTestEnv(t)
	A := newQueueAgent(t, e, "never-a", 1)
	a1, st := requestRun(t, e, A.a, A.sets[0])
	if st != models.RequestQueued {
		t.Fatalf("request of an agent that never synced: %s", st)
	}
	if list := e.admin("GET", "/agents/"+itoa64(A.a.ID)+"/command-sets", nil); list["agent_responding"] != false {
		t.Fatalf("agent_responding: %v", list["agent_responding"])
	}
	if got := handedIDs(syncFree(t, e, A.tok)); len(got) != 1 || got[0] != a1 {
		t.Fatalf("first sync's answer: %v", got)
	}
	wantStates(t, e, "after the first sync", A.a.ID, a1, models.RequestDelivered)
	if list := e.admin("GET", "/agents/"+itoa64(A.a.ID)+"/command-sets", nil); list["agent_responding"] != true {
		t.Fatalf("agent_responding after the sync: %v", list["agent_responding"])
	}
}

// The judgement "responding" at its edges, with the default interval and
// with the interval of the rule version the agent reports.
func TestQueueInterval(t *testing.T) {
	e := newTestEnv(t)
	now := int64(1_000_000)
	if err := e.exec(`INSERT INTO rules (id, name, created_at, updated_at) VALUES (900, 'slow', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(`INSERT INTO rule_versions (rule_id, version, content, created_at) VALUES (900, 1, '{"check_interval_sec": 3600}', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		rule          int64
		version       int
		last          int64
		def, interval int
		want          bool
	}{
		{"never synced", 0, 0, 0, 60, 60, false},
		{"default, at the edge", 0, 0, now - 780, 60, 60, true},
		{"default, past the edge", 0, 0, now - 781, 60, 60, false},
		{"configured default", 0, 0, now - (3*120 + 600), 120, 120, true},
		{"rule 3600, at the edge", 900, 1, now - (3*3600 + 600), 60, 3600, true},
		{"rule 3600, past the edge", 900, 1, now - (3*3600 + 601), 60, 3600, false},
		{"unreadable rule version", 900, 7, now - 781, 60, 60, false},
	} {
		a := &models.Agent{LastSyncAt: tc.last, ReportedRuleID: tc.rule, ReportedRuleVersion: tc.version}
		if got := agentInterval(e.c.db.Read(), a, tc.def); got != tc.interval {
			t.Errorf("%s: interval %d, want %d", tc.name, got, tc.interval)
		}
		if got := agentResponding(e.c.db.Read(), a, tc.def, now); got != tc.want {
			t.Errorf("%s: responding %v, want %v", tc.name, got, tc.want)
		}
	}
}

// running_requests: a running request the agent no longer lists ends; a
// listed one, a missing list and a delivered request are not touched.
func TestQueueRunningEndsBySync(t *testing.T) {
	e := newTestEnv(t)
	A := newQueueAgent(t, e, "held", 2)
	syncFree(t, e, A.tok)
	r1, _ := requestRun(t, e, A.a, A.sets[0])
	r2, _ := requestRun(t, e, A.a, A.sets[1])
	syncFree(t, e, A.tok)
	syncFree(t, e, A.tok)
	reportAgent(t, e, A.tok, modules.ReportRequest{CommandRequestAcks: []modules.CommandRequestAck{
		{RequestID: r1, Status: modules.RequestAckStarted, RunUID: "run-1", At: 1}}})
	wantStates(t, e, "start", A.a.ID, r1, models.RequestRunning, A.a.ID, r2, models.RequestDelivered)
	syncWith(t, e, A.tok, modules.SyncRequest{RunningRequests: []int64{r1}})
	wantStates(t, e, "listed", A.a.ID, r1, models.RequestRunning, A.a.ID, r2, models.RequestDelivered)
	syncWith(t, e, A.tok, modules.SyncRequest{})
	wantStates(t, e, "no list", A.a.ID, r1, models.RequestRunning, A.a.ID, r2, models.RequestDelivered)
	// A missing key and null both decode to no list.
	if rec := e.agentCall("sync", A.tok, "m-"+A.tok[len(A.tok)-6:], map[string]any{"agent_version": "t", "running_requests": nil}); rec.Code != 200 {
		t.Fatalf("sync with null: %d", rec.Code)
	}
	wantStates(t, e, "null", A.a.ID, r1, models.RequestRunning)
	// A list ends only what became running before the sync's handling
	// began (whole seconds): in the same second it ends nothing.
	if err := e.exec(`UPDATE command_requests SET running_at = ? WHERE id = ?`, time.Now().Unix()+5, r1); err != nil {
		t.Fatal(err)
	}
	syncWith(t, e, A.tok, modules.SyncRequest{RunningRequests: []int64{}})
	wantStates(t, e, "empty list older than the run", A.a.ID, r1, models.RequestRunning)
	if err := e.exec(`UPDATE command_requests SET running_at = ? WHERE id = ?`, time.Now().Unix()-1, r1); err != nil {
		t.Fatal(err)
	}
	syncWith(t, e, A.tok, modules.SyncRequest{RunningRequests: []int64{}})
	wantStates(t, e, "empty list", A.a.ID, r1, models.RequestDone, A.a.ID, r2, models.RequestDelivered)
	if r, _ := models.GetCommandRequest(e.c.db.Read(), A.a.ID, r1); r.DoneAt == 0 || r.RunUID != "run-1" {
		t.Fatalf("ended by the list: %+v", r)
	}
	// The closing report afterwards changes nothing.
	finishRun(t, e, A.tok, A.sets[0], r1)
	if r, _ := models.GetCommandRequest(e.c.db.Read(), A.a.ID, r1); r.State != models.RequestDone || r.RunUID != "run-1" {
		t.Fatalf("after the closing report: %+v", r)
	}
}

// A sync whose running_requests the agent read before the run started (the
// agent gave up waiting for its answer and sent the report that made the
// request running; the central carried the sync out after that report) does
// not end the running request. A later sync ends it.
func TestQueueStaleSyncKeepsRunning(t *testing.T) {
	e := newTestEnv(t)
	A := newQueueAgent(t, e, "stale-a", 1)
	syncFree(t, e, A.tok)
	ra, _ := requestRun(t, e, A.a, A.sets[0])
	entered := time.Now().Unix() - 1 // the stale sync's handling began
	if got := handedIDs(syncFree(t, e, A.tok)); len(got) != 1 || got[0] != ra {
		t.Fatalf("handed over: %v", got)
	}
	reportAgent(t, e, A.tok, modules.ReportRequest{CommandRequestAcks: []modules.CommandRequestAck{
		{RequestID: ra, Status: modules.RequestAckStarted, RunUID: "r-a", At: 1}}})
	wantStates(t, e, "started", A.a.ID, ra, models.RequestRunning)
	if r, _ := models.GetCommandRequest(e.c.db.Read(), A.a.ID, ra); r.RunningAt <= entered {
		t.Fatalf("running_at %d, sync began %d", r.RunningAt, entered)
	}
	if _, err := e.c.centralSync(A.a, nil, &modules.SyncRequest{AgentVersion: "test", RunningRequests: []int64{}}, entered); err != nil {
		t.Fatal(err)
	}
	wantStates(t, e, "after the stale sync", A.a.ID, ra, models.RequestRunning)
	if _, err := e.c.centralSync(A.a, nil, &modules.SyncRequest{AgentVersion: "test", RunningRequests: []int64{}}, time.Now().Unix()+1); err != nil {
		t.Fatal(err)
	}
	wantStates(t, e, "after a later sync", A.a.ID, ra, models.RequestDone)
}

// What a report does to run requests.
func TestQueueReportTransitions(t *testing.T) {
	e := newTestEnv(t)
	A := newQueueAgent(t, e, "rep-a", 4)
	B := newQueueAgent(t, e, "rep-b", 1)
	syncFree(t, e, A.tok)
	syncFree(t, e, B.tok)
	var ids []int64
	for _, s := range A.sets {
		id, _ := requestRun(t, e, A.a, s)
		ids = append(ids, id)
	}
	for range ids {
		syncFree(t, e, A.tok)
	}

	// Ack started: running with the run's uid; a run report that is not
	// finished: running too; the run's end: done.
	started := modules.ReportRequest{CommandRequestAcks: []modules.CommandRequestAck{{RequestID: ids[0], Status: modules.RequestAckStarted, RunUID: "u0", At: 1}}}
	reportAgent(t, e, A.tok, started)
	runStart := modules.ReportRequest{CommandRuns: []modules.CommandRunReport{runReport("u1", A.sets[1], "s", ids[1], []modules.CommandResultReport{}, false, "")}}
	reportAgent(t, e, A.tok, runStart)
	for i, uid := range []string{"u0", "u1"} {
		if r, _ := models.GetCommandRequest(e.c.db.Read(), A.a.ID, ids[i]); r.State != models.RequestRunning || r.RunUID != uid {
			t.Fatalf("request %d: %+v", i, r)
		}
	}
	runEnd := modules.ReportRequest{CommandRuns: []modules.CommandRunReport{runReport("u1", A.sets[1], "s", ids[1], []modules.CommandResultReport{}, true, modules.ResultSuccess)}}
	reportAgent(t, e, A.tok, runEnd)
	wantStates(t, e, "run end", A.a.ID, ids[1], models.RequestDone, A.a.ID, ids[0], models.RequestRunning)

	// Ack expired and unknown_set: expired, with one history row each.
	acks := modules.ReportRequest{CommandRequestAcks: []modules.CommandRequestAck{
		{RequestID: ids[2], Status: modules.RequestAckExpired, At: 1}, {RequestID: ids[3], Status: modules.RequestAckUnknownSet, At: 1}}}
	reportAgent(t, e, A.tok, acks)
	wantStates(t, e, "acks", A.a.ID, ids[2], models.RequestExpired, A.a.ID, ids[3], models.RequestExpired)

	// Repeated application changes nothing (and writes no second history
	// row); another agent's report naming A's requests changes nothing; a
	// scheduled run (request id 0) changes no request.
	for _, rep := range []modules.ReportRequest{started, runStart, runEnd, acks} {
		reportAgent(t, e, A.tok, rep)
	}
	wantStates(t, e, "repeated", A.a.ID, ids[0], models.RequestRunning, A.a.ID, ids[1], models.RequestDone,
		A.a.ID, ids[2], models.RequestExpired, A.a.ID, ids[3], models.RequestExpired)
	reportAgent(t, e, B.tok, modules.ReportRequest{CommandRuns: []modules.CommandRunReport{
		runReport("x", B.sets[0], "s", ids[0], []modules.CommandResultReport{}, true, modules.ResultSuccess)}})
	reportAgent(t, e, A.tok, modules.ReportRequest{CommandRuns: []modules.CommandRunReport{
		runReport("sched", A.sets[0], "s", 0, []modules.CommandResultReport{}, true, modules.ResultSuccess)}})
	wantStates(t, e, "other agent and scheduled run", A.a.ID, ids[0], models.RequestRunning)
	items, _, _ := models.ListHistory(e.c.db.Read(), models.HistCommand, "", nil, 100, 0)
	n := 0
	for _, h := range items {
		if h.Action == "request_expired" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("history rows request_expired: %d", n)
	}

	// The transitions of a report in order: acks first, then runs.
	got := requestTransitions(&modules.ReportRequest{
		CommandRuns: []modules.CommandRunReport{runReport("r", 1, "s", 5, nil, true, "success"), runReport("s", 1, "s", 0, nil, true, "success")},
		CommandRequestAcks: []modules.CommandRequestAck{{RequestID: 5, Status: modules.RequestAckStarted, RunUID: "r"},
			{RequestID: 6, Status: modules.RequestAckUnknownSet}, {RequestID: 0, Status: modules.RequestAckStarted}}})
	want := []reqTransition{{requestID: 5, to: models.RequestRunning, runUID: "r"}, {requestID: 6, to: models.RequestExpired, why: modules.RequestAckUnknownSet},
		{requestID: 5, to: models.RequestDone, runUID: "r"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("transitions %v, want %v", got, want)
	}
}

// Expiry of queued requests, discard and set deletion: a queued request can
// be discarded (also with its set) and expires after its expiry; a delivered
// one of a responding agent is neither discarded nor expired.
func TestQueueExpiryAndDiscard(t *testing.T) {
	e := newTestEnv(t)
	A := newQueueAgent(t, e, "exp", 5)
	syncFree(t, e, A.tok)
	path := "/agents/" + itoa64(A.a.ID)
	d1, _ := requestRun(t, e, A.a, A.sets[0])
	syncFree(t, e, A.tok) // d1 delivered
	q1, _ := requestRun(t, e, A.a, A.sets[1])
	q2, _ := requestRun(t, e, A.a, A.sets[2])
	q3, _ := requestRun(t, e, A.a, A.sets[3])
	wantStates(t, e, "start", A.a.ID, d1, models.RequestDelivered, A.a.ID, q1, models.RequestQueued,
		A.a.ID, q2, models.RequestQueued, A.a.ID, q3, models.RequestQueued)

	// Discard of a delivered request: 409. Discard of a queued one.
	if rec := e.cli("DELETE", path+"/command-requests/"+itoa64(d1), nil); rec.Code != 409 {
		t.Fatalf("discard of delivered: %d", rec.Code)
	}
	if out := e.admin("DELETE", path+"/command-requests/"+itoa64(q1), nil); out["state"] != models.RequestDiscarded {
		t.Fatalf("discard answered: %v", out)
	}
	wantStates(t, e, "queued discarded", A.a.ID, q1, models.RequestDiscarded, A.a.ID, q2, models.RequestQueued)

	// Set deletion discards its queued request.
	e.admin("DELETE", path+"/command-sets/"+itoa64(A.sets[2]), nil)
	wantStates(t, e, "set deleted", A.a.ID, q2, models.RequestDiscarded, A.a.ID, q3, models.RequestQueued)

	// Expiry: a queued request past its expiry ends, the delivered one of
	// the responding agent does not.
	q4, _ := requestRun(t, e, A.a, A.sets[4])
	past := time.Now().Unix() - 1
	if err := e.exec(`UPDATE command_requests SET expires_at = ? WHERE id IN (?, ?, ?)`, past, d1, q3, q4); err != nil {
		t.Fatal(err)
	}
	e.c.runRequestsStep(time.Now())
	wantStates(t, e, "expired", A.a.ID, d1, models.RequestDelivered, A.a.ID, q3, models.RequestExpired, A.a.ID, q4, models.RequestExpired)
	items, _, _ := models.ListHistory(e.c.db.Read(), models.HistCommand, "", nil, 100, 0)
	n := 0
	for _, h := range items {
		if h.Action == "request_expired" && strings.Contains(h.I18n, "history.detail.request_not_delivered") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("history rows of expiry: %d", n)
	}
}

// unansweredOfSet counts the queued and delivered requests of a set of an
// agent.
func unansweredOfSet(t *testing.T, e *testEnv, agentID, setID int64) int {
	t.Helper()
	var n int
	if err := e.c.db.Read().QueryRow(`SELECT count(*) FROM command_requests WHERE agent_id = ? AND set_id = ? AND state IN ('queued', 'delivered')`,
		agentID, setID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A run request and the deletion of its set at the same moment: the set is
// deleted (with its discard of requests) after the run request read it and
// before it inserted the request. The run request finds the deletion when it
// reads the set again, discards its request (recorded in the history) and
// answers 404; at the same moment in any order, no request of a deleted set
// is left.
func TestQueueRunVersusSetDeletion(t *testing.T) {
	e := newTestEnv(t)
	A := newQueueAgent(t, e, "del", 2)
	syncFree(t, e, A.tok)
	path := "/agents/" + itoa64(A.a.ID) + "/command-sets/"
	deleted := false
	e.c.runSetLoaded = func() {
		if !deleted {
			deleted = true
			if rec := e.cli("DELETE", path+itoa64(A.sets[0]), nil); rec.Code != 200 {
				t.Errorf("deletion: %d %s", rec.Code, rec.Body.String())
			}
		}
	}
	if rec := e.cli("POST", path+itoa64(A.sets[0])+"/run", nil); rec.Code != 404 || !strings.Contains(rec.Body.String(), "command.set_not_found") {
		t.Fatalf("run of the set deleted meanwhile: %d %s", rec.Code, rec.Body.String())
	}
	e.c.runSetLoaded = nil
	if n := unansweredOfSet(t, e, A.a.ID, A.sets[0]); n != 0 {
		t.Fatalf("unanswered requests of the deleted set: %d", n)
	}
	var id int64
	var by string
	if err := e.c.db.Read().QueryRow(`SELECT id, discarded_by FROM command_requests WHERE agent_id = ? AND set_id = ? AND state = 'discarded'`, A.a.ID, A.sets[0]).Scan(&id, &by); err != nil {
		t.Fatal(err)
	}
	if by != "cli" {
		t.Fatalf("discarded by %q", by)
	}
	items, _, _ := models.ListHistory(e.c.db.Read(), models.HistCommand, "", nil, 100, 0)
	found := false
	for _, h := range items {
		if h.Action == "request_discarded" && strings.Contains(h.I18n, `"request":`+itoa64(id)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no history row of the discard of request %d", id)
	}
	// The other set's request is handed over at the next sync.
	r2, st := requestRun(t, e, A.a, A.sets[1])
	if st != models.RequestQueued {
		t.Fatalf("next request: %s", st)
	}
	if got := handedIDs(syncFree(t, e, A.tok)); !slices.Equal(got, []int64{r2}) {
		t.Fatalf("answer after the deletion: %v", got)
	}

	// The two requests sent at the same moment, many times.
	B := newQueueAgent(t, e, "del-b", 0)
	syncFree(t, e, B.tok)
	for i := range 100 {
		body := commandSetBody(fmt.Sprintf("d%d", i), "run_only")
		body["os_user"] = "svc"
		setID := int64(e.admin("POST", "/agents/"+itoa64(B.a.ID)+"/command-sets", body)["id"].(float64))
		set := "/agents/" + itoa64(B.a.ID) + "/command-sets/" + itoa64(setID)
		var wg sync.WaitGroup
		var runCode int
		wg.Go(func() { runCode = e.cli("POST", set+"/run", nil).Code })
		wg.Go(func() { e.cli("DELETE", set, nil) })
		wg.Wait()
		if runCode != 200 && runCode != 404 {
			t.Fatalf("round %d: run answered %d", i, runCode)
		}
		if n := unansweredOfSet(t, e, B.a.ID, setID); n != 0 {
			t.Fatalf("round %d: %d unanswered requests of the deleted set (run answered %d)", i, n, runCode)
		}
	}
}

// The central closed and opened again on the same data directory: every
// request keeps its state; the delivered one is sent again and the queued
// ones are handed over one per sync.
func TestQueueRestart(t *testing.T) {
	e := newTestEnv(t)
	A := newQueueAgent(t, e, "rs", 3)
	syncFree(t, e, A.tok)
	r1, _ := requestRun(t, e, A.a, A.sets[0])
	syncFree(t, e, A.tok)
	r2, _ := requestRun(t, e, A.a, A.sets[1])
	r3, _ := requestRun(t, e, A.a, A.sets[2])
	wantStates(t, e, "before", A.a.ID, r1, models.RequestDelivered, A.a.ID, r2, models.RequestQueued, A.a.ID, r3, models.RequestQueued)

	e = reopenCentral(t, e)
	wantStates(t, e, "after", A.a.ID, r1, models.RequestDelivered, A.a.ID, r2, models.RequestQueued, A.a.ID, r3, models.RequestQueued)
	if got := handedIDs(syncWith(t, e, A.tok, modules.SyncRequest{RunningRequests: []int64{}})); !slices.Equal(got, []int64{r1}) {
		t.Fatalf("answer after the restart without free_worker: %v", got)
	}
	if got := handedIDs(syncFree(t, e, A.tok)); !slices.Equal(got, []int64{r1, r2}) {
		t.Fatalf("answer after the restart: %v", got)
	}
	finishRun(t, e, A.tok, A.sets[0], r1)
	if got := handedIDs(syncFree(t, e, A.tok)); !slices.Equal(got, []int64{r2, r3}) {
		t.Fatalf("next answer: %v", got)
	}
}

// reopenCentral closes the central of e and opens a new one on the same data
// directory (the central's restart).
func reopenCentral(t *testing.T, e *testEnv) *testEnv {
	t.Helper()
	dir := e.c.dataDir
	e.c.closeTargets()
	if err := e.c.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := modules.OpenSQLite(filepath.Join(dir, modules.CentralDBFileName), modules.SchemaCentral)
	if err != nil {
		t.Fatal(err)
	}
	c := newCore(db, modules.DefaultCentralConfig(), dir, log.New(io.Discard, "", 0))
	t.Cleanup(func() { c.closeTargets(); db.Close() })
	if err := c.ensureServerKey(); err != nil {
		t.Fatal(err)
	}
	if err := c.writeCLISecret(); err != nil {
		t.Fatal(err)
	}
	return &testEnv{t: t, c: c, agent: c.agentHandler(), web: c.webHandler(), cliKey: c.cliSecret}
}

// Many agents (the built-in agent among them) sync and report at the same
// moment while requests of all of them wait: each sync hands over at most
// one new request, only when it says free_worker, only one of the syncing
// agent and the oldest it has queued; a request answered as started is not
// handed over again; every request runs once and ends done.
func TestRunRequestsManyAgentsAtOnce(t *testing.T) {
	e := newTestEnv(t)
	const agents, sets = 24, 3
	var ags []*queueAgent
	for i := range agents - 1 {
		q := newQueueAgent(t, e, fmt.Sprintf("qc%02d", i), sets)
		syncFree(t, e, q.tok)
		ags = append(ags, q)
	}
	b, err := models.GetBuiltinAgent(e.c.db.Read())
	if err != nil {
		t.Fatal(err)
	}
	ags = append(ags, &queueAgent{a: b, sets: addQueueSets(t, e, b, sets)})
	bt := &inProcessTransport{c: e.c, agentID: b.ID}
	if _, err := bt.Sync(&modules.SyncRequest{AgentVersion: "test", RunningRequests: []int64{}}); err != nil {
		t.Fatal(err)
	}
	// All requests at once, in an order mixing the agents.
	own := map[int64][]int64{}
	for s := range sets {
		for _, q := range ags {
			id, _ := requestRun(t, e, q.a, q.sets[s])
			own[q.a.ID] = append(own[q.a.ID], id)
		}
	}

	var mu sync.Mutex
	var problems []string
	runs := map[int64]int{}
	deadline := time.Now().Add(90 * time.Second)
	var wg sync.WaitGroup
	for i, q := range ags {
		mine := own[q.a.ID]
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(i), 7))
			fail := func(format string, args ...any) {
				mu.Lock()
				problems = append(problems, fmt.Sprintf("agent %d: ", q.a.ID)+fmt.Sprintf(format, args...))
				mu.Unlock()
			}
			doSync := func(req modules.SyncRequest) (*modules.SyncResponse, bool) {
				req.AgentVersion = "test"
				if q.tok == "" {
					r, err := bt.Sync(&req)
					if err != nil {
						fail("built-in sync: %v", err)
						return nil, false
					}
					return r, true
				}
				rec := e.agentCall("sync", q.tok, "m-"+q.tok[len(q.tok)-6:], req)
				if rec.Code != 200 {
					fail("sync: %d %s", rec.Code, rec.Body.String())
					return nil, false
				}
				r := decode[modules.SyncResponse](t, rec)
				return &r, true
			}
			report := func(rep modules.ReportRequest) bool {
				if q.tok == "" {
					if err := bt.Report(batchOf(rep)); err != nil {
						fail("built-in report: %v", err)
						return false
					}
					return true
				}
				if rec := e.agentCall("report", q.tok, "m-"+q.tok[len(q.tok)-6:], batchOf(rep)); rec.Code != 200 {
					fail("report: %d %s", rec.Code, rec.Body.String())
					return false
				}
				return true
			}
			workers := 1 + i%2
			held := map[int64]int64{} // running request -> set
			known := map[int64]bool{}
			next := 0
			sig := ""
			for time.Now().Before(deadline) {
				free := len(held) < workers
				req := modules.SyncRequest{CommandSig: sig, RunningRequests: []int64{}, FreeWorker: free}
				for id := range held {
					req.RunningRequests = append(req.RunningRequests, id)
				}
				resp, ok := doSync(req)
				if !ok {
					return
				}
				if resp.CommandSets != nil {
					sig = resp.CommandSets.Signature
				}
				var fresh []modules.CommandRunRequest
				for _, cr := range resp.CommandRequests {
					if known[cr.ID] {
						fail("request %d handed over again after the agent answered it", cr.ID)
						continue
					}
					fresh = append(fresh, cr)
				}
				switch {
				case len(fresh) > 1:
					fail("%d requests handed over by one sync", len(fresh))
				case len(fresh) == 1 && !free:
					fail("request %d handed over by a sync without free_worker", fresh[0].ID)
				case len(fresh) == 1 && (next >= len(mine) || fresh[0].ID != mine[next]):
					fail("request %d handed over, the agent's oldest queued is %v", fresh[0].ID, mine[min(next, len(mine)-1)])
				}
				var rep modules.ReportRequest
				for _, cr := range fresh {
					known[cr.ID] = true
					next++
					held[cr.ID] = cr.SetID
					mu.Lock()
					runs[cr.ID]++
					mu.Unlock()
					uid := "run-" + itoa64(cr.ID)
					rep.CommandRequestAcks = append(rep.CommandRequestAcks, modules.CommandRequestAck{RequestID: cr.ID, Status: modules.RequestAckStarted, RunUID: uid, At: 1000})
					rep.CommandRuns = append(rep.CommandRuns, runReport(uid, cr.SetID, "s", cr.ID, []modules.CommandResultReport{}, false, ""))
				}
				dropped := false
				for id, set := range held {
					if rng.IntN(2) != 0 {
						continue
					}
					// The end comes with a report, or only by a later sync's
					// list (a closing report the agent lost).
					if rng.IntN(3) != 0 {
						rep.CommandRuns = append(rep.CommandRuns, runReport("run-"+itoa64(id), set, "s", id, []modules.CommandResultReport{}, true, modules.ResultSuccess))
					} else {
						dropped = true
					}
					delete(held, id)
				}
				if len(rep.CommandRuns) > 0 || len(rep.CommandRequestAcks) > 0 {
					if !report(rep) {
						return
					}
				}
				if next == len(mine) && len(held) == 0 {
					var open int
					if err := e.c.db.Read().QueryRow(`SELECT count(*) FROM command_requests WHERE agent_id = ? AND state != 'done'`, q.a.ID).Scan(&open); err != nil {
						fail("read: %v", err)
						return
					}
					if open == 0 {
						return
					}
				}
				if dropped {
					// A sync's list ends only the requests that became
					// running before the second its handling began.
					time.Sleep(1100 * time.Millisecond)
				} else {
					time.Sleep(time.Duration(rng.IntN(20)) * time.Millisecond)
				}
			}
			fail("not done before the deadline")
		})
	}
	wg.Wait()
	if len(problems) > 0 {
		t.Fatalf("%d problems, first: %s", len(problems), problems[0])
	}
	var open int
	_ = e.c.db.Read().QueryRow(`SELECT count(*) FROM command_requests WHERE state != 'done'`).Scan(&open)
	if open != 0 {
		t.Fatalf("%d requests not done", open)
	}
	for _, ids := range own {
		for _, id := range ids {
			if runs[id] != 1 {
				t.Fatalf("request %d ran %d times", id, runs[id])
			}
		}
	}
}
