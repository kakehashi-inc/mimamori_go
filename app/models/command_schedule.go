package models

import (
	"database/sql"
	"errors"
)

// CommandSchedule is a row of the command_schedules table of the agent's
// local database: the schedule state of a set. ScheduleSig identifies the
// schedule definition; ArmedAt is when the agent first saw it (occurrences
// before it are not run) and LastDue the latest occurrence already run.
type CommandSchedule struct {
	SetID       int64
	ScheduleSig string
	ArmedAt     int64
	LastDue     int64
}

// GetCommandSchedule returns the schedule state of a set (nil when absent).
func GetCommandSchedule(db Reader, setID int64) (*CommandSchedule, error) {
	s := &CommandSchedule{}
	err := db.QueryRow(`SELECT set_id, schedule_sig, armed_at, last_due FROM command_schedules WHERE set_id = ?`, setID).
		Scan(&s.SetID, &s.ScheduleSig, &s.ArmedAt, &s.LastDue)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// ArmCommandSchedule stores the schedule state of a set when its definition
// is new or changed (sig differs): it is armed at now and has no occurrence
// run yet. An unchanged definition keeps its state. It returns the stored
// state.
func ArmCommandSchedule(db Writer, setID int64, sig string, now int64) (*CommandSchedule, error) {
	if _, err := db.Exec(`INSERT INTO command_schedules (set_id, schedule_sig, armed_at, last_due) VALUES (?, ?, ?, 0)
		ON CONFLICT(set_id) DO UPDATE SET schedule_sig = excluded.schedule_sig, armed_at = excluded.armed_at, last_due = 0
		WHERE command_schedules.schedule_sig <> excluded.schedule_sig`, setID, sig, now); err != nil {
		return nil, err
	}
	return GetCommandSchedule(db, setID)
}

// SetCommandScheduleLastDue records the latest occurrence run.
func SetCommandScheduleLastDue(db Writer, setID, due int64) error {
	return execOne(db, `UPDATE command_schedules SET last_due = ? WHERE set_id = ?`, due, setID)
}

// DeleteCommandSchedulesExcept removes the state of every set not in keep
// (sets that were deleted or are no longer scheduled).
func DeleteCommandSchedulesExcept(db Writer, keep []int64) error {
	rows, err := db.Query(`SELECT set_id FROM command_schedules`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	wanted := make(map[int64]bool, len(keep))
	for _, id := range keep {
		wanted[id] = true
	}
	for _, id := range ids {
		if wanted[id] {
			continue
		}
		if _, err := db.Exec(`DELETE FROM command_schedules WHERE set_id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}
