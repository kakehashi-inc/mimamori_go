package models

import "database/sql"

// CommandResult is a row of the command_results table of a per-target
// database: the result of one executed command of a run, at its Position in
// the set (from 0). Commands not executed because the set stopped have no
// row. Kind is success, failure or error (Reason names the cause of an
// error). ExitCode is not valid when the command did not end normally.
// Output holds the stored console output as bytes (up to 1 MiB; invalid
// UTF-8 is kept), OutputBytes the total size the command wrote and
// OutputTruncated whether the stored output is shorter. ValuesJSON is the
// JSON of the extracted values (modules.CommandValues; empty for run_only
// sets).
type CommandResult struct {
	ID              int64
	RunID           int64
	Position        int
	CommandLine     string
	Note            string
	Kind            string
	Reason          string
	ExitCode        sql.NullInt64
	StartedAt       int64
	FinishedAt      int64
	Output          []byte
	OutputBytes     int64
	OutputTruncated bool
	ValuesJSON      string
}

const commandResultCols = `id, run_id, position, command_line, note, kind, reason, exit_code, started_at, finished_at, output,
	output_bytes, output_truncated, values_json`

func scanCommandResult(s rowScanner) (*CommandResult, error) {
	r := &CommandResult{}
	var trunc int
	if err := s.Scan(&r.ID, &r.RunID, &r.Position, &r.CommandLine, &r.Note, &r.Kind, &r.Reason, &r.ExitCode, &r.StartedAt,
		&r.FinishedAt, &r.Output, &r.OutputBytes, &trunc, &r.ValuesJSON); err != nil {
		return nil, err
	}
	r.OutputTruncated = trunc != 0
	if r.Output == nil {
		r.Output = []byte{}
	}
	return r, nil
}

// InsertCommandResult stores the result of a command once: a result already
// stored for the run and position is kept and created is false.
func InsertCommandResult(db Execer, r *CommandResult) (bool, error) {
	out := r.Output
	if out == nil {
		out = []byte{}
	}
	res, err := db.Exec(`INSERT INTO command_results (run_id, position, command_line, note, kind, reason, exit_code, started_at,
		finished_at, output, output_bytes, output_truncated, values_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, position) DO NOTHING`,
		r.RunID, r.Position, r.CommandLine, r.Note, r.Kind, r.Reason, r.ExitCode, r.StartedAt, r.FinishedAt, out, r.OutputBytes,
		boolToInt(r.OutputTruncated), r.ValuesJSON)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		r.ID, _ = res.LastInsertId()
	}
	return n > 0, nil
}

// ListCommandResults returns the results of a run by position.
func ListCommandResults(db Execer, runID int64) ([]*CommandResult, error) {
	rows, err := db.Query(`SELECT `+commandResultCols+` FROM command_results WHERE run_id = ? ORDER BY position`, runID)
	return collect(rows, err, scanCommandResult)
}

// CommandResultKinds returns the result kinds of a run's commands by
// position (the input of the run state).
func CommandResultKinds(db Execer, runID int64) ([]string, error) {
	rows, err := db.Query(`SELECT kind FROM command_results WHERE run_id = ? ORDER BY position`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
