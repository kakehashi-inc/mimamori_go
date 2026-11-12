package models

import (
	"database/sql"
	"errors"
)

// RunStateRunning marks a run that has not finished; finished runs carry the
// worst result kind of their commands (success, failure or error).
const RunStateRunning = "running"

// CommandRun is a row of the command_runs table of a per-target database: one
// run of a set, reported by the agent. The set's values at the start are
// copied (SetName, Purpose, Joiner, OSUser, WorkDir). FinishedAt is 0 while
// the run is going on. DisplayRows is the JSON of the display value
// (modules.DisplayValue; empty for run_only sets and unfinished runs).
type CommandRun struct {
	ID               int64
	RunUID           string
	SetID            int64
	SetRevision      int64
	SetName          string
	Purpose          string
	Joiner           string
	OSUser           string
	WorkDir          string
	Trigger          string
	RequestID        int64
	ScheduledFor     int64
	CommandsTotal    int
	StartedAt        int64
	FinishedAt       int64
	State            string
	Reason           string
	DisplayRows      string
	DisplayTruncated bool
	ReceivedAt       int64
	UpdatedAt        int64
}

const commandRunCols = `id, run_uid, set_id, set_revision, set_name, purpose, joiner, os_user, work_dir, run_trigger, request_id,
	scheduled_for, commands_total, started_at, finished_at, state, reason, display_rows, display_truncated, received_at, updated_at`

func scanCommandRun(s rowScanner) (*CommandRun, error) {
	r := &CommandRun{}
	var trunc int
	if err := s.Scan(&r.ID, &r.RunUID, &r.SetID, &r.SetRevision, &r.SetName, &r.Purpose, &r.Joiner, &r.OSUser, &r.WorkDir,
		&r.Trigger, &r.RequestID, &r.ScheduledFor, &r.CommandsTotal, &r.StartedAt, &r.FinishedAt, &r.State, &r.Reason,
		&r.DisplayRows, &trunc, &r.ReceivedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.DisplayTruncated = trunc != 0
	return r, nil
}

// EnsureCommandRun stores a run reported for the first time and returns its
// row; a run already stored (same RunUID) is returned unchanged with created
// false. New rows start in the running state.
func EnsureCommandRun(db Execer, r *CommandRun, now int64) (*CommandRun, bool, error) {
	// Looked up first: every attempted insert takes an AUTOINCREMENT number,
	// even when the conflict clause skips it (the clause stays as the safety
	// net).
	if stored, err := GetCommandRunByUID(db, r.RunUID); err == nil {
		return stored, false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	res, err := db.Exec(`INSERT INTO command_runs (run_uid, set_id, set_revision, set_name, purpose, joiner, os_user, work_dir,
		run_trigger, request_id, scheduled_for, commands_total, started_at, state, received_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(run_uid) DO NOTHING`,
		r.RunUID, r.SetID, r.SetRevision, r.SetName, r.Purpose, r.Joiner, r.OSUser, r.WorkDir, r.Trigger, r.RequestID,
		r.ScheduledFor, r.CommandsTotal, r.StartedAt, RunStateRunning, now, now)
	if err != nil {
		return nil, false, err
	}
	n, _ := res.RowsAffected()
	stored, err := GetCommandRunByUID(db, r.RunUID)
	return stored, n > 0, err
}

// FinishCommandRun records the end of a run that is still running. It
// returns sql.ErrNoRows when the run is missing or already finished.
func FinishCommandRun(db Execer, id int64, state, reason string, finishedAt int64, displayRows string, truncated bool, now int64) error {
	return execOne(db, `UPDATE command_runs SET state = ?, reason = ?, finished_at = ?, display_rows = ?, display_truncated = ?,
		updated_at = ? WHERE id = ? AND finished_at = 0`, state, reason, finishedAt, displayRows, boolToInt(truncated), now, id)
}

// TouchCommandRun records that a report about the run arrived.
func TouchCommandRun(db Execer, id int64, now int64) error {
	_, err := db.Exec(`UPDATE command_runs SET updated_at = ? WHERE id = ?`, now, id)
	return err
}

// GetCommandRun returns a run.
func GetCommandRun(db Execer, id int64) (*CommandRun, error) {
	return scanCommandRun(db.QueryRow(`SELECT `+commandRunCols+` FROM command_runs WHERE id = ?`, id))
}

// GetCommandRunByUID returns the run with the unique ID the agent gave it.
func GetCommandRunByUID(db Execer, uid string) (*CommandRun, error) {
	return scanCommandRun(db.QueryRow(`SELECT `+commandRunCols+` FROM command_runs WHERE run_uid = ?`, uid))
}

// ListCommandRuns returns the runs of a set, newest first, at most limit
// rows, only those with an ID below beforeID when it is above 0.
func ListCommandRuns(db Execer, setID int64, limit int, beforeID int64) ([]*CommandRun, error) {
	q := `SELECT ` + commandRunCols + ` FROM command_runs WHERE set_id = ?`
	args := []any{setID}
	if beforeID > 0 {
		q += ` AND id < ?`
		args = append(args, beforeID)
	}
	rows, err := db.Query(q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	return collect(rows, err, scanCommandRun)
}

// LatestFinishedCommandRun returns the last finished run of a set
// (sql.ErrNoRows when there is none).
func LatestFinishedCommandRun(db Execer, setID int64) (*CommandRun, error) {
	return scanCommandRun(db.QueryRow(`SELECT `+commandRunCols+` FROM command_runs WHERE set_id = ? AND finished_at > 0
		ORDER BY finished_at DESC, id DESC LIMIT 1`, setID))
}

// ListLatestFinishedCommandRuns returns the last finished run of every set
// that has one, by set ID.
func ListLatestFinishedCommandRuns(db Execer) (map[int64]*CommandRun, error) {
	rows, err := db.Query(`SELECT ` + commandRunCols + ` FROM command_runs r WHERE finished_at > 0 AND id = (
		SELECT id FROM command_runs WHERE set_id = r.set_id AND finished_at > 0 ORDER BY finished_at DESC, id DESC LIMIT 1)`)
	list, err := collect(rows, err, scanCommandRun)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]*CommandRun, len(list))
	for _, r := range list {
		out[r.SetID] = r
	}
	return out, nil
}

// RunningCommandRunSets returns the IDs of the sets that have a run going on.
func RunningCommandRunSets(db Execer) (map[int64]bool, error) {
	rows, err := db.Query(`SELECT DISTINCT set_id FROM command_runs WHERE finished_at = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// DeleteCommandRunsBefore removes runs received before t (retention), with
// their command results.
func DeleteCommandRunsBefore(db Execer, t int64) (int64, error) {
	res, err := db.Exec(`DELETE FROM command_runs WHERE received_at < ?`, t)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteCommandRunsBeyondKeep removes the finished runs of a set beyond the
// newest keep finished ones. Runs going on are not counted or removed.
func DeleteCommandRunsBeyondKeep(db Execer, setID int64, keep int) (int64, error) {
	res, err := db.Exec(`DELETE FROM command_runs WHERE set_id = ? AND finished_at > 0 AND id NOT IN (
		SELECT id FROM command_runs WHERE set_id = ? AND finished_at > 0 ORDER BY finished_at DESC, id DESC LIMIT ?)`, setID, setID, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CommandRunExists reports whether a run with the unique ID is stored.
func CommandRunExists(db Execer, uid string) (bool, error) {
	_, err := GetCommandRunByUID(db, uid)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
