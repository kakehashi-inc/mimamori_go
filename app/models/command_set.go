package models

import (
	"database/sql"
	"errors"
	"strings"
)

// ErrNameTaken is returned when a name is already used by another row.
var ErrNameTaken = errors.New("the name is already used")

// CommandSet is a row of the command_sets table of a per-target database: a
// set of commands the agent runs on its host as OSUser. Sets are deleted
// logically (DeletedAt); Revision grows with every saved change. The
// schedule columns are used only when RunMode is "schedule": ScheduleAt (once,
// Unix seconds), ScheduleWeekdays (weekly, bit 0 = Sunday), ScheduleTime
// (weekly and daily, minutes after midnight), ScheduleEvery (every_hours,
// every_minutes) and ScheduleMinute (hourly_at).
type CommandSet struct {
	ID               int64
	Name             string
	Description      string
	Purpose          string
	OSUser           string
	WorkDir          string
	RunMode          string
	ScheduleKind     string
	ScheduleAt       int64
	ScheduleWeekdays int
	ScheduleTime     int
	ScheduleEvery    int
	ScheduleMinute   int
	KeepRuns         int
	Joiner           string
	Revision         int64
	ImportedFrom     string
	CreatedAt        int64
	CreatedBy        string
	UpdatedAt        int64
	UpdatedBy        string
	DeletedAt        int64
	DeletedBy        string
}

const commandSetCols = `id, name, description, purpose, os_user, work_dir, run_mode, schedule_kind, schedule_at, schedule_weekdays,
	schedule_time, schedule_every, schedule_minute, keep_runs, joiner, revision, imported_from, created_at, created_by,
	updated_at, updated_by, deleted_at, deleted_by`

func scanCommandSet(s rowScanner) (*CommandSet, error) {
	c := &CommandSet{}
	if err := s.Scan(&c.ID, &c.Name, &c.Description, &c.Purpose, &c.OSUser, &c.WorkDir, &c.RunMode, &c.ScheduleKind,
		&c.ScheduleAt, &c.ScheduleWeekdays, &c.ScheduleTime, &c.ScheduleEvery, &c.ScheduleMinute, &c.KeepRuns, &c.Joiner,
		&c.Revision, &c.ImportedFrom, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy, &c.DeletedAt, &c.DeletedBy); err != nil {
		return nil, err
	}
	return c, nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// InsertCommandSet creates a set with revision 1. It returns ErrNameTaken when
// a set that is not deleted has the same name.
func InsertCommandSet(db Writer, c *CommandSet, now int64) error {
	c.Revision, c.CreatedAt, c.UpdatedAt, c.UpdatedBy = 1, now, now, c.CreatedBy
	res, err := db.Exec(`INSERT INTO command_sets (name, description, purpose, os_user, work_dir, run_mode, schedule_kind, schedule_at,
		schedule_weekdays, schedule_time, schedule_every, schedule_minute, keep_runs, joiner, revision, imported_from, created_at,
		created_by, updated_at, updated_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.Description, c.Purpose, c.OSUser, c.WorkDir, c.RunMode, c.ScheduleKind, c.ScheduleAt, c.ScheduleWeekdays,
		c.ScheduleTime, c.ScheduleEvery, c.ScheduleMinute, c.KeepRuns, c.Joiner, c.Revision, c.ImportedFrom, c.CreatedAt,
		c.CreatedBy, c.UpdatedAt, c.UpdatedBy)
	if isUniqueViolation(err) {
		return ErrNameTaken
	}
	if err != nil {
		return err
	}
	c.ID, _ = res.LastInsertId()
	return nil
}

// UpdateCommandSet saves the editable values of a set that is not deleted
// and increases its revision (c.Revision is set to the new value). It
// returns sql.ErrNoRows when the set is missing or deleted, ErrNameTaken when
// another set has the name.
func UpdateCommandSet(db Writer, c *CommandSet, now int64) error {
	err := execOne(db, `UPDATE command_sets SET name = ?, description = ?, purpose = ?, os_user = ?, work_dir = ?, run_mode = ?,
		schedule_kind = ?, schedule_at = ?, schedule_weekdays = ?, schedule_time = ?, schedule_every = ?, schedule_minute = ?,
		keep_runs = ?, joiner = ?, revision = revision + 1, updated_at = ?, updated_by = ? WHERE id = ? AND deleted_at = 0`,
		c.Name, c.Description, c.Purpose, c.OSUser, c.WorkDir, c.RunMode, c.ScheduleKind, c.ScheduleAt, c.ScheduleWeekdays,
		c.ScheduleTime, c.ScheduleEvery, c.ScheduleMinute, c.KeepRuns, c.Joiner, now, c.UpdatedBy, c.ID)
	if isUniqueViolation(err) {
		return ErrNameTaken
	}
	if err != nil {
		return err
	}
	c.UpdatedAt = now
	return db.QueryRow(`SELECT revision FROM command_sets WHERE id = ?`, c.ID).Scan(&c.Revision)
}

// DeleteCommandSet deletes a set logically; its runs stay until retention
// removes them. It returns sql.ErrNoRows when the set is missing or already
// deleted.
func DeleteCommandSet(db Writer, id int64, by string, now int64) error {
	return execOne(db, `UPDATE command_sets SET deleted_at = ?, deleted_by = ?, revision = revision + 1 WHERE id = ? AND deleted_at = 0`, now, by, id)
}

// GetCommandSet returns a set, deleted or not (see DeletedAt).
func GetCommandSet(db Reader, id int64) (*CommandSet, error) {
	return scanCommandSet(db.QueryRow(`SELECT `+commandSetCols+` FROM command_sets WHERE id = ?`, id))
}

// GetCommandSetByName returns the set with the name that is not deleted.
func GetCommandSetByName(db Reader, name string) (*CommandSet, error) {
	return scanCommandSet(db.QueryRow(`SELECT `+commandSetCols+` FROM command_sets WHERE name = ? AND deleted_at = 0`, name))
}

// ListCommandSets returns the sets that are not deleted, by name.
func ListCommandSets(db Reader) ([]*CommandSet, error) {
	rows, err := db.Query(`SELECT ` + commandSetCols + ` FROM command_sets WHERE deleted_at = 0 ORDER BY name, id`)
	return collect(rows, err, scanCommandSet)
}

// CommandSetNameTaken reports whether a set that is not deleted, other than
// exceptID, has the name.
func CommandSetNameTaken(db Reader, name string, exceptID int64) (bool, error) {
	var id int64
	err := db.QueryRow(`SELECT id FROM command_sets WHERE name = ? AND deleted_at = 0 AND id <> ? LIMIT 1`, name, exceptID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// CommandSetRevisions returns the revision of every set that is not deleted,
// by ID (the input of the definitions' signature).
func CommandSetRevisions(db Reader) (map[int64]int64, error) {
	rows, err := db.Query(`SELECT id, revision FROM command_sets WHERE deleted_at = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var id, rev int64
		if err := rows.Scan(&id, &rev); err != nil {
			return nil, err
		}
		out[id] = rev
	}
	return out, rows.Err()
}
