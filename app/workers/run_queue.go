package workers

import (
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// The queue of run requests. The central's item workers is the largest
// number of run requests in progress at agents at the same time, all agents
// together (the built-in agent is one of them). A request is in progress
// from the moment it is given a worker (pending) until the central learns
// that its run ended (delivered and running in between). Every decision is
// taken by queueStep inside one write transaction of central.db, which runs
// alone on that file, so no two flows can give the same worker.

// releasedRequest is a granted request that went back to the queue because
// its agent is not responding (logged after the transaction).
type releasedRequest struct {
	id       int64
	agent    string
	lastSync int64
}

// queueStep puts back the granted requests of agents that are not
// responding and gives free workers to the oldest queued requests of agents
// that are responding. It runs inside a write transaction of central.db,
// reads and writes only through tx and waits for nothing. It returns the
// requests it put back, for the log.
func (c *core) queueStep(tx models.Writer, now int64) ([]releasedRequest, error) {
	list, err := models.ListOpenCommandRequests(tx)
	if err != nil {
		return nil, err
	}
	cfg := c.conf()
	ags, err := models.ListAgentsWithOpenCommandRequests(tx)
	if err != nil {
		return nil, err
	}
	// Whether an agent responds is judged once per agent and call; a rule
	// version is read once per call.
	responding := map[int64]bool{}
	agents := map[int64]*models.Agent{}
	intervals := map[[2]int64]int{}
	for _, a := range ags {
		agents[a.ID] = a
		key := [2]int64{a.ReportedRuleID, int64(a.ReportedRuleVersion)}
		iv, ok := intervals[key]
		if !ok {
			iv = agentInterval(tx, a, cfg.DefaultCheckIntervalSec)
			intervals[key] = iv
		}
		responding[a.ID] = syncedWithin(a.LastSyncAt, iv, now)
	}
	var released []releasedRequest
	var granted []*models.CommandRequest
	busy := 0
	for _, r := range list {
		switch r.State {
		case models.RequestPending:
			if !responding[r.AgentID] {
				ok, err := models.ReleaseCommandRequest(tx, r.ID)
				if err != nil {
					return nil, err
				}
				if ok {
					r.State = models.RequestQueued
					rel := releasedRequest{id: r.ID, agent: "agent-" + itoa64(r.AgentID)}
					if a := agents[r.AgentID]; a != nil {
						rel.agent, rel.lastSync = agentName(tx, a), a.LastSyncAt
					}
					released = append(released, rel)
				}
				continue
			}
			busy++
		case models.RequestDelivered, models.RequestRunning:
			if responding[r.AgentID] {
				busy++
			}
		}
	}
	for _, r := range list {
		if busy >= cfg.Workers {
			break
		}
		if r.State != models.RequestQueued || !responding[r.AgentID] || r.ExpiresAt <= now {
			continue
		}
		ok, err := models.GrantCommandRequest(tx, r.ID)
		if err != nil {
			return nil, err
		}
		if ok {
			busy++
			r.State = models.RequestPending
			granted = append(granted, r)
		}
	}
	if c.queueChecked != nil {
		c.queueChecked(tx, now, granted)
	}
	return released, nil
}

// queueWrite runs before, then queueStep, then after in one write
// transaction of central.db (before and after may be nil), and logs
// afterwards the requests the step put back.
func (c *core) queueWrite(now int64, before, after func(tx models.Writer) error) error {
	var released []releasedRequest
	err := c.db.Write(func(tx models.Writer) error {
		if before != nil {
			if err := before(tx); err != nil {
				return err
			}
		}
		var err error
		if released, err = c.queueStep(tx, now); err != nil {
			return err
		}
		if after != nil {
			return after(tx)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, r := range released {
		since := "it was created"
		if r.lastSync > 0 {
			since = time.Unix(r.lastSync, 0).UTC().Format(time.RFC3339)
		}
		c.logger.Printf("run request %d of %s waits again: the agent has not synced since %s", r.id, r.agent, since)
	}
	return nil
}

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

// runRequestsStep is the periodic step "run requests": the queued and
// pending requests whose expiry passed end as expired (recorded in the
// history after the transaction, with the set's name read from the agent's
// database), and the queue step runs (an agent that stopped syncing gives
// its granted requests back; a raised worker count saved by central config
// takes effect).
func (c *core) runRequestsStep(now time.Time) {
	var list []*models.CommandRequest
	err := c.queueWrite(now.Unix(), func(tx models.Writer) error {
		var err error
		list, err = models.ExpireCommandRequests(tx, now.Unix())
		return err
	}, nil)
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

// queueAfterConfig runs the queue step once after the configuration was
// changed on the settings screen, so a raised worker count takes effect at
// once.
func (c *core) queueAfterConfig() {
	if err := c.queueWrite(time.Now().Unix(), nil, nil); err != nil {
		c.logger.Printf("run requests after the configuration change: %v", err)
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
			_, err = models.MarkCommandRequestRunning(tx, agentID, t.requestID, t.runUID)
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
