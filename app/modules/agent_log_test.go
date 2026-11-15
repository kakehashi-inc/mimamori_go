package modules

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// The agent's log lines say what happened, in English, with a subject.

// errTransport answers every request with err.
type errTransport struct{ err error }

func (f *errTransport) Sync(*SyncRequest) (*SyncResponse, error) { return nil, f.err }
func (f *errTransport) Report(*ReportBatch) error                { return f.err }

// TestLogNamesTheCentralsAnswer: a failed request is logged by what
// happened: the central answered with an error status, the central cannot
// be reached, or (the built-in agent, whose requests do not travel) the
// central could not take it.
func TestLogNamesTheCentralsAnswer(t *testing.T) {
	for _, c := range []struct {
		name   string
		err    error
		remote bool
		want   string
	}{
		{"status 500", &HTTPError{Status: 500, Body: `{"error":"internal error"}`}, true,
			"report: the central answered HTTP 500: internal error"},
		{"status 503", &HTTPError{Status: 503}, true, "report: the central answered HTTP 503: Service Unavailable"},
		{"no connection", errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), true,
			"report: cannot reach the central: dial tcp 127.0.0.1:1: connect: connection refused"},
		{"built-in agent", errors.New("database is locked (5) (SQLITE_BUSY)"), false,
			"report: the central could not take it: database is locked (5) (SQLITE_BUSY)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logs lockedLog
			rt := newCommandRuntime(t, &errTransport{err: c.err})
			rt.Logger = logs.logger()
			if c.remote {
				rt.comm.config = &AgentConfig{}
			}
			rt.flushOutbox()
			if got := strings.TrimSpace(logs.String()); got != c.want {
				t.Fatalf("log %q, want %q", got, c.want)
			}
		})
	}
}

// TestLogOfDatabaseFailures: a failure of the local database is logged with
// its subject and its consequence, and a run whose end was not stored is not
// logged as finished.
func TestLogOfDatabaseFailures(t *testing.T) {
	requireBash(t)
	var logs lockedLog
	rt := newCommandRuntime(t, &cmdTransport{})
	rt.Logger = logs.logger()
	rt.recordRetry = 20 * time.Millisecond
	set := testSet(1, PurposeRunOnly, "true")
	set.RunMode = RunModeSchedule
	set.Schedule = &CommandSchedule{Kind: ScheduleEveryMinutes, Every: 5}
	rt.storeCommandDefinitions(defsOf(set))

	// The schedule states cannot be stored when the stored definitions are
	// loaded at the start.
	execOnDB(t, rt.DB, `CREATE TRIGGER fail_arm BEFORE DELETE ON command_schedules BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	b, err := json.Marshal(defsOf(testSet(2, PurposeRunOnly, "true")))
	if err != nil {
		t.Fatal(err)
	}
	setLocalState(t, rt.DB, StateCommandDefinitions, string(b))
	logs = lockedLog{}
	rt.loadCommandDefinitions()
	if got := logs.String(); !strings.HasPrefix(got, "the stored command set definitions: storing their schedule states failed (the states stored before are used): ") {
		t.Fatalf("log of the load: %q", got)
	}
	execOnDB(t, rt.DB, `DROP TRIGGER fail_arm`)

	// The answers to run requests cannot be queued.
	execOnDB(t, rt.DB, `CREATE TRIGGER fail_acks BEFORE INSERT ON outbox WHEN NEW.payload LIKE '%command_request_acks%'
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	logs = lockedLog{}
	now := time.Now().Unix()
	receiveRequests(rt, []CommandRunRequest{{ID: 1, SetID: 2, RequestedAt: now - 100, ExpiresAt: now - 1}})
	if got := logs.String(); !strings.HasPrefix(got, "queuing the answers to run requests failed (they are made again when the requests are delivered again): ") {
		t.Fatalf("log of the answers: %q", got)
	}
	execOnDB(t, rt.DB, `DROP TRIGGER fail_acks`)

	// The batch of the queued items cannot be formed.
	execOnDB(t, rt.DB, `CREATE TRIGGER fail_batch BEFORE INSERT ON state WHEN NEW.key = 'outbox_batch'
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	logs = lockedLog{}
	rt.flushOutbox()
	if got := logs.String(); !strings.HasPrefix(got, "report: forming the batch of the queued items failed (nothing was sent; the next round tries again): ") {
		t.Fatalf("log of the batch: %q", got)
	}
	execOnDB(t, rt.DB, `DROP TRIGGER fail_batch`)

	// The end of a run cannot be stored before the stop: no "finished".
	stop := make(chan struct{})
	rt.stop, rt.sched.stop = stop, stop
	rt.storeCommandDefinitions(defsOf(testSet(3, PurposeRunOnly, "true")))
	receiveRequests(rt, manualRequests(10, 3))
	job := nextCommandJob(rt)
	execOnDB(t, rt.DB, `CREATE TRIGGER fail_end BEFORE INSERT ON outbox WHEN NEW.payload LIKE '%"finished":true%'
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	logs = lockedLog{}
	done := runInBackground(rt, job)
	waitFor(t, "a failed attempt", func() bool { return strings.Contains(logs.String(), "storing it failed") })
	close(stop)
	returned(t, done)
	if got := logs.String(); strings.Contains(got, "finished") || !strings.Contains(got, "recording the end of run") {
		t.Fatalf("log of a run whose end was not stored: %q", got)
	}
	execOnDB(t, rt.DB, `DROP TRIGGER fail_end`)
}

// TestLogOfAnInterruptedRun: a run closed by the stop before its first
// command is logged with the state and the reason of the run.
func TestLogOfAnInterruptedRun(t *testing.T) {
	var logs lockedLog
	rt, stop, _ := stopRuntime(t, &logs, "true")
	job := nextCommandJob(rt)
	rt.Logger.SetOutput(writerFunc(func(p []byte) (int, error) {
		if strings.Contains(string(p), "started (manual") {
			close(stop)
		}
		return logs.Write(p)
	}))
	rt.runCommandJob(job)
	if got := logs.String(); !strings.Contains(got, `command set "setb" finished: error (interrupted)`) {
		t.Fatalf("log: %q", got)
	}
}
