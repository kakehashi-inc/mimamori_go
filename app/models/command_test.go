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

func TestCommandRequestModel(t *testing.T) {
	inTx(t, openDB(t, modules.SchemaAgentDB), func(tx models.Writer) {
		s := newSet("disk")
		if err := models.InsertCommandSet(tx, s, 100); err != nil {
			t.Fatal(err)
		}
		r := &models.CommandRequest{SetID: s.ID, RequestedAt: 100, RequestedBy: "a", ExpiresAt: 100 + 86400}
		if err := models.InsertCommandRequest(tx, r); err != nil || r.State != models.RequestPending {
			t.Fatal(err)
		}
		if err := models.InsertCommandRequest(tx, &models.CommandRequest{SetID: s.ID, RequestedAt: 101, ExpiresAt: 2}); !errors.Is(err, models.ErrRequestActive) {
			t.Fatalf("second request: %v", err)
		}
		if act, _ := models.ActiveCommandRequest(tx, s.ID); act == nil || act.ID != r.ID {
			t.Fatal("active request not found")
		}
		if err := models.MarkCommandRequestDelivered(tx, r.ID, 110); err != nil {
			t.Fatal(err)
		}
		if err := models.DiscardCommandRequest(tx, r.ID, "a", 111); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("discard of a delivered request: %v", err)
		}
		if err := models.InsertCommandRequest(tx, &models.CommandRequest{SetID: s.ID, RequestedAt: 112, ExpiresAt: 3}); !errors.Is(err, models.ErrRequestActive) {
			t.Fatalf("request while delivered: %v", err)
		}
		if err := models.MarkCommandRequestDone(tx, r.ID, "u1", 120); err != nil {
			t.Fatal(err)
		}
		if got, _ := models.GetCommandRequest(tx, r.ID); got.State != models.RequestDone || got.RunUID != "u1" || got.DeliveredAt != 110 {
			t.Fatalf("done: %+v", got)
		}

		r2 := &models.CommandRequest{SetID: s.ID, RequestedAt: 200, ExpiresAt: 300}
		if err := models.InsertCommandRequest(tx, r2); err != nil {
			t.Fatalf("new request after done: %v", err)
		}
		if list, _ := models.ListActiveCommandRequests(tx); len(list) != 1 || list[0].ID != r2.ID {
			t.Fatalf("active list: %+v", list)
		}
		if exp, _ := models.ExpireCommandRequests(tx, 299); len(exp) != 0 {
			t.Fatal("expired too early")
		}
		exp, err := models.ExpireCommandRequests(tx, 300)
		if err != nil || len(exp) != 1 || exp[0].ID != r2.ID || exp[0].State != models.RequestExpired {
			t.Fatalf("expire: %v %+v", err, exp)
		}
		r3 := &models.CommandRequest{SetID: s.ID, RequestedAt: 400, ExpiresAt: 500}
		if err := models.InsertCommandRequest(tx, r3); err != nil {
			t.Fatal(err)
		}
		if err := models.DiscardCommandRequest(tx, r3.ID, "b", 401); err != nil {
			t.Fatal(err)
		}
		if got, _ := models.GetCommandRequest(tx, r3.ID); got.State != models.RequestDiscarded || got.DiscardedBy != "b" {
			t.Fatalf("discarded: %+v", got)
		}
		r4 := &models.CommandRequest{SetID: s.ID, RequestedAt: 600, ExpiresAt: 700}
		if err := models.InsertCommandRequest(tx, r4); err != nil {
			t.Fatal(err)
		}
		if err := models.MarkCommandRequestExpired(tx, r4.ID, 800); err != nil {
			t.Fatal(err)
		}
		if n, _ := models.DeleteEndedCommandRequestsBefore(tx, 450); n != 3 {
			t.Fatalf("retention removed %d", n)
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
