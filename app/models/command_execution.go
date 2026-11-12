package models

import (
	"database/sql"
	"errors"
)

// CommandExecution is a row of the command_executions table of the agent's
// local database: a run the agent started (RunUID is unique). FinishedAt is 0
// while it is going on; a row still unfinished when the agent starts belongs
// to a run that was interrupted. RequestID (when above 0) is unique, so a
// manual request is never run twice.
type CommandExecution struct {
	RunUID       string
	SetID        int64
	SetRevision  int64
	RequestID    int64
	Trigger      string
	ScheduledFor int64
	StartedAt    int64
	FinishedAt   int64
}

const commandExecutionCols = `run_uid, set_id, set_revision, request_id, run_trigger, scheduled_for, started_at, finished_at`

func scanCommandExecution(s rowScanner) (*CommandExecution, error) {
	e := &CommandExecution{}
	if err := s.Scan(&e.RunUID, &e.SetID, &e.SetRevision, &e.RequestID, &e.Trigger, &e.ScheduledFor, &e.StartedAt, &e.FinishedAt); err != nil {
		return nil, err
	}
	return e, nil
}

// ErrAlreadyExecuted is returned when a run of the same manual request was
// already started.
var ErrAlreadyExecuted = errors.New("the run request was already executed")

// InsertCommandExecution records the start of a run. It returns
// ErrAlreadyExecuted when the request ID was already used.
func InsertCommandExecution(db Execer, e *CommandExecution) error {
	_, err := db.Exec(`INSERT INTO command_executions (`+commandExecutionCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.RunUID, e.SetID, e.SetRevision, e.RequestID, e.Trigger, e.ScheduledFor, e.StartedAt, e.FinishedAt)
	if isUniqueViolation(err) {
		return ErrAlreadyExecuted
	}
	return err
}

// FinishCommandExecution records the end of a run.
func FinishCommandExecution(db Execer, runUID string, now int64) error {
	return execOne(db, `UPDATE command_executions SET finished_at = ? WHERE run_uid = ? AND finished_at = 0`, now, runUID)
}

// ListUnfinishedCommandExecutions returns the runs that were started and not
// finished, oldest first.
func ListUnfinishedCommandExecutions(db Execer) ([]*CommandExecution, error) {
	rows, err := db.Query(`SELECT ` + commandExecutionCols + ` FROM command_executions WHERE finished_at = 0 ORDER BY started_at, run_uid`)
	return collect(rows, err, scanCommandExecution)
}

// CommandExecutionByRequest returns the run started for a manual request
// (nil when there is none).
func CommandExecutionByRequest(db Execer, requestID int64) (*CommandExecution, error) {
	e, err := scanCommandExecution(db.QueryRow(`SELECT `+commandExecutionCols+` FROM command_executions WHERE request_id = ?`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

// DeleteFinishedCommandExecutionsBefore removes runs that finished before t.
func DeleteFinishedCommandExecutionsBefore(db Execer, t int64) (int64, error) {
	res, err := db.Exec(`DELETE FROM command_executions WHERE finished_at > 0 AND finished_at < ?`, t)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
