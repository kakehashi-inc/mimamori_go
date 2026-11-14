package models_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"mimamori/app/models"
	"mimamori/app/modules"
)

func openDB(t *testing.T, schema string) *modules.DB {
	t.Helper()
	modules.MigrationsFS = os.DirFS("../..")
	db, err := modules.OpenSQLite(filepath.Join(t.TempDir(), schema+".db"), schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// inTx runs fn in one write transaction of db and commits it when fn
// returns: the model functions under test read and write through tx, as the
// code calls them inside a Write. t.Fatal inside fn rolls the transaction
// back.
func inTx(t *testing.T, db *modules.DB, fn func(tx models.Writer)) {
	t.Helper()
	if err := db.Write(func(tx models.Writer) error {
		fn(tx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func newSet(name string) *models.CommandSet {
	return &models.CommandSet{Name: name, Purpose: "collect_output", OSUser: "app", RunMode: "manual", KeepRuns: 100, Joiner: "comma", CreatedBy: "admin@example.com"}
}

func TestDBInstance(t *testing.T) {
	a, err := models.GetDBInstance(openDB(t, modules.SchemaAgentDB).Read())
	if err != nil || len(a) != 32 {
		t.Fatalf("instance %q %v", a, err)
	}
	b, _ := models.GetDBInstance(openDB(t, modules.SchemaAgentDB).Read())
	if a == b {
		t.Fatal("two databases have the same instance value")
	}
}

func TestReceivedReport(t *testing.T) {
	db := openDB(t, modules.SchemaAgentDB)
	if got, err := models.LastReceivedReport(db.Read()); err != nil || got != "" {
		t.Fatalf("empty: %q %v", got, err)
	}
	for _, id := range []string{"0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"} {
		inTx(t, db, func(tx models.Writer) {
			if err := models.SetLastReceivedReport(tx, id, 1000); err != nil {
				t.Fatal(err)
			}
		})
		if got, err := models.LastReceivedReport(db.Read()); err != nil || got != id {
			t.Fatalf("read back: %q %v, want %q", got, err, id)
		}
	}
	var rows int
	if err := db.Read().QueryRow(`SELECT count(*) FROM received_report`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows: %d %v", rows, err)
	}
}

func TestCommandSetModel(t *testing.T) {
	inTx(t, openDB(t, modules.SchemaAgentDB), func(tx models.Writer) {
		s := newSet("disk")
		if err := models.InsertCommandSet(tx, s, 100); err != nil || s.ID == 0 || s.Revision != 1 {
			t.Fatalf("insert: %v %+v", err, s)
		}
		if err := models.InsertCommandSet(tx, newSet("disk"), 100); !errors.Is(err, models.ErrNameTaken) {
			t.Fatalf("duplicate name: %v", err)
		}
		other := newSet("mem")
		if err := models.InsertCommandSet(tx, other, 100); err != nil {
			t.Fatal(err)
		}
		other.Name = "disk"
		if err := models.UpdateCommandSet(tx, other, 101); !errors.Is(err, models.ErrNameTaken) {
			t.Fatalf("rename to a taken name: %v", err)
		}
		s.Description, s.UpdatedBy = "changed", "b@example.com"
		if err := models.UpdateCommandSet(tx, s, 102); err != nil || s.Revision != 2 {
			t.Fatalf("update: %v rev %d", err, s.Revision)
		}
		got, _ := models.GetCommandSet(tx, s.ID)
		if got.Description != "changed" || got.Revision != 2 || got.UpdatedAt != 102 || got.CreatedBy != "admin@example.com" {
			t.Fatalf("stored: %+v", got)
		}
		taken, _ := models.CommandSetNameTaken(tx, "disk", s.ID)
		if taken {
			t.Fatal("own name counted as taken")
		}
		taken, _ = models.CommandSetNameTaken(tx, "disk", other.ID)
		if !taken {
			t.Fatal("name not taken")
		}

		items := []*models.CommandItem{
			{CommandLine: "df -h", SuccessKind: "single", FailureKind: "min", FailureA: 1, OnProblems: "stop", TimeoutSec: 300, LineLimit: 20, Patterns: `["(?P<u>\\d+)%"]`},
			{CommandLine: "df -h", SuccessKind: "range", SuccessA: 0, SuccessB: 1, FailureKind: "single", FailureA: 2, OnProblems: "continue_always", TimeoutSec: 5, LineLimit: 1},
		}
		if err := models.ReplaceCommandItems(tx, s.ID, items); err != nil {
			t.Fatal(err)
		}
		list, _ := models.ListCommandItems(tx, s.ID)
		if len(list) != 2 || list[0].Position != 0 || list[1].Position != 1 || list[1].Patterns != "[]" || list[1].SuccessB != 1 {
			t.Fatalf("items: %+v %+v", list[0], list[1])
		}
		if err := models.ReplaceCommandItems(tx, s.ID, items[:1]); err != nil {
			t.Fatal(err)
		}
		if list, _ := models.ListCommandItems(tx, s.ID); len(list) != 1 {
			t.Fatalf("items not replaced: %d", len(list))
		}

		revs, _ := models.CommandSetRevisions(tx)
		if !reflect.DeepEqual(revs, map[int64]int64{s.ID: 2, other.ID: 1}) {
			t.Fatalf("revisions: %v", revs)
		}
		if err := models.DeleteCommandSet(tx, s.ID, "admin", 103); err != nil {
			t.Fatal(err)
		}
		if err := models.DeleteCommandSet(tx, s.ID, "admin", 104); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("second delete: %v", err)
		}
		if err := models.UpdateCommandSet(tx, s, 105); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("update of a deleted set: %v", err)
		}
		sets, _ := models.ListCommandSets(tx)
		if len(sets) != 1 || sets[0].ID != other.ID {
			t.Fatalf("list after delete: %d", len(sets))
		}
		// The name of a deleted set can be used again.
		again := newSet("disk")
		if err := models.InsertCommandSet(tx, again, 106); err != nil {
			t.Fatalf("reuse of a deleted name: %v", err)
		}
		if byName, err := models.GetCommandSetByName(tx, "disk"); err != nil || byName.ID != again.ID {
			t.Fatalf("by name: %v", err)
		}
		if deleted, _ := models.GetCommandSet(tx, s.ID); deleted.DeletedAt != 103 || deleted.DeletedBy != "admin" {
			t.Fatalf("deleted row: %+v", deleted)
		}
	})
}

func TestCommandRunModels(t *testing.T) {
	inTx(t, openDB(t, modules.SchemaAgentDB), func(tx models.Writer) {
		s := newSet("disk")
		if err := models.InsertCommandSet(tx, s, 100); err != nil {
			t.Fatal(err)
		}
		r := &models.CommandRun{RunUID: "u1", SetID: s.ID, SetRevision: 1, SetName: "disk", Purpose: "collect_output", Joiner: "comma", Trigger: "manual", CommandsTotal: 2, StartedAt: 200}
		stored, created, err := models.EnsureCommandRun(tx, r, 201)
		if err != nil || !created || stored.State != models.RunStateRunning || stored.ReceivedAt != 201 {
			t.Fatalf("ensure: %v %v %+v", err, created, stored)
		}
		again, created, err := models.EnsureCommandRun(tx, r, 300)
		if err != nil || created || again.ID != stored.ID || again.ReceivedAt != 201 {
			t.Fatalf("ensure again: %v %v %+v", err, created, again)
		}
		// Reports of a stored run use no ID: the next run gets the next number.
		for i := 0; i < 3; i++ {
			if _, _, err := models.EnsureCommandRun(tx, r, int64(301+i)); err != nil {
				t.Fatal(err)
			}
		}
		next, created, err := models.EnsureCommandRun(tx, &models.CommandRun{RunUID: "u0", SetID: s.ID, Trigger: "manual", StartedAt: 199}, 310)
		if err != nil || !created || next.ID != stored.ID+1 {
			t.Fatalf("run IDs must not skip numbers: %v %v %d after %d", err, created, next.ID, stored.ID)
		}
		if _, err := tx.Exec(`DELETE FROM command_runs WHERE run_uid = 'u0'`); err != nil {
			t.Fatal(err)
		}

		code := sql.NullInt64{Int64: 0, Valid: true}
		res := &models.CommandResult{RunID: stored.ID, Position: 0, CommandLine: "df", Kind: "success", ExitCode: code, Output: []byte("a\xffb"), OutputBytes: 3}
		if ok, err := models.InsertCommandResult(tx, res); err != nil || !ok {
			t.Fatalf("result: %v %v", ok, err)
		}
		dup := *res
		dup.Kind = "error"
		if ok, err := models.InsertCommandResult(tx, &dup); err != nil || ok {
			t.Fatalf("duplicate result: %v %v", ok, err)
		}
		if ok, _ := models.InsertCommandResult(tx, &models.CommandResult{RunID: stored.ID, Position: 1, Kind: "error", Reason: "timeout"}); !ok {
			t.Fatal("second result not stored")
		}
		results, _ := models.ListCommandResults(tx, stored.ID)
		if len(results) != 2 || results[0].Kind != "success" || string(results[0].Output) != "a\xffb" || results[1].ExitCode.Valid || results[1].Output == nil {
			t.Fatalf("results: %+v", results)
		}
		kinds, _ := models.CommandResultKinds(tx, stored.ID)
		if !reflect.DeepEqual(kinds, []string{"success", "error"}) {
			t.Fatalf("kinds: %v", kinds)
		}

		running, _ := models.RunningCommandRunSets(tx)
		if !running[s.ID] {
			t.Fatal("running set not listed")
		}
		if err := models.FinishCommandRun(tx, stored.ID, "error", "", 250, `{"rows":[]}`, true, 251); err != nil {
			t.Fatal(err)
		}
		if err := models.FinishCommandRun(tx, stored.ID, "success", "", 260, "", false, 261); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("second finish: %v", err)
		}
		fin, _ := models.GetCommandRun(tx, stored.ID)
		if fin.State != "error" || fin.FinishedAt != 250 || !fin.DisplayTruncated || fin.DisplayRows != `{"rows":[]}` {
			t.Fatalf("finished: %+v", fin)
		}
		if ex, _ := models.CommandRunExists(tx, "u1"); !ex {
			t.Fatal("run not found by uid")
		}

		// Keep count: only finished runs count; the newest are kept.
		for i, uid := range []string{"u2", "u3", "u4"} {
			run, _, err := models.EnsureCommandRun(tx, &models.CommandRun{RunUID: uid, SetID: s.ID, SetName: "disk", Purpose: "run_only", Trigger: "schedule", StartedAt: int64(300 + i)}, int64(400+i))
			if err != nil {
				t.Fatal(err)
			}
			if uid != "u4" {
				if err := models.FinishCommandRun(tx, run.ID, "success", "", int64(310+i), "", false, 0); err != nil {
					t.Fatal(err)
				}
			}
		}
		latest, err := models.LatestFinishedCommandRun(tx, s.ID)
		if err != nil || latest.RunUID != "u3" {
			t.Fatalf("latest: %v %+v", err, latest)
		}
		all, _ := models.ListLatestFinishedCommandRuns(tx)
		if len(all) != 1 || all[s.ID].RunUID != "u3" {
			t.Fatalf("latest per set: %+v", all)
		}
		if n, err := models.DeleteCommandRunsBeyondKeep(tx, s.ID, 2); err != nil || n != 1 {
			t.Fatalf("keep: %d %v", n, err)
		}
		runs, _ := models.ListCommandRuns(tx, s.ID, 10, 0)
		var uids []string
		for _, r := range runs {
			uids = append(uids, r.RunUID)
		}
		if !reflect.DeepEqual(uids, []string{"u4", "u3", "u2"}) {
			t.Fatalf("runs after keep: %v", uids)
		}
		if page, _ := models.ListCommandRuns(tx, s.ID, 1, runs[0].ID); len(page) != 1 || page[0].RunUID != "u3" {
			t.Fatalf("page: %+v", page)
		}
		// The results of a deleted run go with it.
		if res, _ := models.ListCommandResults(tx, stored.ID); len(res) != 0 {
			t.Fatal("results of the deleted run remain")
		}
		// Retention by age removes every run received before the limit, the
		// last one of a set too.
		if n, _ := models.DeleteCommandRunsBefore(tx, 1000); n != 3 {
			t.Fatalf("retention removed %d", n)
		}
	})
}

// The run requests of all agents are rows of one table of the central
// database. Every transition changes one row, only from its allowed states
// and only of its own agent; the unique index allows one unanswered request
// per set.
func TestCommandRequestModel(t *testing.T) {
	inTx(t, openDB(t, modules.SchemaCentral), func(tx models.Writer) {
		agentA, agentB := &models.Agent{Kind: models.AgentBuiltin}, &models.Agent{Kind: models.AgentBuiltin}
		for _, ag := range []*models.Agent{agentA, agentB} {
			if err := models.InsertAgent(tx, ag); err != nil {
				t.Fatal(err)
			}
		}
		A, B := agentA.ID, agentB.ID
		state := func(id int64) string {
			t.Helper()
			r, err := models.GetCommandRequest(tx, A, id)
			if err != nil {
				t.Fatalf("request %d: %v", id, err)
			}
			return r.State
		}
		insert := func(agent, set, at int64) *models.CommandRequest {
			t.Helper()
			r := &models.CommandRequest{AgentID: agent, SetID: set, RequestedAt: at, RequestedBy: "a", ExpiresAt: at + 100}
			if err := models.InsertCommandRequest(tx, r); err != nil {
				t.Fatalf("insert: %v", err)
			}
			return r
		}
		mustChange := func(what string, ok bool, err error, want bool) {
			t.Helper()
			if err != nil || ok != want {
				t.Fatalf("%s: changed %v (want %v), %v", what, ok, want, err)
			}
		}

		// queued; a second unanswered request of the set is refused, the
		// same set id of another agent is another set.
		r := insert(A, 1, 100)
		if r.State != models.RequestQueued || state(r.ID) != models.RequestQueued {
			t.Fatalf("inserted: %+v", r)
		}
		if err := models.InsertCommandRequest(tx, &models.CommandRequest{AgentID: A, SetID: 1, RequestedAt: 101, ExpiresAt: 2}); !errors.Is(err, models.ErrRequestActive) {
			t.Fatalf("second request: %v", err)
		}
		rb := insert(B, 1, 100)
		if _, err := models.GetCommandRequest(tx, A, rb.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("request of another agent read: %v", err)
		}
		if un, _ := models.UnansweredCommandRequests(tx, A); len(un) != 1 || un[1] == nil || un[1].ID != r.ID {
			t.Fatalf("unanswered: %+v", un)
		}

		// queued -> pending -> queued -> pending; nothing else from queued.
		ok, err := models.ReleaseCommandRequest(tx, r.ID)
		mustChange("release of queued", ok, err, false)
		ok, err = models.MarkCommandRequestRunning(tx, A, r.ID, "u")
		mustChange("running from queued", ok, err, false)
		ok, err = models.MarkCommandRequestDone(tx, A, r.ID, "u", 1)
		mustChange("done from queued", ok, err, false)
		ok, err = models.MarkCommandRequestExpired(tx, A, r.ID, 1)
		mustChange("expired from queued", ok, err, false)
		ok, err = models.GrantCommandRequest(tx, r.ID)
		mustChange("grant", ok, err, true)
		ok, err = models.GrantCommandRequest(tx, r.ID)
		mustChange("grant of pending", ok, err, false)
		ok, err = models.ReleaseCommandRequest(tx, r.ID)
		mustChange("release", ok, err, true)
		ok, err = models.GrantCommandRequest(tx, r.ID)
		mustChange("grant again", ok, err, true)
		if state(r.ID) != models.RequestPending {
			t.Fatal("not pending")
		}

		// Hand-over: pending of the agent (expiry not passed) -> delivered;
		// every delivered request is returned, the other agent's are not.
		if _, err := models.GrantCommandRequest(tx, rb.ID); err != nil {
			t.Fatal(err)
		}
		list, err := models.HandOverCommandRequests(tx, A, 150)
		if err != nil || len(list) != 1 || list[0].ID != r.ID || list[0].State != models.RequestDelivered || list[0].DeliveredAt != 150 {
			t.Fatalf("hand-over: %v %+v", err, list)
		}
		if list, _ = models.HandOverCommandRequests(tx, A, 160); len(list) != 1 || list[0].DeliveredAt != 150 {
			t.Fatalf("hand-over again: %+v", list)
		}
		if rbNow, _ := models.GetCommandRequest(tx, B, rb.ID); rbNow.State != models.RequestPending {
			t.Fatalf("other agent's request handed over: %+v", rbNow)
		}
		if list, _ = models.HandOverCommandRequests(tx, B, 200); len(list) != 0 {
			t.Fatalf("hand-over past the expiry: %+v", list)
		}
		ok, err = models.DiscardCommandRequest(tx, A, r.ID, "a", 151)
		mustChange("discard of delivered", ok, err, false)
		if err := models.InsertCommandRequest(tx, &models.CommandRequest{AgentID: A, SetID: 1, RequestedAt: 152, ExpiresAt: 3}); !errors.Is(err, models.ErrRequestActive) {
			t.Fatalf("request while delivered: %v", err)
		}

		// delivered -> running (only by its own agent) -> done.
		ok, err = models.MarkCommandRequestRunning(tx, B, r.ID, "u1")
		mustChange("running by another agent", ok, err, false)
		ok, err = models.MarkCommandRequestRunning(tx, A, r.ID, "u1")
		mustChange("running", ok, err, true)
		ok, err = models.MarkCommandRequestExpired(tx, A, r.ID, 1)
		mustChange("expired from running", ok, err, false)
		// A new request of the set while the first one runs.
		r2 := insert(A, 1, 170)
		ok, err = models.MarkCommandRequestDone(tx, B, r.ID, "", 180)
		mustChange("done by another agent", ok, err, false)
		ok, err = models.MarkCommandRequestDone(tx, A, r.ID, "", 180)
		mustChange("done", ok, err, true)
		if got, _ := models.GetCommandRequest(tx, A, r.ID); got.State != models.RequestDone || got.RunUID != "u1" || got.DoneAt != 180 || got.DeliveredAt != 150 {
			t.Fatalf("done: %+v", got)
		}
		ok, err = models.MarkCommandRequestDone(tx, A, r.ID, "u1", 181)
		mustChange("done twice", ok, err, false)

		// delivered -> done directly, with the run's uid.
		if _, err := models.GrantCommandRequest(tx, r2.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := models.HandOverCommandRequests(tx, A, 171); err != nil {
			t.Fatal(err)
		}
		ok, err = models.MarkCommandRequestDone(tx, A, r2.ID, "u2", 190)
		mustChange("done from delivered", ok, err, true)
		if got, _ := models.GetCommandRequest(tx, A, r2.ID); got.RunUID != "u2" {
			t.Fatalf("uid: %+v", got)
		}

		// delivered -> expired; only from delivered.
		r3 := insert(A, 1, 200)
		ok, err = models.MarkCommandRequestExpired(tx, A, r3.ID, 201)
		mustChange("expired from queued", ok, err, false)
		_, _ = models.GrantCommandRequest(tx, r3.ID)
		_, _ = models.HandOverCommandRequests(tx, A, 202)
		ok, err = models.MarkCommandRequestExpired(tx, B, r3.ID, 203)
		mustChange("expired by another agent", ok, err, false)
		ok, err = models.MarkCommandRequestExpired(tx, A, r3.ID, 203)
		mustChange("expired", ok, err, true)

		// The open list over all agents, by id.
		r4 := insert(A, 1, 300)
		r5 := insert(A, 2, 301)
		_, _ = models.GrantCommandRequest(tx, r5.ID)
		r6 := insert(A, 3, 302)
		_, _ = models.GrantCommandRequest(tx, r6.ID)
		_, _ = models.HandOverCommandRequests(tx, A, 303)
		_, _ = models.MarkCommandRequestRunning(tx, A, r6.ID, "u6")
		open, err := models.ListOpenCommandRequests(tx)
		var ids []int64
		for _, o := range open {
			ids = append(ids, o.ID)
		}
		if err != nil || !reflect.DeepEqual(ids, []int64{rb.ID, r4.ID, r5.ID, r6.ID}) {
			t.Fatalf("open: %v %v", err, ids)
		}
		if ags, err := models.ListAgentsWithOpenCommandRequests(tx); err != nil || len(ags) != 2 || ags[0].ID != A || ags[1].ID != B {
			t.Fatalf("agents with open requests: %v %+v", err, ags)
		}

		// running requests the agent no longer holds end; delivered ones
		// and listed ones are not touched.
		r7 := insert(A, 4, 304)
		_, _ = models.GrantCommandRequest(tx, r7.ID)
		_, _ = models.HandOverCommandRequests(tx, A, 305)
		_, _ = models.MarkCommandRequestRunning(tx, A, r7.ID, "u7")
		if n, err := models.EndUnheldCommandRequests(tx, A, []int64{r7.ID}, 310); err != nil || n != 1 {
			t.Fatalf("end unheld: %d %v", n, err)
		}
		if state(r6.ID) != models.RequestDone || state(r7.ID) != models.RequestRunning || state(r5.ID) != models.RequestDelivered {
			t.Fatalf("after end unheld: %s %s %s", state(r6.ID), state(r7.ID), state(r5.ID))
		}
		if n, _ := models.EndUnheldCommandRequests(tx, B, nil, 311); n != 0 {
			t.Fatalf("other agent ended %d", n)
		}
		if n, _ := models.EndUnheldCommandRequests(tx, A, []int64{}, 312); n != 1 || state(r7.ID) != models.RequestDone {
			t.Fatalf("empty list: %d %s", n, state(r7.ID))
		}

		// Expiry ends queued and pending requests past their expiry only.
		r8 := insert(A, 5, 400)
		_, _ = models.GrantCommandRequest(tx, r8.ID)
		if exp, _ := models.ExpireCommandRequests(tx, 399); len(exp) != 1 || exp[0].ID != rb.ID {
			t.Fatalf("expired too early: %+v", exp)
		}
		exp, err := models.ExpireCommandRequests(tx, 500)
		var expIDs []int64
		for _, x := range exp {
			expIDs = append(expIDs, x.ID)
			if x.State != models.RequestExpired || x.DoneAt != 500 {
				t.Fatalf("expired row: %+v", x)
			}
		}
		if err != nil || !reflect.DeepEqual(expIDs, []int64{r4.ID, r8.ID}) || state(r5.ID) != models.RequestDelivered {
			t.Fatalf("expire: %v %v %s", err, expIDs, state(r5.ID))
		}

		// Discard: queued and pending of the own agent only.
		r9 := insert(A, 6, 600)
		ok, err = models.DiscardCommandRequest(tx, B, r9.ID, "b", 601)
		mustChange("discard by another agent", ok, err, false)
		ok, err = models.DiscardCommandRequest(tx, A, r9.ID, "b", 601)
		mustChange("discard of queued", ok, err, true)
		if got, _ := models.GetCommandRequest(tx, A, r9.ID); got.State != models.RequestDiscarded || got.DiscardedBy != "b" || got.DiscardedAt != 601 {
			t.Fatalf("discarded: %+v", got)
		}
		r10 := insert(A, 6, 602)
		_, _ = models.GrantCommandRequest(tx, r10.ID)
		ok, err = models.DiscardCommandRequest(tx, A, r10.ID, "b", 603)
		mustChange("discard of pending", ok, err, true)
		r11 := insert(A, 7, 604)
		if ids, err := models.DiscardCommandRequestsOfSet(tx, B, 7, "c", 605); err != nil || len(ids) != 0 {
			t.Fatalf("set of another agent: %v %v", ids, err)
		}
		if ids, err := models.DiscardCommandRequestsOfSet(tx, A, 7, "c", 605); err != nil || !reflect.DeepEqual(ids, []int64{r11.ID}) {
			t.Fatalf("discard of the set: %v %v", ids, err)
		}
		if ids, _ := models.DiscardCommandRequestsOfSet(tx, A, 2, "c", 606); len(ids) != 0 || state(r5.ID) != models.RequestDelivered {
			t.Fatalf("delivered request discarded with its set: %v", ids)
		}

		// Retention deletes ended requests requested before the limit only.
		if n, _ := models.DeleteEndedCommandRequestsBefore(tx, 300); n != 4 {
			t.Fatalf("retention removed %d", n)
		}
		for _, id := range []int64{r.ID, r2.ID, r3.ID, rb.ID} {
			if _, err := models.GetCommandRequest(tx, A, id); !errors.Is(err, sql.ErrNoRows) {
				if _, err := models.GetCommandRequest(tx, B, id); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("request %d kept", id)
				}
			}
		}
		if state(r4.ID) != models.RequestExpired || state(r6.ID) != models.RequestDone || state(r5.ID) != models.RequestDelivered {
			t.Fatal("retention removed a request requested after the limit or an open one")
		}

		// The requests of a deleted agent go with it.
		if _, err := tx.Exec(`DELETE FROM agents WHERE id = ?`, A); err != nil {
			t.Fatal(err)
		}
		if open, _ := models.ListOpenCommandRequests(tx); len(open) != 0 {
			t.Fatalf("requests of a deleted agent: %+v", open)
		}
	})
}

func TestAgentLocalCommandModels(t *testing.T) {
	inTx(t, openDB(t, modules.SchemaAgentLocal), func(tx models.Writer) {
		e := &models.CommandExecution{RunUID: "u1", SetID: 1, SetRevision: 2, RequestID: 7, Trigger: "manual", StartedAt: 100}
		if err := models.InsertCommandExecution(tx, e); err != nil {
			t.Fatal(err)
		}
		if err := models.InsertCommandExecution(tx, &models.CommandExecution{RunUID: "u2", SetID: 1, RequestID: 7, Trigger: "manual", StartedAt: 101}); !errors.Is(err, models.ErrAlreadyExecuted) {
			t.Fatalf("request run twice: %v", err)
		}
		if err := models.InsertCommandExecution(tx, &models.CommandExecution{RunUID: "u3", SetID: 1, Trigger: "schedule", ScheduledFor: 90, StartedAt: 102}); err != nil {
			t.Fatal(err)
		}
		if err := models.InsertCommandExecution(tx, &models.CommandExecution{RunUID: "u4", SetID: 2, Trigger: "schedule", StartedAt: 103}); err != nil {
			t.Fatalf("second scheduled run: %v", err)
		}
		if got, _ := models.CommandExecutionByRequest(tx, 7); got == nil || got.RunUID != "u1" {
			t.Fatalf("by request: %+v", got)
		}
		if got, _ := models.CommandExecutionByRequest(tx, 8); got != nil {
			t.Fatal("unknown request found")
		}
		if err := models.FinishCommandExecution(tx, "u1", 150); err != nil {
			t.Fatal(err)
		}
		open, _ := models.ListUnfinishedCommandExecutions(tx)
		if len(open) != 2 || open[0].RunUID != "u3" || open[0].ScheduledFor != 90 {
			t.Fatalf("unfinished: %+v", open)
		}
		if n, _ := models.DeleteFinishedCommandExecutionsBefore(tx, 151); n != 1 {
			t.Fatalf("cleanup removed %d", n)
		}

		st, err := models.ArmCommandSchedule(tx, 1, "sig-a", 100)
		if err != nil || st.ArmedAt != 100 || st.LastDue != 0 {
			t.Fatalf("arm: %v %+v", err, st)
		}
		if err := models.SetCommandScheduleLastDue(tx, 1, 120); err != nil {
			t.Fatal(err)
		}
		st, _ = models.ArmCommandSchedule(tx, 1, "sig-a", 200)
		if st.ArmedAt != 100 || st.LastDue != 120 {
			t.Fatalf("unchanged schedule re-armed: %+v", st)
		}
		st, _ = models.ArmCommandSchedule(tx, 1, "sig-b", 300)
		if st.ArmedAt != 300 || st.LastDue != 0 {
			t.Fatalf("changed schedule not re-armed: %+v", st)
		}
		if _, err := models.ArmCommandSchedule(tx, 2, "x", 300); err != nil {
			t.Fatal(err)
		}
		if err := models.DeleteCommandSchedulesExcept(tx, []int64{2}); err != nil {
			t.Fatal(err)
		}
		if st, _ := models.GetCommandSchedule(tx, 1); st != nil {
			t.Fatal("state of a removed set kept")
		}
		if st, _ := models.GetCommandSchedule(tx, 2); st == nil {
			t.Fatal("state of a kept set removed")
		}
	})
}

func TestAgentCommandColumns(t *testing.T) {
	inTx(t, openDB(t, modules.SchemaCentral), func(tx models.Writer) {
		a := &models.Agent{Kind: models.AgentToken}
		if err := models.InsertAgent(tx, a); err != nil {
			t.Fatal(err)
		}
		if err := models.RecordAgentReport(tx, a.ID, models.AgentReportFields{HasHost: true, Hostname: "h", HostOSUser: "mimamori", TimeZone: "JST", UTCOffsetSec: 32400, Now: 5}); err != nil {
			t.Fatal(err)
		}
		if err := models.RecordAgentCommandSig(tx, a.ID, "abc"); err != nil {
			t.Fatal(err)
		}
		got, _ := models.GetAgent(tx, a.ID)
		if got.HostOSUser != "mimamori" || got.TimeZone != "JST" || got.UTCOffsetSec != 32400 || got.CommandSig != "abc" {
			t.Fatalf("agent: %+v", got)
		}
	})
}
