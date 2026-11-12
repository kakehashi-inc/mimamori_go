package modules

import (
	"database/sql"
	"encoding/json"
	"time"

	"mimamori/app/models"
)

// StateCommandDefinitions is the key of the agent's local state that holds
// the delivered command set definitions (JSON of CommandDefinitions).
const StateCommandDefinitions = "command_defs"

// CommandSetRows converts a normalized definition into the rows of the
// command_sets and command_items tables (IDs, revision and audit columns are
// left for the caller).
func CommandSetRows(s *CommandSetSpec) (*models.CommandSet, []*models.CommandItem) {
	row := &models.CommandSet{
		Name: s.Name, Description: s.Description, Purpose: s.Purpose, OSUser: s.OSUser, WorkDir: s.WorkDir,
		RunMode: s.RunMode, KeepRuns: s.KeepRuns, Joiner: s.Joiner,
	}
	if sc := s.Schedule; sc != nil && s.RunMode == RunModeSchedule {
		row.ScheduleKind = sc.Kind
		switch sc.Kind {
		case ScheduleOnce:
			row.ScheduleAt = sc.AtTime().Unix()
		case ScheduleWeekly:
			row.ScheduleWeekdays, row.ScheduleTime = sc.WeekdayMask(), sc.TimeOfDay()
		case ScheduleDaily:
			row.ScheduleTime = sc.TimeOfDay()
		case ScheduleEveryHours, ScheduleEveryMinutes:
			row.ScheduleEvery = sc.Every
		case ScheduleHourlyAt:
			row.ScheduleMinute = sc.MinuteValue()
		}
	}
	items := make([]*models.CommandItem, 0, len(s.Commands))
	for _, c := range s.Commands {
		patterns := c.Patterns
		if patterns == nil {
			patterns = []string{}
		}
		pj, _ := json.Marshal(patterns)
		items = append(items, &models.CommandItem{
			CommandLine: c.CommandLine, Note: c.Note,
			SuccessKind: c.Success.Kind, SuccessA: c.Success.A, SuccessB: c.Success.B,
			FailureKind: c.Failure.Kind, FailureA: c.Failure.A, FailureB: c.Failure.B,
			OnProblems: c.OnProblems, TimeoutSec: c.TimeoutSec, LineLimit: c.LineLimit, Patterns: string(pj),
		})
	}
	return row, items
}

// CommandSetFromRows builds the stored definition of a set from its rows.
func CommandSetFromRows(row *models.CommandSet, items []*models.CommandItem) *CommandSetDef {
	d := &CommandSetDef{ID: row.ID, Revision: row.Revision, CommandSetSpec: CommandSetSpec{
		Name: row.Name, Description: row.Description, Purpose: row.Purpose, OSUser: row.OSUser, WorkDir: row.WorkDir,
		RunMode: row.RunMode, KeepRuns: row.KeepRuns, Joiner: row.Joiner, Commands: make([]CommandSpec, 0, len(items)),
	}}
	if row.RunMode == RunModeSchedule && row.ScheduleKind != "" {
		sc := &CommandSchedule{Kind: row.ScheduleKind}
		switch row.ScheduleKind {
		case ScheduleOnce:
			sc.At = FormatScheduleAt(time.Unix(row.ScheduleAt, 0))
		case ScheduleWeekly:
			sc.Weekdays, sc.Time = WeekdaysFromMask(row.ScheduleWeekdays), FormatTimeOfDay(row.ScheduleTime)
		case ScheduleDaily:
			sc.Time = FormatTimeOfDay(row.ScheduleTime)
		case ScheduleEveryHours, ScheduleEveryMinutes:
			sc.Every = row.ScheduleEvery
		case ScheduleHourlyAt:
			m := row.ScheduleMinute
			sc.Minute = &m
		}
		d.Schedule = sc
	}
	for _, it := range items {
		var patterns []string
		if err := json.Unmarshal([]byte(it.Patterns), &patterns); err != nil || patterns == nil {
			patterns = []string{}
		}
		d.Commands = append(d.Commands, CommandSpec{
			CommandLine: it.CommandLine, Note: it.Note,
			Success:    CodeSpec{Kind: it.SuccessKind, A: it.SuccessA, B: it.SuccessB},
			Failure:    CodeSpec{Kind: it.FailureKind, A: it.FailureA, B: it.FailureB},
			OnProblems: it.OnProblems, TimeoutSec: it.TimeoutSec, LineLimit: it.LineLimit, Patterns: patterns,
		})
	}
	return d
}

// LoadCommandSet reads a set (deleted or not) with its commands.
func LoadCommandSet(db models.Execer, id int64) (*CommandSetDef, *models.CommandSet, error) {
	row, err := models.GetCommandSet(db, id)
	if err != nil {
		return nil, nil, err
	}
	items, err := models.ListCommandItems(db, id)
	if err != nil {
		return nil, nil, err
	}
	return CommandSetFromRows(row, items), row, nil
}

// LoadCommandDefinitions reads every set of a target database that is not
// deleted, with the signature of the definitions.
func LoadCommandDefinitions(db models.Execer) (*CommandDefinitions, error) {
	instance, err := models.GetDBInstance(db)
	if err != nil {
		return nil, err
	}
	rows, err := models.ListCommandSets(db)
	if err != nil {
		return nil, err
	}
	defs := &CommandDefinitions{Sets: make([]CommandSetDef, 0, len(rows))}
	revs := make(map[int64]int64, len(rows))
	for _, r := range rows {
		items, err := models.ListCommandItems(db, r.ID)
		if err != nil {
			return nil, err
		}
		defs.Sets = append(defs.Sets, *CommandSetFromRows(r, items))
		revs[r.ID] = r.Revision
	}
	defs.Signature = CommandSignature(instance, revs)
	return defs, nil
}

// TargetCommandSignature returns the signature of a target database's
// definitions without reading the commands.
func TargetCommandSignature(db models.Execer) (string, error) {
	instance, err := models.GetDBInstance(db)
	if err != nil {
		return "", err
	}
	revs, err := models.CommandSetRevisions(db)
	if err != nil {
		return "", err
	}
	return CommandSignature(instance, revs), nil
}

// CommandResultRow converts a reported command result into its row.
func CommandResultRow(runID int64, r *CommandResultReport) *models.CommandResult {
	row := &models.CommandResult{
		RunID: runID, Position: r.Position, CommandLine: r.CommandLine, Note: r.Note, Kind: r.Kind, Reason: r.Reason,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Output: r.Output, OutputBytes: r.OutputBytes, OutputTruncated: r.Truncated,
	}
	if r.ExitCode != nil {
		row.ExitCode = sql.NullInt64{Int64: int64(*r.ExitCode), Valid: true}
	}
	if r.Values != nil {
		b, _ := json.Marshal(r.Values)
		row.ValuesJSON = string(b)
	}
	return row
}

// CommandRunRow converts a reported run into its row (the start values).
func CommandRunRow(r *CommandRunReport) *models.CommandRun {
	return &models.CommandRun{
		RunUID: r.RunUID, SetID: r.SetID, SetRevision: r.SetRevision, SetName: r.SetName, Purpose: r.Purpose, Joiner: r.Joiner,
		OSUser: r.OSUser, WorkDir: r.WorkDir, Trigger: r.Trigger, RequestID: r.RequestID, ScheduledFor: r.ScheduledFor,
		CommandsTotal: r.CommandsTotal, StartedAt: r.StartedAt,
	}
}

// ScheduleSignature identifies the schedule definition of a set on the agent
// (set ID, run mode and the schedule's JSON); a changed signature arms the
// schedule again.
func ScheduleSignature(d *CommandSetDef) string {
	b, _ := json.Marshal(struct {
		ID       int64            `json:"id"`
		RunMode  string           `json:"run_mode"`
		Schedule *CommandSchedule `json:"schedule"`
	}{d.ID, d.RunMode, d.Schedule})
	return string(b)
}
