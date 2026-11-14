package workers

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// The queue of run requests: the central's item workers is the largest
// number of run requests in progress at agents (pending, delivered,
// running), all agents together.

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

// queueInvariant checks, inside a transaction, that the requests in
// progress of responding agents are at most workers and that no worker is
// left unused while a request of a responding agent waits.
func queueInvariant(c *core, tx models.Reader, now int64) error {
	list, err := models.ListOpenCommandRequests(tx)
	if err != nil {
		return err
	}
	agents, err := models.ListAgentsWithOpenCommandRequests(tx)
	if err != nil {
		return err
	}
	responding := map[int64]bool{}
	for _, a := range agents {
		responding[a.ID] = agentResponding(tx, a, c.conf().DefaultCheckIntervalSec, now)
	}
	busy, waiting := 0, 0
	for _, r := range list {
		switch {
		case !responding[r.AgentID]:
		case r.State == models.RequestQueued && r.ExpiresAt > now:
			waiting++
		case r.State != models.RequestQueued:
			busy++
		}
	}
	if busy > c.conf().Workers {
		return fmt.Errorf("%d requests in progress with workers %d", busy, c.conf().Workers)
	}
	if busy < c.conf().Workers && waiting > 0 {
		return fmt.Errorf("%d requests in progress with workers %d while %d wait", busy, c.conf().Workers, waiting)
	}
	return nil
}

// Q2: workers 2, requests to three responding agents A, B, C in this
// order: A and B get the workers, C waits; when A's run ends, C is granted
// in the same call.
func TestQueueOrderOverAgents(t *testing.T) {
	e := newTestEnv(t)
	if e.c.conf().Workers != 2 {
		t.Fatalf("default workers %d", e.c.conf().Workers)
	}
	var ags []*queueAgent
	for _, n := range []string{"qa", "qb", "qc"} {
		q := newQueueAgent(t, e, n, 1)
		syncWith(t, e, q.tok, modules.SyncRequest{})
		ags = append(ags, q)
	}
	var ids []int64
	for i, want := range []string{models.RequestPending, models.RequestPending, models.RequestQueued} {
		id, st := requestRun(t, e, ags[i].a, ags[i].sets[0])
		if st != want {
			t.Fatalf("request of agent %d: %s, want %s", i, st, want)
		}
		ids = append(ids, id)
	}
	A, C := ags[0], ags[2]
	if got := handedIDs(syncWith(t, e, C.tok, modules.SyncRequest{})); len(got) != 0 {
		t.Fatalf("queued request handed over: %v", got)
	}
	if got := handedIDs(syncWith(t, e, A.tok, modules.SyncRequest{})); len(got) != 1 || got[0] != ids[0] {
		t.Fatalf("A's answer: %v", got)
	}
	wantStates(t, e, "A handed over", A.a.ID, ids[0], models.RequestDelivered, C.a.ID, ids[2], models.RequestQueued)
	finishRun(t, e, A.tok, A.sets[0], ids[0])
	wantStates(t, e, "A ended", A.a.ID, ids[0], models.RequestDone, C.a.ID, ids[2], models.RequestPending)
	if got := handedIDs(syncWith(t, e, C.tok, modules.SyncRequest{})); len(got) != 1 || got[0] != ids[2] {
		t.Fatalf("C's answer: %v", got)
	}
}

// Many syncs and reports of different agents at the same moment (through
// the HTTPS handler, and the built-in agent's direct calls) never let more
// requests be in progress than workers, never leave a worker unused while a
// request of a responding agent waits, and grant by request id.
func TestQueueConcurrentSyncs(t *testing.T) {
	e := newTestEnv(t)
	const agents, sets = 50, 4
	var ags []*queueAgent
	for i := range agents - 1 {
		q := newQueueAgent(t, e, fmt.Sprintf("qc%02d", i), sets)
		syncWith(t, e, q.tok, modules.SyncRequest{})
		ags = append(ags, q)
	}
	b, err := models.GetBuiltinAgent(e.c.db.Read())
	if err != nil {
		t.Fatal(err)
	}
	builtin := &queueAgent{a: b, sets: addQueueSets(t, e, b, sets)}
	bt := &inProcessTransport{c: e.c, agentID: b.ID}
	if _, err := bt.Sync(&modules.SyncRequest{AgentVersion: "test"}); err != nil {
		t.Fatal(err)
	}
	ags = append(ags, builtin)

	// An agent with nothing to do waits as a real one waits for its
	// interval, but is woken when one of its requests is granted, so the
	// test does not wait for intervals.
	wake := map[int64]chan struct{}{}
	for _, q := range ags {
		wake[q.a.ID] = make(chan struct{}, 1)
	}
	var mu sync.Mutex
	var problems []string
	var lastGrant int64
	grants := 0
	e.c.queueChecked = func(tx models.Reader, now int64, granted []*models.CommandRequest) {
		mu.Lock()
		defer mu.Unlock()
		for _, r := range granted {
			if r.ID <= lastGrant {
				problems = append(problems, fmt.Sprintf("request %d granted after %d", r.ID, lastGrant))
			}
			lastGrant = r.ID
			grants++
			select {
			case wake[r.AgentID] <- struct{}{}:
			default:
			}
		}
		if err := queueInvariant(e.c, tx, now); err != nil {
			problems = append(problems, err.Error())
		}
	}
	// All requests at once, in an order mixing the agents.
	total := 0
	for s := range sets {
		for _, q := range ags {
			requestRun(t, e, q.a, q.sets[s])
			total++
		}
	}

	var done atomic.Bool
	var wg sync.WaitGroup
	deadline := time.Now().Add(4 * time.Minute)
	for i, q := range ags {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(i), 7))
			fail := func(format string, args ...any) {
				mu.Lock()
				problems = append(problems, fmt.Sprintf(format, args...))
				mu.Unlock()
				done.Store(true)
			}
			held := map[int64]int64{} // request id -> set id
			sig := ""
			for !done.Load() && time.Now().Before(deadline) {
				req := modules.SyncRequest{AgentVersion: "test", CommandSig: sig, RunningRequests: []int64{}}
				for id := range held {
					req.RunningRequests = append(req.RunningRequests, id)
				}
				var resp *modules.SyncResponse
				if q.tok == "" {
					r, err := bt.Sync(&req)
					if err != nil {
						fail("built-in sync: %v", err)
						return
					}
					resp = r
				} else {
					rec := e.agentCall("sync", q.tok, "m-"+q.tok[len(q.tok)-6:], req)
					if rec.Code != 200 {
						fail("sync: %d %s", rec.Code, rec.Body.String())
						return
					}
					r := decode[modules.SyncResponse](t, rec)
					resp = &r
				}
				if resp.CommandSets != nil {
					sig = resp.CommandSets.Signature
				}
				var rep modules.ReportRequest
				for _, cr := range resp.CommandRequests {
					if _, ok := held[cr.ID]; ok {
						continue
					}
					held[cr.ID] = cr.SetID
					uid := "run-" + itoa64(cr.ID)
					rep.CommandRequestAcks = append(rep.CommandRequestAcks, modules.CommandRequestAck{RequestID: cr.ID, Status: modules.RequestAckStarted, RunUID: uid, At: 1000})
					rep.CommandRuns = append(rep.CommandRuns, runReport(uid, cr.SetID, "s", cr.ID, []modules.CommandResultReport{}, false, ""))
				}
				dropped := false
				for id, set := range held {
					if rng.IntN(2) != 0 {
						continue
					}
					// The end comes with a report, or only by the next
					// sync's list (a closing report the agent lost).
					if rng.IntN(2) == 0 {
						rep.CommandRuns = append(rep.CommandRuns, runReport("run-"+itoa64(id), set, "s", id, []modules.CommandResultReport{}, true, modules.ResultSuccess))
					} else {
						dropped = true
					}
					delete(held, id)
				}
				if len(rep.CommandRuns) == 0 && len(rep.CommandRequestAcks) == 0 {
					if len(held) == 0 && !dropped {
						select {
						case <-wake[q.a.ID]:
						case <-time.After(time.Duration(2000+rng.IntN(2000)) * time.Millisecond):
						}
					}
					continue
				}
				if q.tok == "" {
					if err := bt.Report(&rep); err != nil {
						fail("built-in report: %v", err)
						return
					}
				} else if rec := e.agentCall("report", q.tok, "m-"+q.tok[len(q.tok)-6:], rep); rec.Code != 200 {
					fail("report: %d %s", rec.Code, rec.Body.String())
					return
				}
			}
		}()
	}
	for !done.Load() && time.Now().Before(deadline) {
		var open int
		if err := e.c.db.Read().QueryRow(`SELECT count(*) FROM command_requests WHERE state != 'done'`).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open == 0 {
			done.Store(true)
		}
		time.Sleep(20 * time.Millisecond)
	}
	done.Store(true)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(problems) > 0 {
		t.Fatalf("%d problems, first: %s", len(problems), problems[0])
	}
	var open int
	_ = e.c.db.Read().QueryRow(`SELECT count(*) FROM command_requests WHERE state != 'done'`).Scan(&open)
	if open != 0 || grants != total {
		t.Fatalf("%d of %d requests not done, %d grants", open, total, grants)
	}
}

// Q4: an agent that stops syncing holds no worker and blocks nobody; when
// it returns its requests count again (above workers for a while, with no
// grant until below), and its queued request is granted and handed over in
// the answer to its sync once a worker is free.
func TestQueueSilentAgent(t *testing.T) {
	e := newTestEnv(t)
	var logs bytes.Buffer
	var logMu sync.Mutex
	e.c.logger = log.New(lockedWriter{&logs, &logMu}, "", 0)
	A := newQueueAgent(t, e, "silent-a", 2)
	B := newQueueAgent(t, e, "silent-b", 2)
	syncWith(t, e, A.tok, modules.SyncRequest{})
	syncWith(t, e, B.tok, modules.SyncRequest{})

	a1, _ := requestRun(t, e, A.a, A.sets[0])
	syncWith(t, e, A.tok, modules.SyncRequest{}) // a1 delivered
	a2, _ := requestRun(t, e, A.a, A.sets[1])
	b1, _ := requestRun(t, e, B.a, B.sets[0])
	b2, _ := requestRun(t, e, B.a, B.sets[1])
	wantStates(t, e, "start", A.a.ID, a1, models.RequestDelivered, A.a.ID, a2, models.RequestPending,
		B.a.ID, b1, models.RequestQueued, B.a.ID, b2, models.RequestQueued)

	// A is silent for longer than 3 x 60 + 600 s.
	now := time.Now()
	setLastSync(t, e, A.a.ID, now.Unix()-781)
	if list := e.admin("GET", "/agents/"+itoa64(A.a.ID)+"/command-sets", nil); list["agent_responding"] != false {
		t.Fatalf("agent_responding of the silent agent: %v", list["agent_responding"])
	}
	e.c.runRequestsStep(now)
	wantStates(t, e, "A silent", A.a.ID, a1, models.RequestDelivered, A.a.ID, a2, models.RequestQueued,
		B.a.ID, b1, models.RequestPending, B.a.ID, b2, models.RequestPending)
	logMu.Lock()
	line := fmt.Sprintf("run request %d of silent-a waits again: the agent has not synced since ", a2)
	if strings.Count(logs.String(), line) != 1 {
		t.Fatalf("log: %q", logs.String())
	}
	logMu.Unlock()

	// A returns: a1 counts again (3 in progress with workers 2), a2 waits.
	if got := handedIDs(syncWith(t, e, A.tok, modules.SyncRequest{})); len(got) != 1 || got[0] != a1 {
		t.Fatalf("A's answer: %v", got)
	}
	wantStates(t, e, "A back", A.a.ID, a2, models.RequestQueued)
	syncWith(t, e, B.tok, modules.SyncRequest{})
	finishRun(t, e, B.tok, B.sets[0], b1)
	wantStates(t, e, "b1 ended, still 2 in progress", A.a.ID, a2, models.RequestQueued)
	finishRun(t, e, B.tok, B.sets[1], b2)
	wantStates(t, e, "b2 ended", A.a.ID, a2, models.RequestPending)
	if got := handedIDs(syncWith(t, e, A.tok, modules.SyncRequest{})); len(got) != 2 || got[1] != a2 {
		t.Fatalf("A's answer with a2: %v", got)
	}

	// A silent again with a queued request: it is granted and handed over
	// in the answer to the sync that brings A back.
	finishRun(t, e, A.tok, A.sets[0], a1)
	finishRun(t, e, A.tok, A.sets[1], a2)
	setLastSync(t, e, A.a.ID, time.Now().Unix()-781)
	a3, st := requestRun(t, e, A.a, A.sets[0])
	if st != models.RequestQueued {
		t.Fatalf("request of a silent agent: %s", st)
	}
	if got := handedIDs(syncWith(t, e, A.tok, modules.SyncRequest{})); len(got) != 1 || got[0] != a3 {
		t.Fatalf("answer of the returning agent: %v", got)
	}
	wantStates(t, e, "a3", A.a.ID, a3, models.RequestDelivered)
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

// An agent that never synced is not responding: its requests wait without a
// worker and block nobody; its first sync grants and hands over.
func TestQueueNeverSyncedAgent(t *testing.T) {
	e := newTestEnv(t)
	A := newQueueAgent(t, e, "never-a", 1)
	B := newQueueAgent(t, e, "never-b", 2)
	syncWith(t, e, B.tok, modules.SyncRequest{})
	a1, st := requestRun(t, e, A.a, A.sets[0])
	if st != models.RequestQueued {
		t.Fatalf("request of an agent that never synced: %s", st)
	}
	if list := e.admin("GET", "/agents/"+itoa64(A.a.ID)+"/command-sets", nil); list["agent_responding"] != false {
		t.Fatalf("agent_responding: %v", list["agent_responding"])
	}
	b1, st := requestRun(t, e, B.a, B.sets[0])
	if st != models.RequestPending {
		t.Fatalf("request of B behind A's: %s", st)
	}
	if got := handedIDs(syncWith(t, e, A.tok, modules.SyncRequest{})); len(got) != 1 || got[0] != a1 {
		t.Fatalf("first sync's answer: %v", got)
	}
	wantStates(t, e, "after the first sync", A.a.ID, a1, models.RequestDelivered, B.a.ID, b1, models.RequestPending)
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
	syncWith(t, e, A.tok, modules.SyncRequest{})
	r1, _ := requestRun(t, e, A.a, A.sets[0])
	r2, _ := requestRun(t, e, A.a, A.sets[1])
	syncWith(t, e, A.tok, modules.SyncRequest{})
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

// What a report does to run requests (1.7).
func TestQueueReportTransitions(t *testing.T) {
	e := newTestEnv(t)
	A := newQueueAgent(t, e, "rep-a", 4)
	B := newQueueAgent(t, e, "rep-b", 1)
	syncWith(t, e, A.tok, modules.SyncRequest{})
	syncWith(t, e, B.tok, modules.SyncRequest{})
	setConfig(e, func(c *modules.CentralConfig) { c.Workers = 10 })
	var ids []int64
	for _, s := range A.sets {
		id, _ := requestRun(t, e, A.a, s)
		ids = append(ids, id)
	}
	syncWith(t, e, A.tok, modules.SyncRequest{})

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

// Q6: a lowered worker count takes nothing back; a raised one takes effect
// in the answer of PUT /config, or after the reload and the periodic step
// when the settings row is changed by central config.
func TestQueueWorkersChange(t *testing.T) {
	e := newTestEnv(t)
	A := newQueueAgent(t, e, "wc", 7)
	syncWith(t, e, A.tok, modules.SyncRequest{})
	var ids []int64
	for _, s := range A.sets[:3] {
		id, _ := requestRun(t, e, A.a, s)
		ids = append(ids, id)
	}
	wantStates(t, e, "workers 2", A.a.ID, ids[0], models.RequestPending, A.a.ID, ids[1], models.RequestPending, A.a.ID, ids[2], models.RequestQueued)

	e.admin("PUT", "/config", map[string]any{"set": map[string]string{"workers": "1"}})
	if e.c.conf().Workers != 1 {
		t.Fatalf("workers in use: %d", e.c.conf().Workers)
	}
	wantStates(t, e, "lowered", A.a.ID, ids[0], models.RequestPending, A.a.ID, ids[1], models.RequestPending, A.a.ID, ids[2], models.RequestQueued)
	syncWith(t, e, A.tok, modules.SyncRequest{})
	finishRun(t, e, A.tok, A.sets[0], ids[0])
	wantStates(t, e, "one ended", A.a.ID, ids[2], models.RequestQueued)
	finishRun(t, e, A.tok, A.sets[1], ids[1])
	wantStates(t, e, "both ended", A.a.ID, ids[2], models.RequestPending)

	// Raised on the settings screen: granted in that request.
	for _, s := range A.sets[3:5] {
		id, _ := requestRun(t, e, A.a, s)
		ids = append(ids, id)
	}
	wantStates(t, e, "before the raise", A.a.ID, ids[3], models.RequestQueued, A.a.ID, ids[4], models.RequestQueued)
	out := e.admin("PUT", "/config", map[string]any{"set": map[string]string{"workers": "3"}})
	for _, it := range out["items"].([]any) {
		if m := it.(map[string]any); m["name"] == "workers" && (m["restart"] != false || m["pending"] != false) {
			t.Fatalf("workers item: %v", m)
		}
	}
	wantStates(t, e, "raised", A.a.ID, ids[3], models.RequestPending, A.a.ID, ids[4], models.RequestPending)

	// Raised by a settings row (central config set): after the reload and
	// the step "run requests".
	for _, s := range A.sets[5:7] {
		id, _ := requestRun(t, e, A.a, s)
		ids = append(ids, id)
	}
	if err := e.exec(`UPDATE settings SET value = '5' WHERE key = 'config.workers'`); err != nil {
		t.Fatal(err)
	}
	e.c.runRequestsStep(time.Now())
	wantStates(t, e, "row changed, not reloaded", A.a.ID, ids[5], models.RequestQueued)
	e.c.reloadConfig()
	e.c.runRequestsStep(time.Now())
	wantStates(t, e, "reloaded", A.a.ID, ids[5], models.RequestPending, A.a.ID, ids[6], models.RequestPending)
}

// Expiry of requests not handed over, discard and set deletion free their
// workers at once.
func TestQueueExpiryAndDiscard(t *testing.T) {
	e := newTestEnv(t)
	A := newQueueAgent(t, e, "exp", 5)
	syncWith(t, e, A.tok, modules.SyncRequest{})
	path := "/agents/" + itoa64(A.a.ID)
	d1, _ := requestRun(t, e, A.a, A.sets[0])
	syncWith(t, e, A.tok, modules.SyncRequest{}) // d1 delivered
	p1, _ := requestRun(t, e, A.a, A.sets[1])
	q1, _ := requestRun(t, e, A.a, A.sets[2])
	q2, _ := requestRun(t, e, A.a, A.sets[3])
	wantStates(t, e, "start", A.a.ID, d1, models.RequestDelivered, A.a.ID, p1, models.RequestPending,
		A.a.ID, q1, models.RequestQueued, A.a.ID, q2, models.RequestQueued)

	// Discard of a delivered request: 409. Discard of the pending one
	// frees its worker for q1 in the same request.
	if rec := e.cli("DELETE", path+"/command-requests/"+itoa64(d1), nil); rec.Code != 409 {
		t.Fatalf("discard of delivered: %d", rec.Code)
	}
	e.admin("DELETE", path+"/command-requests/"+itoa64(p1), nil)
	wantStates(t, e, "pending discarded", A.a.ID, p1, models.RequestDiscarded, A.a.ID, q1, models.RequestPending, A.a.ID, q2, models.RequestQueued)
	// Discard of a queued one.
	e.admin("DELETE", path+"/command-requests/"+itoa64(q2), nil)
	wantStates(t, e, "queued discarded", A.a.ID, q2, models.RequestDiscarded)

	// Set deletion discards its pending request and grants the next.
	q3, _ := requestRun(t, e, A.a, A.sets[4])
	wantStates(t, e, "q3", A.a.ID, q3, models.RequestQueued)
	e.admin("DELETE", path+"/command-sets/"+itoa64(A.sets[2]), nil)
	wantStates(t, e, "set deleted", A.a.ID, q1, models.RequestDiscarded, A.a.ID, q3, models.RequestPending)

	// Expiry: queued and pending past their expiry end, delivered not.
	q4, _ := requestRun(t, e, A.a, A.sets[1])
	wantStates(t, e, "q4", A.a.ID, q4, models.RequestQueued)
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

// The central closed and opened again on the same data directory: every
// request keeps its state and the queue goes on from the stored rows.
func TestQueueRestart(t *testing.T) {
	e := newTestEnv(t)
	A := newQueueAgent(t, e, "rs", 3)
	syncWith(t, e, A.tok, modules.SyncRequest{})
	r1, _ := requestRun(t, e, A.a, A.sets[0])
	syncWith(t, e, A.tok, modules.SyncRequest{})
	r2, _ := requestRun(t, e, A.a, A.sets[1])
	r3, _ := requestRun(t, e, A.a, A.sets[2])
	wantStates(t, e, "before", A.a.ID, r1, models.RequestDelivered, A.a.ID, r2, models.RequestPending, A.a.ID, r3, models.RequestQueued)

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
	e = &testEnv{t: t, c: c, agent: c.agentHandler(), web: c.webHandler(), cliKey: c.cliSecret}
	wantStates(t, e, "after", A.a.ID, r1, models.RequestDelivered, A.a.ID, r2, models.RequestPending, A.a.ID, r3, models.RequestQueued)
	if got := handedIDs(syncWith(t, e, A.tok, modules.SyncRequest{})); len(got) != 2 || got[0] != r1 || got[1] != r2 {
		t.Fatalf("answer after the restart: %v", got)
	}
	finishRun(t, e, A.tok, A.sets[0], r1)
	wantStates(t, e, "queue goes on", A.a.ID, r3, models.RequestPending)
}
