package workers

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// A sync whose answer cannot be made (here the agent's database cannot be
// read for the command set definitions) is answered 500 and records
// nothing: the agent's last sync, the version and the signature it reported
// and the state of its run request stay as they were, and the request is
// not handed over. Once the answer can be made, the next sync records the
// sync and hands the request over.
func TestSyncAnswerReadBeforeRecording(t *testing.T) {
	e := newTestEnv(t)
	A := newQueueAgent(t, e, "broken-a", 1)
	syncWith(t, e, A.tok, modules.SyncRequest{RunningRequests: []int64{}})
	id, _ := requestRun(t, e, A.a, A.sets[0])
	before, err := models.GetAgent(e.c.db.Read(), A.a.ID)
	if err != nil {
		t.Fatal(err)
	}
	stateBefore := reqState(t, e, A.a.ID, id)

	// The agent's database becomes unreadable: closed and its file replaced.
	e.c.targetsMu.Lock()
	ent := e.c.targets[A.a.ID]
	delete(e.c.targets, A.a.ID)
	e.c.targetsMu.Unlock()
	<-ent.ready
	ent.db.Close()
	p := modules.TargetDBPath(e.c.dataDir, before.DBFile)
	if err := os.Rename(p, p+".kept"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	// A later second, so that a recorded sync would show.
	time.Sleep(1100 * time.Millisecond)
	req := modules.SyncRequest{AgentVersion: "other", CommandSig: "sig-other", RunningRequests: []int64{}, FreeWorker: true}
	for range 3 {
		if rec := e.agentCall("sync", A.tok, "m-"+A.tok[len(A.tok)-6:], req); rec.Code != 500 {
			t.Fatalf("sync with an unreadable database: %d %s", rec.Code, rec.Body.String())
		}
	}
	after, err := models.GetAgent(e.c.db.Read(), A.a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastSyncAt != before.LastSyncAt || after.AgentVersion != before.AgentVersion || after.CommandSig != before.CommandSig {
		t.Fatalf("the failed sync was recorded: last sync %d -> %d, version %q -> %q, signature %q -> %q",
			before.LastSyncAt, after.LastSyncAt, before.AgentVersion, after.AgentVersion, before.CommandSig, after.CommandSig)
	}
	if st := reqState(t, e, A.a.ID, id); st != stateBefore {
		t.Fatalf("the failed sync changed the request: %s -> %s", stateBefore, st)
	}

	// The database is readable again: the sync is recorded and the request
	// handed over.
	if err := os.Rename(p+".kept", p); err != nil {
		t.Fatal(err)
	}
	resp := syncWith(t, e, A.tok, req)
	if got := handedIDs(resp); !slices.Equal(got, []int64{id}) {
		t.Fatalf("handed over: %v", got)
	}
	if st := reqState(t, e, A.a.ID, id); st != models.RequestDelivered {
		t.Fatalf("request after the sync: %s", st)
	}
	if cur, _ := models.GetAgent(e.c.db.Read(), A.a.ID); cur.LastSyncAt <= before.LastSyncAt || cur.CommandSig != "sig-other" {
		t.Fatalf("sync not recorded: %+v", cur)
	}
}

// A sync hands a request over only together with the definition of its
// set. A set created and a run of it requested while a sync is between the
// reads for its answer and its transaction are not part of that answer:
// neither the definitions it sends nor the ones the agent holds (when its
// signature was the current one) have the set, so the request is not handed
// over by that sync. The next sync sends the set's definition and hands the
// request over.
func TestSyncHandsOverOnlyWithDefinitions(t *testing.T) {
	for _, held := range []bool{true, false} {
		name := "agent holds the definitions"
		if !held {
			name = "definitions sent with the answer"
		}
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			A := newQueueAgent(t, e, "late-a", 1)
			first := syncWith(t, e, A.tok, modules.SyncRequest{RunningRequests: []int64{}})
			if first.CommandSets == nil {
				t.Fatal("the first sync sent no definitions")
			}
			sig := first.CommandSets.Signature
			if !held {
				sig = "sig-other"
			}
			var lateSet, lateReq int64
			e.c.syncRead = func() {
				e.c.syncRead = nil
				body := commandSetBody("late", "run_only")
				body["os_user"] = "svc"
				lateSet = int64(e.admin("POST", "/agents/"+itoa64(A.a.ID)+"/command-sets", body)["id"].(float64))
				lateReq, _ = requestRun(t, e, A.a, lateSet)
			}
			req := modules.SyncRequest{CommandSig: sig, RunningRequests: []int64{}, FreeWorker: true}
			resp := syncWith(t, e, A.tok, req)
			if lateReq == 0 {
				t.Fatal("the hook did not run")
			}
			defs := first.CommandSets
			if resp.CommandSets != nil {
				defs = resp.CommandSets
			}
			if (resp.CommandSets != nil) == held {
				t.Fatalf("definitions sent: %v", resp.CommandSets != nil)
			}
			for _, r := range resp.CommandRequests {
				if !slices.ContainsFunc(defs.Sets, func(d modules.CommandSetDef) bool { return d.ID == r.SetID }) {
					t.Fatalf("request %d of set %d handed over in an answer that does not carry the set's definition", r.ID, r.SetID)
				}
			}
			if st := reqState(t, e, A.a.ID, lateReq); st != models.RequestQueued {
				t.Fatalf("request after the sync: %s", st)
			}

			// The next sync carries the set's definition and the request.
			resp = syncWith(t, e, A.tok, req)
			if resp.CommandSets == nil || !slices.ContainsFunc(resp.CommandSets.Sets, func(d modules.CommandSetDef) bool { return d.ID == lateSet }) {
				t.Fatalf("the next sync did not send the set's definition: %+v", resp.CommandSets)
			}
			if got := handedIDs(resp); !slices.Equal(got, []int64{lateReq}) {
				t.Fatalf("handed over by the next sync: %v", got)
			}
			if st := reqState(t, e, A.a.ID, lateReq); st != models.RequestDelivered {
				t.Fatalf("request after the next sync: %s", st)
			}
		})
	}
}
