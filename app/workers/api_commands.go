package workers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// Command sets: definitions (administrators and the CLI), run requests, and
// results (every logged-in account). A viewer's responses carry only the
// set name, the display value, result kinds, times, and per command the
// exit code, result kind and console output: never the command line, note,
// OS user, working directory, schedule, purpose, description, patterns or
// code specs.

type commandRequestJSON struct {
	ID          int64   `json:"id"`
	SetID       int64   `json:"set_id"`
	State       string  `json:"state"`
	RequestedAt *string `json:"requested_at"`
	RequestedBy string  `json:"requested_by"`
	ExpiresAt   *string `json:"expires_at"`
	DeliveredAt *string `json:"delivered_at"`
	DoneAt      *string `json:"done_at"`
	RunUID      string  `json:"run_uid"`
	DiscardedAt *string `json:"discarded_at"`
	DiscardedBy string  `json:"discarded_by"`
}

func commandRequestOut(q *models.CommandRequest) *commandRequestJSON {
	return &commandRequestJSON{ID: q.ID, SetID: q.SetID, State: q.State, RequestedAt: ts(q.RequestedAt), RequestedBy: q.RequestedBy,
		ExpiresAt: ts(q.ExpiresAt), DeliveredAt: ts(q.DeliveredAt), DoneAt: ts(q.DoneAt), RunUID: q.RunUID,
		DiscardedAt: ts(q.DiscardedAt), DiscardedBy: q.DiscardedBy}
}

// commandRunJSON is a run. The fields with omitempty are given to
// administrators and the CLI only: a viewer does not receive the keys at
// all. ScheduledFor is a pointer to a pointer so that administrators still
// receive the key with null when a run was not started by the schedule.
type commandRunJSON struct {
	ID            int64                 `json:"id"`
	SetID         int64                 `json:"set_id"`
	SetName       string                `json:"set_name"`
	Trigger       *string               `json:"trigger,omitempty"`
	ScheduledFor  **string              `json:"scheduled_for,omitempty"`
	RequestID     *int64                `json:"request_id,omitempty"`
	CommandsTotal *int                  `json:"commands_total,omitempty"`
	StartedAt     *string               `json:"started_at"`
	FinishedAt    *string               `json:"finished_at"`
	State         string                `json:"state"`
	Reason        string                `json:"reason"`
	Display       *modules.DisplayValue `json:"display"`
	ReceivedAt    *string               `json:"received_at"`
	SetRevision   *int64                `json:"set_revision,omitempty"`
	Purpose       *string               `json:"purpose,omitempty"`
	OSUser        *string               `json:"os_user,omitempty"`
	WorkDir       *string               `json:"work_dir,omitempty"`
}

func commandRunOut(run *models.CommandRun, full bool) *commandRunJSON {
	out := &commandRunJSON{ID: run.ID, SetID: run.SetID, SetName: run.SetName, StartedAt: ts(run.StartedAt), FinishedAt: ts(run.FinishedAt),
		State: run.State, Reason: run.Reason, ReceivedAt: ts(run.ReceivedAt)}
	if run.DisplayRows != "" {
		var dv modules.DisplayValue
		if json.Unmarshal([]byte(run.DisplayRows), &dv) == nil {
			if dv.Rows == nil {
				dv.Rows = []modules.DisplayRow{}
			}
			for i := range dv.Rows {
				dv.Rows[i].Values = nonNil(dv.Rows[i].Values)
			}
			out.Display = &dv
		}
	} else if run.Purpose == modules.PurposeCollectOutput && run.FinishedAt > 0 {
		out.Display = &modules.DisplayValue{Rows: []modules.DisplayRow{}}
	}
	if full {
		trigger, due, request, total := run.Trigger, ts(run.ScheduledFor), run.RequestID, run.CommandsTotal
		out.Trigger, out.ScheduledFor, out.RequestID, out.CommandsTotal = &trigger, &due, &request, &total
		rev, purpose, user, dir := run.SetRevision, run.Purpose, run.OSUser, run.WorkDir
		out.SetRevision, out.Purpose, out.OSUser, out.WorkDir = &rev, &purpose, &user, &dir
	}
	return out
}

// commandResultJSON is the result of one executed command. CommandLine,
// Note and Values are given to administrators and the CLI only.
type commandResultJSON struct {
	Position        int                    `json:"position"`
	Kind            string                 `json:"kind"`
	Reason          string                 `json:"reason"`
	ExitCode        *int64                 `json:"exit_code"`
	StartedAt       *string                `json:"started_at"`
	FinishedAt      *string                `json:"finished_at"`
	Output          string                 `json:"output"`
	OutputBytes     int64                  `json:"output_bytes"`
	OutputTruncated bool                   `json:"output_truncated"`
	CommandLine     *string                `json:"command_line,omitempty"`
	Note            *string                `json:"note,omitempty"`
	Values          *modules.CommandValues `json:"values,omitempty"`
}

type commandRunDetailJSON struct {
	commandRunJSON
	AgentID     int64               `json:"agent_id"`
	AgentName   string              `json:"agent_name"`
	NotExecuted int                 `json:"not_executed"`
	Commands    []commandResultJSON `json:"commands"`
}

// commandSetJSON is a stored set for administrators and the CLI: the
// definition (the shape of create and update, with id and revision) and
// its state.
type commandSetJSON struct {
	modules.CommandSetDef
	ScheduleSummary string              `json:"schedule_summary"`
	ImportedFrom    string              `json:"imported_from"`
	CreatedAt       *string             `json:"created_at"`
	CreatedBy       string              `json:"created_by"`
	UpdatedAt       *string             `json:"updated_at"`
	UpdatedBy       string              `json:"updated_by"`
	Request         *commandRequestJSON `json:"request"`
	LastRun         *commandRunJSON     `json:"last_run"`
	Running         bool                `json:"running"`
	AgentProblems   []string            `json:"agent_problems"`
	// AgentProblemsI18n are the same problems with their codes, in the
	// same order (a problem from an agent of an older build is its English
	// text as data).
	AgentProblemsI18n []modules.Problem `json:"agent_problems_i18n"`
}

type commandSetListJSON struct {
	AgentID       int64  `json:"agent_id"`
	AgentName     string `json:"agent_name"`
	DefaultOSUser string `json:"default_os_user"`
	TimeZone      string `json:"time_zone"`
	UTCOffsetSec  int    `json:"utc_offset_sec"`
	Delivered     bool   `json:"definitions_delivered"`
	// AgentResponding is the central's judgement whether the agent syncs
	// (agentResponding): while false its run requests wait without a worker.
	AgentResponding bool             `json:"agent_responding"`
	Sets            []commandSetJSON `json:"sets"`
}

// commandLatestJSON is the latest finished run of a set (any account).
type commandLatestJSON struct {
	AgentID   int64           `json:"agent_id"`
	AgentName string          `json:"agent_name"`
	SetID     int64           `json:"set_id"`
	SetName   string          `json:"set_name"`
	Running   bool            `json:"running"`
	LastRun   *commandRunJSON `json:"last_run"`
	Purpose   *string         `json:"purpose,omitempty"`
}

// fullCommandView reports whether the caller receives the definition
// fields (administrators and the CLI).
func fullCommandView(r *http.Request) bool { return principalFrom(r).isAdmin() }

// hostZone returns the host's time zone of an agent for schedule summaries
// ("JST, UTC+09:00") and the location for a once time.
func hostZone(a *models.Agent) (*time.Location, string) {
	if a.TimeZone == "" {
		return time.UTC, ""
	}
	return time.FixedZone(a.TimeZone, a.UTCOffsetSec), a.TimeZone + ", " + modules.FormatUTCOffset(a.UTCOffsetSec)
}

// loadCommandTarget reads the agent of the path and opens its database.
func (c *core) loadCommandTarget(w http.ResponseWriter, r *http.Request) (*models.Agent, *modules.DB) {
	a := c.loadAgent(w, r, "id")
	if a == nil {
		return nil, nil
	}
	tdb, err := c.targetDB(a)
	if err != nil {
		c.internalError(w, r, err)
		return nil, nil
	}
	return a, tdb
}

// loadCommandSetRow reads the set of the path; deleted sets are found only
// when withDeleted is true.
func (c *core) loadCommandSetRow(w http.ResponseWriter, r *http.Request, tdb *modules.DB, withDeleted bool) *models.CommandSet {
	id, ok := pathID(r, "set")
	if !ok {
		writeError(w, http.StatusNotFound, modules.M("command.set_not_found"))
		return nil
	}
	s, err := models.GetCommandSet(tdb.Read(), id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && s.DeletedAt > 0 && !withDeleted) {
		writeError(w, http.StatusNotFound, modules.M("command.set_not_found"))
		return nil
	}
	if err != nil {
		c.internalError(w, r, err)
		return nil
	}
	return s
}

// commandSetOut builds the administrator view of a set; requests are the
// agent's unanswered run requests by set id.
func (c *core) commandSetOut(r *http.Request, a *models.Agent, tdb *modules.DB, row *models.CommandSet, problems map[int64]*setProblems,
	latest map[int64]*models.CommandRun, running map[int64]bool, requests map[int64]*models.CommandRequest) (*commandSetJSON, error) {
	items, err := models.ListCommandItems(tdb.Read(), row.ID)
	if err != nil {
		return nil, err
	}
	loc, zone := hostZone(a)
	show := c.operatorShown(r)
	def := modules.CommandSetFromRows(row, items)
	sp := problems[row.ID]
	if sp == nil {
		sp = &setProblems{}
	}
	out := &commandSetJSON{CommandSetDef: *def, ScheduleSummary: modules.ScheduleSummary(def.Schedule, loc, zone),
		ImportedFrom: row.ImportedFrom, CreatedAt: ts(row.CreatedAt), CreatedBy: show(row.CreatedBy), UpdatedAt: ts(row.UpdatedAt),
		UpdatedBy: show(row.UpdatedBy), Running: running[row.ID], AgentProblems: nonNil(sp.texts),
		AgentProblemsI18n: nonNil(sp.coded)}
	if q := requests[row.ID]; q != nil {
		out.Request = commandRequestOut(q)
	}
	if run := latest[row.ID]; run != nil {
		out.LastRun = commandRunOut(run, true)
	}
	return out, nil
}

// commandSetOne builds the administrator view of one set.
func (c *core) commandSetOne(r *http.Request, a *models.Agent, tdb *modules.DB, row *models.CommandSet) (*commandSetJSON, error) {
	latest := map[int64]*models.CommandRun{}
	if run, err := models.LatestFinishedCommandRun(tdb.Read(), row.ID); err == nil {
		latest[row.ID] = run
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	running, err := models.RunningCommandRunSets(tdb.Read())
	if err != nil {
		return nil, err
	}
	requests, err := models.UnansweredCommandRequests(c.db.Read(), a.ID)
	if err != nil {
		return nil, err
	}
	return c.commandSetOut(r, a, tdb, row, agentCommandProblems(a), latest, running, requests)
}

func (c *core) handleListCommandSets(w http.ResponseWriter, r *http.Request) {
	a, tdb := c.loadCommandTarget(w, r)
	if a == nil {
		return
	}
	rows, err := models.ListCommandSets(tdb.Read())
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	latest, err := models.ListLatestFinishedCommandRuns(tdb.Read())
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	running, err := models.RunningCommandRunSets(tdb.Read())
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	sig, err := modules.TargetCommandSignature(tdb.Read())
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	requests, err := models.UnansweredCommandRequests(c.db.Read(), a.ID)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	out := commandSetListJSON{AgentID: a.ID, AgentName: c.agentDisplayName(a), DefaultOSUser: a.HostOSUser, TimeZone: a.TimeZone,
		UTCOffsetSec: a.UTCOffsetSec, Delivered: a.CommandSig == sig, Sets: []commandSetJSON{},
		AgentResponding: agentResponding(c.db.Read(), a, c.conf().DefaultCheckIntervalSec, time.Now().Unix())}
	problems := agentCommandProblems(a)
	for _, row := range rows {
		s, err := c.commandSetOut(r, a, tdb, row, problems, latest, running, requests)
		if err != nil {
			c.internalError(w, r, err)
			return
		}
		out.Sets = append(out.Sets, *s)
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *core) handleGetCommandSet(w http.ResponseWriter, r *http.Request) {
	a, tdb := c.loadCommandTarget(w, r)
	if a == nil {
		return
	}
	row := c.loadCommandSetRow(w, r, tdb, false)
	if row == nil {
		return
	}
	out, err := c.commandSetOne(r, a, tdb, row)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// readCommandSetSpec reads a definition from the request body (strictly:
// unknown fields are an error) and fills an omitted OS user with
// defaultUser.
func readCommandSetSpec(r *http.Request, defaultUser string) (*modules.CommandSetSpec, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, invalidBody(err)
	}
	spec, err := modules.ParseCommandSetSpec(body)
	if err != nil {
		return nil, err
	}
	if spec.OSUser == "" {
		spec.OSUser = defaultUser
	}
	return spec, nil
}

// writeCommandProblems answers 400 with every validation problem.
func writeCommandProblems(w http.ResponseWriter, problems []modules.Problem) {
	writeProblems(w, http.StatusBadRequest, modules.M("command.invalid_set", "problems", problems), problems)
}

// validationTime returns the time a once schedule is checked against: 0
// (no check) when the once time is the one already stored, so that a set
// whose single run lies in the past can still be edited.
func validationTime(spec *modules.CommandSetSpec, stored *models.CommandSet, now int64) int64 {
	if sc := spec.Schedule; stored != nil && sc != nil && sc.Kind == modules.ScheduleOnce && stored.ScheduleKind == modules.ScheduleOnce {
		if at := sc.AtTime(); !at.IsZero() && at.Unix() == stored.ScheduleAt {
			return 0
		}
	}
	return now
}

// commandNameTaken is the answer to a name another set of the target has.
var commandNameTaken = modules.M("command.name_taken")

// saveCommandSet inserts (row.ID == 0) or updates a set with its commands
// in one transaction.
func saveCommandSet(tdb *modules.DB, row *models.CommandSet, items []*models.CommandItem, now int64) error {
	return tdb.Write(func(tx models.Writer) error {
		var err error
		if row.ID == 0 {
			err = models.InsertCommandSet(tx, row, now)
		} else {
			err = models.UpdateCommandSet(tx, row, now)
		}
		if err != nil {
			return err
		}
		return models.ReplaceCommandItems(tx, row.ID, items)
	})
}

func (c *core) handleCreateCommandSet(w http.ResponseWriter, r *http.Request) {
	a, tdb := c.loadCommandTarget(w, r)
	if a == nil {
		return
	}
	spec, err := readCommandSetSpec(r, a.HostOSUser)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	now := time.Now().Unix()
	if problems := spec.Problems(now); len(problems) > 0 {
		writeCommandProblems(w, problems)
		return
	}
	p := principalFrom(r)
	row, items := modules.CommandSetRows(spec)
	row.CreatedBy = p.operator()
	if err := saveCommandSet(tdb, row, items, now); errors.Is(err, models.ErrNameTaken) {
		writeError(w, http.StatusConflict, commandNameTaken)
		return
	} else if err != nil {
		c.internalError(w, r, err)
		return
	}
	c.history(p, models.HistCommand, "created", c.agentDisplayName(a)+" / "+row.Name,
		modules.M("history.detail.command_set_rev", "set", row.ID, "revision", row.Revision))
	c.respondCommandSet(w, r, a, tdb, row.ID)
}

// respondCommandSet answers with the administrator view of a stored set.
func (c *core) respondCommandSet(w http.ResponseWriter, r *http.Request, a *models.Agent, tdb *modules.DB, id int64) {
	row, err := models.GetCommandSet(tdb.Read(), id)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	out, err := c.commandSetOne(r, a, tdb, row)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *core) handleUpdateCommandSet(w http.ResponseWriter, r *http.Request) {
	a, tdb := c.loadCommandTarget(w, r)
	if a == nil {
		return
	}
	stored := c.loadCommandSetRow(w, r, tdb, false)
	if stored == nil {
		return
	}
	// An omitted OS user keeps the set's current one.
	spec, err := readCommandSetSpec(r, stored.OSUser)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	now := time.Now().Unix()
	if problems := spec.Problems(validationTime(spec, stored, now)); len(problems) > 0 {
		writeCommandProblems(w, problems)
		return
	}
	p := principalFrom(r)
	row, items := modules.CommandSetRows(spec)
	row.ID, row.UpdatedBy = stored.ID, p.operator()
	switch err := saveCommandSet(tdb, row, items, now); {
	case errors.Is(err, models.ErrNameTaken):
		writeError(w, http.StatusConflict, commandNameTaken)
		return
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, modules.M("command.set_not_found"))
		return
	case err != nil:
		c.internalError(w, r, err)
		return
	}
	detail := []modules.Msg{modules.M("history.detail.command_set_rev", "set", row.ID, "revision", row.Revision)}
	if stored.Name != row.Name {
		detail = append(detail, modules.M("history.detail.command_renamed", "name", stored.Name))
	}
	c.history(p, models.HistCommand, "updated", c.agentDisplayName(a)+" / "+row.Name, historyParts(detail))
	c.respondCommandSet(w, r, a, tdb, row.ID)
}

func (c *core) handleDeleteCommandSet(w http.ResponseWriter, r *http.Request) {
	a, tdb := c.loadCommandTarget(w, r)
	if a == nil {
		return
	}
	stored := c.loadCommandSetRow(w, r, tdb, false)
	if stored == nil {
		return
	}
	p := principalFrom(r)
	now := time.Now().Unix()
	err := tdb.Write(func(tx models.Writer) error { return models.DeleteCommandSet(tx, stored.ID, p.operator(), now) })
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, modules.M("command.set_not_found"))
		return
	} else if err != nil {
		c.internalError(w, r, err)
		return
	}
	// A request not yet handed over goes with the set (in the central
	// database, after the set's deletion), and the queue step gives its
	// worker to the next request; a delivered one is answered by the agent
	// (it no longer holds the set). The set is deleted even when this
	// fails, so the failure is logged and the deletion answered.
	var discarded []int64
	err = c.queueWrite(now, func(tx models.Writer) error {
		var err error
		discarded, err = models.DiscardCommandRequestsOfSet(tx, a.ID, stored.ID, p.operator(), now)
		return err
	}, nil)
	if err != nil {
		c.logger.Printf("run requests of the deleted command set %d of %s: %v", stored.ID, c.agentDisplayName(a), err)
	}
	detail := []modules.Msg{modules.M("history.detail.command_set", "set", stored.ID)}
	for _, id := range discarded {
		detail = append(detail, modules.M("history.detail.command_request_discarded_with", "request", id))
	}
	c.history(p, models.HistCommand, "deleted", c.agentDisplayName(a)+" / "+stored.Name, historyParts(detail))
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (c *core) handleValidateCommandSet(w http.ResponseWriter, r *http.Request) {
	a, tdb := c.loadCommandTarget(w, r)
	if a == nil {
		return
	}
	var stored *models.CommandSet
	setID, err := queryInt(r, "set_id", 0, 0, math.MaxInt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	defaultUser := a.HostOSUser
	if setID > 0 {
		s, err := models.GetCommandSet(tdb.Read(), int64(setID))
		if errors.Is(err, sql.ErrNoRows) || (err == nil && s.DeletedAt > 0) {
			writeError(w, http.StatusNotFound, modules.M("command.set_not_found"))
			return
		}
		if err != nil {
			c.internalError(w, r, err)
			return
		}
		stored, defaultUser = s, s.OSUser
	}
	spec, err := readCommandSetSpec(r, defaultUser)
	if err != nil {
		out := problemsBody([]modules.Problem{{Msg: modules.AsMsg(err)}})
		out["definition"] = nil
		writeJSON(w, http.StatusOK, out)
		return
	}
	problems := spec.Problems(validationTime(spec, stored, time.Now().Unix()))
	if spec.Name != "" {
		taken, err := models.CommandSetNameTaken(tdb.Read(), spec.Name, int64(setID))
		if err != nil {
			c.internalError(w, r, err)
			return
		}
		if taken {
			problems = append(problems, modules.Problem{Msg: commandNameTaken})
		}
	}
	out := problemsBody(problems)
	out["definition"] = spec
	writeJSON(w, http.StatusOK, out)
}

func (c *core) handleImportCommandSets(w http.ResponseWriter, r *http.Request) {
	a, tdb := c.loadCommandTarget(w, r)
	if a == nil {
		return
	}
	var in struct {
		SourceAgentID int64 `json:"source_agent_id"`
		Items         []struct {
			SetID int64  `json:"set_id"`
			Name  string `json:"name"`
		} `json:"items"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(in.Items) == 0 {
		writeError(w, http.StatusBadRequest, modules.M("command.import_select"))
		return
	}
	src, err := models.GetAgent(c.db.Read(), in.SourceAgentID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusBadRequest, modules.M("command.import_source_not_found"))
		return
	}
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	sdb, err := c.targetDB(src)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	srcName := c.agentDisplayName(src)
	type copied struct {
		from string
		spec modules.CommandSetSpec
	}
	var list []copied
	var problems []modules.Problem
	var conflicts []string
	seen := map[string]bool{}
	for i, it := range in.Items {
		def, row, err := modules.LoadCommandSet(sdb.Read(), it.SetID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && row.DeletedAt > 0) {
			problems = append(problems, modules.Problem{Where: []modules.Msg{modules.M("loc.item", "n", i+1)},
				Msg: modules.M("command.import_item_not_found", "set", it.SetID)})
			continue
		}
		if err != nil {
			c.internalError(w, r, err)
			return
		}
		spec := def.CommandSetSpec
		if name := strings.TrimSpace(it.Name); name != "" {
			spec.Name = name
		}
		// The definition was valid at the source; the check covers the
		// new name (a past once time is kept as it is).
		for _, p := range spec.Problems(0) {
			p.Where = append([]modules.Msg{modules.M("loc.item", "n", i+1)}, p.Where...)
			problems = append(problems, p)
		}
		if seen[spec.Name] {
			conflicts = append(conflicts, spec.Name)
		} else if taken, err := models.CommandSetNameTaken(tdb.Read(), spec.Name, 0); err != nil {
			c.internalError(w, r, err)
			return
		} else if taken {
			conflicts = append(conflicts, spec.Name)
		}
		seen[spec.Name] = true
		list = append(list, copied{from: srcName + " / " + row.Name, spec: spec})
	}
	if len(problems) > 0 {
		writeCommandProblems(w, problems)
		return
	}
	if len(conflicts) > 0 {
		slices.Sort(conflicts)
		conflicts = slices.Compact(conflicts)
		m := modules.M("command.import_conflicts", "names", conflicts)
		writeJSON(w, http.StatusConflict, map[string]any{"error": m.Text(), "error_i18n": m, "conflicts": conflicts})
		return
	}
	p := principalFrom(r)
	now := time.Now().Unix()
	// The unique name index decides inside the transaction; the answer is
	// written after it.
	ids := make([]int64, 0, len(list))
	var taken string
	err = tdb.Write(func(tx models.Writer) error {
		for _, cp := range list {
			row, items := modules.CommandSetRows(&cp.spec)
			row.CreatedBy, row.ImportedFrom = p.operator(), truncate(cp.from, 400)
			if err := models.InsertCommandSet(tx, row, now); err != nil {
				if errors.Is(err, models.ErrNameTaken) {
					taken = row.Name
				}
				return err
			}
			if err := models.ReplaceCommandItems(tx, row.ID, items); err != nil {
				return err
			}
			ids = append(ids, row.ID)
		}
		return nil
	})
	switch {
	case taken != "":
		m := modules.M("command.import_conflicts", "names", []string{taken})
		writeJSON(w, http.StatusConflict, map[string]any{"error": m.Text(), "error_i18n": m, "conflicts": []string{taken}})
		return
	case err != nil:
		c.internalError(w, r, err)
		return
	}
	out := make([]commandSetJSON, 0, len(ids))
	for i, id := range ids {
		c.history(p, models.HistCommand, "imported", c.agentDisplayName(a)+" / "+list[i].spec.Name, modules.M("history.detail.command_copied", "source", list[i].from))
		row, err := models.GetCommandSet(tdb.Read(), id)
		if err != nil {
			c.internalError(w, r, err)
			return
		}
		s, err := c.commandSetOne(r, a, tdb, row)
		if err != nil {
			c.internalError(w, r, err)
			return
		}
		out = append(out, *s)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sets": out})
}

func (c *core) handleRequestCommandRun(w http.ResponseWriter, r *http.Request) {
	a, tdb := c.loadCommandTarget(w, r)
	if a == nil {
		return
	}
	stored := c.loadCommandSetRow(w, r, tdb, false)
	if stored == nil {
		return
	}
	p := principalFrom(r)
	now := time.Now().Unix()
	q := &models.CommandRequest{AgentID: a.ID, SetID: stored.ID, RequestedAt: now, RequestedBy: p.operator(),
		ExpiresAt: now + int64(modules.CommandRequestExpiry/time.Second)}
	target := modules.Raw(c.agentDisplayName(a) + " / " + stored.Name)
	// The request is inserted as queued and recorded in the history, the
	// queue step gives it a worker when one is free and its agent is
	// responding, and the row is read again for the answer: all in one
	// transaction of the central database.
	err := c.queueWrite(now, func(tx models.Writer) error {
		if err := models.InsertCommandRequest(tx, q); err != nil {
			return err
		}
		return models.InsertHistory(tx, c.historyRow(p, models.HistCommand, "run_requested", target,
			modules.M("history.detail.command_request", "request", q.ID)))
	}, func(tx models.Writer) error {
		var err error
		q, err = models.GetCommandRequest(tx, a.ID, q.ID)
		return err
	})
	if errors.Is(err, models.ErrRequestActive) {
		writeError(w, http.StatusConflict, modules.M("command.request_pending"))
		return
	} else if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, commandRequestOut(q))
}

// errNotDiscardable ends the discard transaction of a request that was
// already handed over or ended.
var errNotDiscardable = errors.New("the run request cannot be discarded")

func (c *core) handleDiscardCommandRequest(w http.ResponseWriter, r *http.Request) {
	a, tdb := c.loadCommandTarget(w, r)
	if a == nil {
		return
	}
	id, ok := pathID(r, "req")
	if !ok {
		writeError(w, http.StatusNotFound, modules.M("command.request_not_found"))
		return
	}
	p := principalFrom(r)
	// The request is read, discarded and read again in one transaction,
	// whose queue step gives the worker of a pending one to the next
	// request at once.
	var q *models.CommandRequest
	missing, notDiscardable := false, false
	now := time.Now().Unix()
	err := c.queueWrite(now, func(tx models.Writer) error {
		if _, err := models.GetCommandRequest(tx, a.ID, id); err != nil {
			missing = errors.Is(err, sql.ErrNoRows)
			return err
		}
		ok, err := models.DiscardCommandRequest(tx, a.ID, id, p.operator(), now)
		if err != nil {
			return err
		}
		if !ok {
			notDiscardable = true
			return errNotDiscardable
		}
		return nil
	}, func(tx models.Writer) error {
		var err error
		q, err = models.GetCommandRequest(tx, a.ID, id)
		return err
	})
	switch {
	case missing:
		writeError(w, http.StatusNotFound, modules.M("command.request_not_found"))
		return
	case notDiscardable:
		writeError(w, http.StatusConflict, modules.M("command.request_not_discardable"))
		return
	case err != nil:
		c.internalError(w, r, err)
		return
	}
	c.historyMsg(p, models.HistCommand, "request_discarded", c.commandTarget(a, tdb, q.SetID), modules.M("history.detail.command_request", "request", q.ID))
	writeJSON(w, http.StatusOK, commandRequestOut(q))
}

// --- results (any logged-in account) ---

// latestOf builds the latest result entry of a set.
func latestOf(a *models.Agent, name string, row *models.CommandSet, latest map[int64]*models.CommandRun, running map[int64]bool, full bool) commandLatestJSON {
	out := commandLatestJSON{AgentID: a.ID, AgentName: name, SetID: row.ID, SetName: row.Name, Running: running[row.ID]}
	if run := latest[row.ID]; run != nil {
		out.LastRun = commandRunOut(run, full)
	}
	if full {
		purpose := row.Purpose
		out.Purpose = &purpose
	}
	return out
}

func (c *core) handleAgentCommandResults(w http.ResponseWriter, r *http.Request) {
	a, tdb := c.loadCommandTarget(w, r)
	if a == nil {
		return
	}
	rows, err := models.ListCommandSets(tdb.Read())
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	latest, err := models.ListLatestFinishedCommandRuns(tdb.Read())
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	running, err := models.RunningCommandRunSets(tdb.Read())
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	full := fullCommandView(r)
	name := c.agentDisplayName(a)
	results, sets := []commandLatestJSON{}, []commandLatestJSON{}
	for _, row := range rows {
		e := latestOf(a, name, row, latest, running, full)
		sets = append(sets, e)
		if row.Purpose == modules.PurposeCollectOutput {
			results = append(results, e)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent_id": a.ID, "agent_name": name, "results": results, "sets": sets})
}

func (c *core) handleCommandResultsByName(w http.ResponseWriter, r *http.Request) {
	want := r.URL.Query().Get("name")
	agents, err := models.ListAgents(c.db.Read())
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	full := fullCommandView(r)
	names := map[string]bool{}
	results := []commandLatestJSON{}
	for _, a := range agents {
		tdb, err := c.targetDB(a)
		if err != nil {
			c.internalError(w, r, err)
			return
		}
		rows, err := models.ListCommandSets(tdb.Read())
		if err != nil {
			c.internalError(w, r, err)
			return
		}
		var match *models.CommandSet
		for _, row := range rows {
			if row.Purpose != modules.PurposeCollectOutput {
				continue
			}
			names[row.Name] = true
			if want != "" && row.Name == want {
				match = row
			}
		}
		if match == nil {
			continue
		}
		latest := map[int64]*models.CommandRun{}
		if run, err := models.LatestFinishedCommandRun(tdb.Read(), match.ID); err == nil {
			latest[match.ID] = run
		} else if !errors.Is(err, sql.ErrNoRows) {
			c.internalError(w, r, err)
			return
		}
		running, err := models.RunningCommandRunSets(tdb.Read())
		if err != nil {
			c.internalError(w, r, err)
			return
		}
		results = append(results, latestOf(a, c.agentDisplayName(a), match, latest, running, full))
	}
	list := make([]string, 0, len(names))
	for n := range names {
		list = append(list, n)
	}
	slices.Sort(list)
	slices.SortStableFunc(results, func(x, y commandLatestJSON) int { return strings.Compare(x.AgentName, y.AgentName) })
	writeJSON(w, http.StatusOK, map[string]any{"names": list, "name": want, "results": results})
}

func (c *core) handleListCommandRuns(w http.ResponseWriter, r *http.Request) {
	a, tdb := c.loadCommandTarget(w, r)
	if a == nil {
		return
	}
	row := c.loadCommandSetRow(w, r, tdb, true)
	if row == nil {
		return
	}
	limit, err := queryInt(r, "limit", 50, 1, 500)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	before, err := queryInt(r, "before", 0, 0, math.MaxInt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	runs, err := models.ListCommandRuns(tdb.Read(), row.ID, limit+1, int64(before))
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	var next *int64
	if len(runs) > limit {
		runs = runs[:limit]
		id := runs[limit-1].ID
		next = &id
	}
	full := fullCommandView(r)
	out := make([]*commandRunJSON, 0, len(runs))
	for _, run := range runs {
		out = append(out, commandRunOut(run, full))
	}
	resp := map[string]any{"agent_id": a.ID, "agent_name": c.agentDisplayName(a), "set_id": row.ID, "set_name": row.Name,
		"deleted": row.DeletedAt > 0, "runs": out, "next_before": next}
	if full {
		resp["purpose"] = row.Purpose
	}
	writeJSON(w, http.StatusOK, resp)
}

func (c *core) handleGetCommandRun(w http.ResponseWriter, r *http.Request) {
	a, tdb := c.loadCommandTarget(w, r)
	if a == nil {
		return
	}
	id, ok := pathID(r, "run")
	if !ok {
		writeError(w, http.StatusNotFound, modules.M("command.run_not_found"))
		return
	}
	run, err := models.GetCommandRun(tdb.Read(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, modules.M("command.run_not_found"))
		return
	}
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	results, err := models.ListCommandResults(tdb.Read(), run.ID)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	full := fullCommandView(r)
	out := commandRunDetailJSON{commandRunJSON: *commandRunOut(run, full), AgentID: a.ID, AgentName: c.agentDisplayName(a),
		Commands: make([]commandResultJSON, 0, len(results))}
	if run.FinishedAt > 0 {
		out.NotExecuted = max(run.CommandsTotal-len(results), 0)
	}
	for _, res := range results {
		cr := commandResultJSON{Position: res.Position, Kind: res.Kind, Reason: res.Reason, StartedAt: ts(res.StartedAt),
			FinishedAt: ts(res.FinishedAt), Output: modules.DisplayText(res.Output), OutputBytes: res.OutputBytes,
			OutputTruncated: res.OutputTruncated}
		if res.ExitCode.Valid {
			code := res.ExitCode.Int64
			cr.ExitCode = &code
		}
		if full {
			line, note := res.CommandLine, res.Note
			cr.CommandLine, cr.Note = &line, &note
			if res.ValuesJSON != "" {
				var v modules.CommandValues
				if json.Unmarshal([]byte(res.ValuesJSON), &v) == nil {
					v.Values = nonNil(v.Values)
					cr.Values = &v
				}
			}
		}
		out.Commands = append(out.Commands, cr)
	}
	writeJSON(w, http.StatusOK, out)
}
