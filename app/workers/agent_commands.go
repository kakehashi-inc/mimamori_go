package workers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// Command sets on the central side of sync and report: definitions and
// manual run requests go to the agent with sync; runs, command results,
// request answers and the outcome of storing definitions come back with
// reports. Nothing about command results is mailed.

// commandSync adds the command set definitions (only when the agent's
// signature differs) and the waiting run requests to a sync response, and
// keeps the signature the agent reported. It runs for the built-in agent
// too.
func (c *core) commandSync(agent *models.Agent, req *modules.SyncRequest, resp *modules.SyncResponse, now int64) error {
	if req.CommandSig != agent.CommandSig {
		if err := models.RecordAgentCommandSig(c.db, agent.ID, req.CommandSig); err != nil {
			return err
		}
		agent.CommandSig = req.CommandSig
	}
	tdb, err := c.targetDB(agent)
	if err != nil {
		return err
	}
	sig, err := modules.TargetCommandSignature(tdb)
	if err != nil {
		return err
	}
	if sig != req.CommandSig {
		defs, err := modules.LoadCommandDefinitions(tdb)
		if err != nil {
			return err
		}
		resp.CommandSets = defs
	}
	reqs, err := models.ListActiveCommandRequests(tdb)
	if err != nil {
		return err
	}
	for _, r := range reqs {
		// A pending request past its expiry is ended by the periodic task;
		// a delivered one is delivered again until the agent answers it
		// (the agent runs a request at most once and answers it again).
		if r.State == models.RequestPending {
			if r.ExpiresAt <= now {
				continue
			}
			if err := models.MarkCommandRequestDelivered(tdb, r.ID, now); errors.Is(err, sql.ErrNoRows) {
				// Discarded or expired after it was listed: not delivered.
				continue
			} else if err != nil {
				return err
			}
		}
		resp.CommandRequests = append(resp.CommandRequests, modules.CommandRunRequest{
			ID: r.ID, SetID: r.SetID, RequestedAt: r.RequestedAt, ExpiresAt: r.ExpiresAt,
		})
	}
	return nil
}

// validResultKind reports whether k is one of the three result kinds.
func validResultKind(k string) bool {
	return k == modules.ResultSuccess || k == modules.ResultFailure || k == modules.ResultError
}

// storeCommandRuns stores the runs and command results of a report inside
// the report's transaction of the target database. Every report of a run
// repeats its start values, so the run is created from whichever arrives
// first; results are stored once per (run, position) and a run is closed
// once. Runs of sets the database does not have (a recreated database) are
// skipped.
func (c *core) storeCommandRuns(tx *sql.Tx, agent *models.Agent, runs []modules.CommandRunReport, now int64) error {
	for i := range runs {
		rep := &runs[i]
		if rep.RunUID == "" || len(rep.RunUID) > 128 {
			continue
		}
		set, err := models.GetCommandSet(tx, rep.SetID)
		if errors.Is(err, sql.ErrNoRows) {
			c.logger.Printf("%s: run %s of unknown command set %d ignored", c.agentDisplayName(agent), rep.RunUID, rep.SetID)
			continue
		} else if err != nil {
			return err
		}
		row := modules.CommandRunRow(rep)
		// A run the agent found interrupted at its start may come without
		// the definition's values.
		if row.SetName == "" {
			row.SetName = set.Name
		}
		if row.Purpose == "" {
			row.Purpose, row.Joiner = set.Purpose, set.Joiner
		}
		if row.Trigger != modules.TriggerManual && row.Trigger != modules.TriggerSchedule {
			row.Trigger = modules.TriggerManual
		}
		run, _, err := models.EnsureCommandRun(tx, row, now)
		if err != nil {
			return err
		}
		if run.RequestID > 0 {
			if err := models.MarkCommandRequestDone(tx, run.RequestID, run.RunUID, now); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		for j := range rep.Results {
			res := &rep.Results[j]
			if res.Position < 0 || (run.CommandsTotal > 0 && res.Position >= run.CommandsTotal) {
				continue
			}
			if !validResultKind(res.Kind) {
				res.Kind, res.Reason = modules.ResultError, truncate(res.Reason, 64)
			}
			if _, err := models.InsertCommandResult(tx, modules.CommandResultRow(run.ID, res)); err != nil {
				return err
			}
		}
		if !rep.Finished {
			if err := models.TouchCommandRun(tx, run.ID, now); err != nil {
				return err
			}
			continue
		}
		state := rep.State
		if !validResultKind(state) {
			kinds, err := models.CommandResultKinds(tx, run.ID)
			if err != nil {
				return err
			}
			if state = modules.RunState(kinds); state == "" {
				state = modules.ResultError
			}
		}
		finishedAt := rep.FinishedAt
		if finishedAt <= 0 {
			finishedAt = now
		}
		display, truncated := "", false
		if rep.Display != nil && run.Purpose == modules.PurposeCollectOutput {
			b, _ := json.Marshal(rep.Display)
			display, truncated = string(b), rep.Display.Truncated
		}
		err = models.FinishCommandRun(tx, run.ID, state, truncate(rep.Reason, 64), finishedAt, display, truncated, now)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return nil
}

// endedRequest is a run request the agent answered without running it.
type endedRequest struct {
	setID int64
	why   string
}

// storeCommandAcks records the agent's answers to delivered run requests.
// It returns the requests that ended without a run (for the history).
func storeCommandAcks(tx *sql.Tx, acks []modules.CommandRequestAck, now int64) ([]endedRequest, error) {
	var ended []endedRequest
	for _, a := range acks {
		var err error
		switch a.Status {
		case modules.RequestAckStarted:
			err = models.MarkCommandRequestDone(tx, a.RequestID, a.RunUID, now)
		case modules.RequestAckExpired, modules.RequestAckUnknownSet:
			err = models.MarkCommandRequestExpired(tx, a.RequestID, now)
			if err == nil {
				why := "it reached the agent after its expiry"
				if a.Status == modules.RequestAckUnknownSet {
					why = "the agent does not hold this command set"
				}
				if r, gerr := models.GetCommandRequest(tx, a.RequestID); gerr == nil {
					ended = append(ended, endedRequest{setID: r.SetID, why: fmt.Sprintf("request %d ended without a run: %s", a.RequestID, why)})
				}
			}
		default:
			continue
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return ended, nil
}

// recordCommandApply keeps the outcome the agent reported for storing
// delivered definitions (shown with the sets it refused).
func (c *core) recordCommandApply(agent *models.Agent, ap *modules.CommandApplyResult) error {
	if ap.Problems == nil {
		ap.Problems = []modules.CommandApplyProblem{}
	}
	for i := range ap.Problems {
		ap.Problems[i].SetName = truncate(ap.Problems[i].SetName, 400)
		if len(ap.Problems[i].Problems) > 100 {
			ap.Problems[i].Problems = ap.Problems[i].Problems[:100]
		}
		for j, p := range ap.Problems[i].Problems {
			ap.Problems[i].Problems[j] = truncate(p, 2000)
		}
	}
	if len(ap.Problems) > 1000 {
		ap.Problems = ap.Problems[:1000]
	}
	b, _ := json.Marshal(ap)
	if err := models.RecordAgentCommandApply(c.db, agent.ID, string(b)); err != nil {
		return err
	}
	if ap.Signature != "" && ap.Signature != agent.CommandSig {
		agent.CommandSig = ap.Signature
		return models.RecordAgentCommandSig(c.db, agent.ID, ap.Signature)
	}
	return nil
}

// agentCommandProblems returns the problems the agent reported for the
// definitions it holds, by set ID (only when they are the current ones).
func agentCommandProblems(agent *models.Agent) map[int64][]string {
	out := map[int64][]string{}
	if agent.CommandProblems == "" {
		return out
	}
	var ap modules.CommandApplyResult
	if json.Unmarshal([]byte(agent.CommandProblems), &ap) != nil || ap.Signature != agent.CommandSig {
		return out
	}
	for _, p := range ap.Problems {
		out[p.SetID] = append(out[p.SetID], p.Problems...)
	}
	return out
}

// expireCommandRequests ends the run requests that were not delivered
// within their expiry and records them in the history.
func (c *core) expireCommandRequests(now time.Time) {
	agents, err := models.ListAgents(c.db)
	if err != nil {
		return
	}
	for _, a := range agents {
		tdb, err := c.targetDB(a)
		if err != nil {
			continue
		}
		list, err := models.ExpireCommandRequests(tdb, now.Unix())
		if err != nil {
			c.logger.Printf("command requests of %s: %v", c.agentDisplayName(a), err)
			continue
		}
		for _, r := range list {
			c.history(nil, models.HistCommand, "request_expired", c.commandTarget(a, tdb, r.SetID),
				fmt.Sprintf("request %d was not delivered to the agent within %s", r.ID, modules.CommandRequestExpiry))
		}
	}
}

// commandRetention removes the runs of a target older than cut (also the
// last run of a set), the finished runs of every set beyond its keep count,
// and the ended run requests older than cut.
func (c *core) commandRetention(a *models.Agent, tdb *sql.DB, cut int64) {
	removed, err := models.DeleteCommandRunsBefore(tdb, cut)
	if err != nil {
		c.logger.Printf("retention of command runs of %s: %v", c.agentDisplayName(a), err)
		return
	}
	sets, err := models.ListCommandSets(tdb)
	if err != nil {
		return
	}
	for _, s := range sets {
		n, err := models.DeleteCommandRunsBeyondKeep(tdb, s.ID, s.KeepRuns)
		if err != nil {
			c.logger.Printf("retention of command runs of %s: %v", c.agentDisplayName(a), err)
			return
		}
		removed += n
	}
	if removed > 0 {
		c.logger.Printf("retention: removed %d command run(s) of %s", removed, c.agentDisplayName(a))
	}
	_, _ = models.DeleteEndedCommandRequestsBefore(tdb, cut)
}

// commandTarget names a set for the history: "<agent> / <set name>".
func (c *core) commandTarget(a *models.Agent, tdb *sql.DB, setID int64) string {
	name := fmt.Sprintf("set %d", setID)
	if s, err := models.GetCommandSet(tdb, setID); err == nil {
		name = s.Name
	}
	return c.agentDisplayName(a) + " / " + name
}
