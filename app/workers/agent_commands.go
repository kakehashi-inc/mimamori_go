package workers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// Command sets on the central side of sync and report: definitions and
// manual run requests go to the agent with sync; runs, command results,
// request answers and the outcome of storing definitions come back with
// reports. Nothing about command results is mailed.

// commandSync adds the command set definitions to a sync response (only
// when the agent's signature differs) and keeps the signature the agent
// reported. It runs for the built-in agent too. The run requests are handed
// over by centralSync.
func (c *core) commandSync(agent *models.Agent, req *modules.SyncRequest, resp *modules.SyncResponse) error {
	if req.CommandSig != agent.CommandSig {
		if err := c.db.Write(func(tx models.Writer) error { return models.RecordAgentCommandSig(tx, agent.ID, req.CommandSig) }); err != nil {
			return err
		}
		agent.CommandSig = req.CommandSig
	}
	tdb, err := c.targetDB(agent)
	if err != nil {
		return err
	}
	sig, err := modules.TargetCommandSignature(tdb.Read())
	if err != nil {
		return err
	}
	if sig != req.CommandSig {
		defs, err := modules.LoadCommandDefinitions(tdb.Read())
		if err != nil {
			return err
		}
		resp.CommandSets = defs
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
// skipped and returned, so that the caller logs them after the transaction
// (the agent's name is read from the central database, which a transaction
// on the target database does not touch).
func storeCommandRuns(tx models.Writer, runs []modules.CommandRunReport, now int64) ([]ignoredRun, error) {
	var ignored []ignoredRun
	for i := range runs {
		rep := &runs[i]
		if rep.RunUID == "" || len(rep.RunUID) > 128 {
			continue
		}
		set, err := models.GetCommandSet(tx, rep.SetID)
		if errors.Is(err, sql.ErrNoRows) {
			ignored = append(ignored, ignoredRun{runUID: rep.RunUID, setID: rep.SetID})
			continue
		} else if err != nil {
			return nil, err
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
			return nil, err
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
				return nil, err
			}
		}
		if !rep.Finished {
			if err := models.TouchCommandRun(tx, run.ID, now); err != nil {
				return nil, err
			}
			continue
		}
		state := rep.State
		if !validResultKind(state) {
			kinds, err := models.CommandResultKinds(tx, run.ID)
			if err != nil {
				return nil, err
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
			return nil, err
		}
	}
	return ignored, nil
}

// ignoredRun is a run of a report whose command set the target database
// does not have.
type ignoredRun struct {
	runUID string
	setID  int64
}

// endedRequest is a run request the agent answered without running it.
type endedRequest struct {
	setID int64
	why   modules.Msg
}

// storeRequestAnswers records, inside a transaction of the central
// database, what a report of an agent says about its run requests: an ack
// "started" and every run with a request id end the request as done, an
// ack "expired" or "unknown_set" ends it as expired. Each change is
// conditional on the request's agent and state, so a request of another
// agent or one already ended is not changed. It returns the requests that
// ended without a run (for the history).
func storeRequestAnswers(tx models.Writer, agentID int64, req *modules.ReportRequest, now int64) ([]endedRequest, error) {
	var ended []endedRequest
	for _, a := range req.CommandRequestAcks {
		switch a.Status {
		case modules.RequestAckStarted:
			if _, err := models.MarkCommandRequestDone(tx, agentID, a.RequestID, a.RunUID, now); err != nil {
				return nil, err
			}
		case modules.RequestAckExpired, modules.RequestAckUnknownSet:
			ok, err := models.MarkCommandRequestExpired(tx, agentID, a.RequestID, now)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			why := modules.M("history.detail.request_ended_why.expired")
			if a.Status == modules.RequestAckUnknownSet {
				why = modules.M("history.detail.request_ended_why.unknown_set")
			}
			r, err := models.GetCommandRequest(tx, agentID, a.RequestID)
			if err != nil {
				return nil, err
			}
			ended = append(ended, endedRequest{setID: r.SetID, why: modules.M("history.detail.request_ended", "request", a.RequestID, "why", why)})
		}
	}
	for _, run := range req.CommandRuns {
		if run.RequestID <= 0 || run.RunUID == "" || len(run.RunUID) > 128 {
			continue
		}
		if _, err := models.MarkCommandRequestDone(tx, agentID, run.RequestID, run.RunUID, now); err != nil {
			return nil, err
		}
	}
	return ended, nil
}

// recordCommandApply keeps the outcome the agent reported for storing
// delivered definitions (shown with the sets it refused). It runs in the
// transaction of the report's central part; agent is the row read there.
func recordCommandApply(tx models.Writer, agent *models.Agent, ap *modules.CommandApplyResult) error {
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
		ap.Problems[i].ProblemsI18n = checkedProblemsI18n(ap.Problems[i].Problems, ap.Problems[i].ProblemsI18n)
	}
	if len(ap.Problems) > 1000 {
		ap.Problems = ap.Problems[:1000]
	}
	b, _ := json.Marshal(ap)
	if err := models.RecordAgentCommandApply(tx, agent.ID, string(b)); err != nil {
		return err
	}
	if ap.Signature != "" && ap.Signature != agent.CommandSig {
		agent.CommandSig = ap.Signature
		return models.RecordAgentCommandSig(tx, agent.ID, ap.Signature)
	}
	return nil
}

// checkedProblemsI18n returns the coded problems an agent reported for the
// English problems (in their order): a coded problem whose English rendering
// differs from the English one (an unknown code, a cut text) or whose JSON
// is too large is replaced by the English as data. Nil when the agent sent
// none or they do not line up (an agent of an older build sends none).
func checkedProblemsI18n(texts []string, ps []modules.Problem) []modules.Problem {
	if len(ps) == 0 || len(ps) < len(texts) {
		return nil
	}
	out := make([]modules.Problem, len(texts))
	for i, text := range texts {
		p := ps[i]
		if p.Text() != text || jsonSize(p) > maxApplyI18nBytes {
			p = modules.Problem{Msg: modules.Raw(text)}
		}
		out[i] = p
	}
	return out
}

// setProblems are the problems the agent reported for a set: the English
// texts and the same problems with their codes in the same order.
type setProblems struct {
	texts []string
	coded []modules.Problem
}

// agentCommandProblems returns the problems the agent reported for the
// definitions it holds, by set ID (only when they are the current ones). A
// problem reported without its code (an agent of an older build) is coded
// as its English text (data).
func agentCommandProblems(agent *models.Agent) map[int64]*setProblems {
	out := map[int64]*setProblems{}
	if agent.CommandProblems == "" {
		return out
	}
	var ap modules.CommandApplyResult
	if json.Unmarshal([]byte(agent.CommandProblems), &ap) != nil || ap.Signature != agent.CommandSig {
		return out
	}
	for _, p := range ap.Problems {
		sp := out[p.SetID]
		if sp == nil {
			sp = &setProblems{}
			out[p.SetID] = sp
		}
		coded := checkedProblemsI18n(p.Problems, p.ProblemsI18n)
		for i, text := range p.Problems {
			sp.texts = append(sp.texts, text)
			if coded != nil {
				sp.coded = append(sp.coded, coded[i])
			} else {
				sp.coded = append(sp.coded, modules.Problem{Msg: modules.Raw(text)})
			}
		}
	}
	return out
}

// expireCommandRequests ends the run requests of all agents that were not
// handed over within their expiry and records them in the history (the
// set's name is read from the agent's database after the transaction).
func (c *core) expireCommandRequests(now time.Time) {
	var list []*models.CommandRequest
	err := c.db.Write(func(tx models.Writer) error {
		var err error
		list, err = models.ExpireCommandRequests(tx, now.Unix())
		return err
	})
	if err != nil {
		c.logger.Printf("command requests: %v", err)
		return
	}
	for _, r := range list {
		a, err := models.GetAgent(c.db.Read(), r.AgentID)
		if err != nil {
			continue
		}
		tdb, err := c.targetDB(a)
		if err != nil {
			continue
		}
		c.historyMsg(nil, models.HistCommand, "request_expired", c.commandTarget(a, tdb, r.SetID),
			modules.M("history.detail.request_not_delivered", "request", r.ID, "expiry", modules.CommandRequestExpiry.String()))
	}
}

// commandRetention removes, inside a transaction of a target database, the
// runs older than cut (also the last run of a set) and the finished runs of
// every set beyond its keep count. It returns the number of runs removed
// and every part that failed (each named); a failed part does not stop the
// others.
func commandRetention(tx models.Writer, cut int64) (int64, error) {
	var errs []string
	removed, err := models.DeleteCommandRunsBefore(tx, cut)
	if err != nil {
		errs = append(errs, "runs older than the retention period: "+err.Error())
	}
	sets, err := models.ListCommandSets(tx)
	if err != nil {
		errs = append(errs, "list of command sets: "+err.Error())
	}
	for _, s := range sets {
		n, err := models.DeleteCommandRunsBeyondKeep(tx, s.ID, s.KeepRuns)
		if err != nil {
			errs = append(errs, "runs of set "+strconv.FormatInt(s.ID, 10)+" beyond its keep count: "+err.Error())
			continue
		}
		removed += n
	}
	if len(errs) > 0 {
		return removed, errors.New(strings.Join(errs, "; "))
	}
	return removed, nil
}

// commandTarget names a set for the history: "<agent> / <set name>", or
// "<agent> / set <id>" when the set no longer exists.
func (c *core) commandTarget(a *models.Agent, tdb *modules.DB, setID int64) modules.Msg {
	if s, err := models.GetCommandSet(tdb.Read(), setID); err == nil {
		return modules.Raw(c.agentDisplayName(a) + " / " + s.Name)
	}
	return modules.M("history.target.command_set", "agent", c.agentDisplayName(a),
		"set", modules.M("history.target.command_set_fallback", "set", setID))
}
