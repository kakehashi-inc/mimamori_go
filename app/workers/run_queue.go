package workers

import (
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// The run requests of the agents. A request waits queued in the central
// database until a sync of its agent that says the agent can take one
// (free_worker) hands it over: in the transaction that records the sync, the
// oldest queued request of that agent becomes delivered (one per sync), and
// the answer carries every delivered request of the agent. Requests of other
// agents play no role; nothing counts requests over all agents. Reports then
// move a request on (running, done, expired).

// agentResponding reports whether the agent synced within three of its
// check intervals and ten minutes (the judgement the central uses for "this
// agent has not connected for a while"). An agent that never synced is not
// responding.
func agentResponding(r models.Reader, a *models.Agent, defaultInterval int, now int64) bool {
	return syncedWithin(a.LastSyncAt, agentInterval(r, a, defaultInterval), now)
}

// syncedWithin reports whether a sync at lastSync (0: never) lies within
// three check intervals and ten minutes of now.
func syncedWithin(lastSync int64, interval int, now int64) bool {
	return lastSync > 0 && now-lastSync <= 3*int64(interval)+600
}

// agentInterval returns the check interval the agent runs with: that of the
// rule version it reports it runs, else defaultInterval (what an agent uses
// without a rule). A rule version that cannot be read gives the default.
func agentInterval(r models.Reader, a *models.Agent, defaultInterval int) int {
	if a.ReportedRuleID > 0 {
		if rv, err := models.GetRuleVersion(r, a.ReportedRuleID, a.ReportedRuleVersion); err == nil {
			if content, err := modules.ParseRuleContent([]byte(rv.Content)); err == nil {
				return content.CheckInterval()
			}
		}
	}
	return defaultInterval
}

// runRequestsStep is the periodic step "run requests": the queued requests
// whose expiry passed end as expired (recorded in the history after the
// transaction, with the set's name read from the agent's database).
func (c *core) runRequestsStep(now time.Time) {
	var list []*models.CommandRequest
	err := c.db.Write(func(tx models.Writer) error {
		var err error
		list, err = models.ExpireCommandRequests(tx, now.Unix())
		return err
	})
	if err != nil {
		c.logger.Printf("run requests: %v", err)
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

// reqTransition is one change a report asks of a run request.
type reqTransition struct {
	requestID int64
	to        string // RequestRunning, RequestDone or RequestExpired
	runUID    string
	why       string // for RequestExpired: the ack status
}

// requestTransitions returns what a report says about run requests, in
// order: the acks first (started: running; expired, unknown_set: expired),
// then every run with a request id (not finished: running; finished: done).
// A scheduled run (request id 0) changes no request.
func requestTransitions(req *modules.ReportRequest) []reqTransition {
	var out []reqTransition
	for _, a := range req.CommandRequestAcks {
		if a.RequestID <= 0 {
			continue
		}
		switch a.Status {
		case modules.RequestAckStarted:
			out = append(out, reqTransition{requestID: a.RequestID, to: models.RequestRunning, runUID: a.RunUID})
		case modules.RequestAckExpired, modules.RequestAckUnknownSet:
			out = append(out, reqTransition{requestID: a.RequestID, to: models.RequestExpired, why: a.Status})
		}
	}
	for _, run := range req.CommandRuns {
		if run.RequestID <= 0 || run.RunUID == "" || len(run.RunUID) > 128 {
			continue
		}
		to := models.RequestRunning
		if run.Finished {
			to = models.RequestDone
		}
		out = append(out, reqTransition{requestID: run.RequestID, to: to, runUID: run.RunUID})
	}
	return out
}

// endedRequest is a run request the agent answered without running it.
type endedRequest struct {
	setID int64
	why   modules.Msg
}

// applyRequestTransitions applies the transitions of a report of an agent
// inside a transaction of central.db. Each change is conditional on the
// request's agent and state, so a request of another agent or one already
// further changes nothing, and applying the same transitions again changes
// nothing. It returns the requests that ended as expired (for the history).
func applyRequestTransitions(tx models.Writer, agentID int64, ts []reqTransition, now int64) ([]endedRequest, error) {
	var ended []endedRequest
	for _, t := range ts {
		var err error
		switch t.to {
		case models.RequestRunning:
			_, err = models.MarkCommandRequestRunning(tx, agentID, t.requestID, t.runUID, now)
		case models.RequestDone:
			_, err = models.MarkCommandRequestDone(tx, agentID, t.requestID, t.runUID, now)
		case models.RequestExpired:
			var ok bool
			ok, err = models.MarkCommandRequestExpired(tx, agentID, t.requestID, now)
			if err != nil || !ok {
				break
			}
			why := modules.M("history.detail.request_ended_why.expired")
			if t.why == modules.RequestAckUnknownSet {
				why = modules.M("history.detail.request_ended_why.unknown_set")
			}
			var r *models.CommandRequest
			if r, err = models.GetCommandRequest(tx, agentID, t.requestID); err == nil {
				ended = append(ended, endedRequest{setID: r.SetID, why: modules.M("history.detail.request_ended", "request", t.requestID, "why", why)})
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return ended, nil
}
