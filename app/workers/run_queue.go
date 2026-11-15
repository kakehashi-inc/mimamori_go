package workers

import (
	"math"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// The run requests of the agents. A request waits queued in the central
// database until a sync of its agent that says the agent can take one
// (free_worker) hands it over: in the transaction that records the sync, the
// oldest queued request of that agent becomes delivered (one per sync), and
// the answer carries every delivered request of the agent. Requests of other
// agents play no role. Reports then move a request on (running, done,
// expired).

// An agent that is not responding (it said that it stops, or it has not
// synced within three of its check intervals and ten minutes, or never)
// holds no request handed over to it: in the transaction of its stopping
// report, and in the periodic step for the others, its delivered requests go
// back to queued. They are handed over again when the agent syncs (the agent
// answers one it already ran with that run), and can be discarded and
// expire meanwhile. Its running requests stay running.

// responding reports whether the agent is responding: it did not say that
// it stops since its last sync, and agentResponding holds.
func (c *core) responding(a *models.Agent, now int64) bool {
	return !c.isStopping(a.ID) && agentResponding(a, now)
}

// agentResponding reports whether the agent synced within three of its
// check intervals and ten minutes (the judgement the central uses for "this
// agent has not connected for a while"). The interval is the one the answer
// to its last sync gave it (agents.check_interval_sec, stored with the
// sync): the agent waits that interval until it syncs again, whatever
// changed at the central meanwhile. An agent that never synced is not
// responding.
func agentResponding(a *models.Agent, now int64) bool {
	return a.LastSyncAt > 0 && now-a.LastSyncAt <= 3*int64(a.CheckIntervalSec)+600
}

// markStopping remembers that the agent said it stops (a report with
// stopping true) until its next sync (clearStopping), in memory only: after
// a restart of the central the time rule judges the agent. It is called
// inside the write transaction of central.db that stores the report and
// clearStopping inside that of a sync, so the marks follow the order of
// those transactions. It returns the mark, for undoStopping.
func (c *core) markStopping(agentID int64) uint64 {
	c.stoppingMu.Lock()
	defer c.stoppingMu.Unlock()
	if c.stopping == nil {
		c.stopping = map[int64]uint64{}
	}
	c.stoppingSeq++
	c.stopping[agentID] = c.stoppingSeq
	return c.stoppingSeq
}

// undoStopping removes the mark when it is still the one markStopping
// returned (the report's transaction was not committed).
func (c *core) undoStopping(agentID int64, mark uint64) {
	c.stoppingMu.Lock()
	defer c.stoppingMu.Unlock()
	if c.stopping[agentID] == mark {
		delete(c.stopping, agentID)
	}
}

// clearStopping forgets that the agent said it stops (it synced).
func (c *core) clearStopping(agentID int64) {
	c.stoppingMu.Lock()
	defer c.stoppingMu.Unlock()
	delete(c.stopping, agentID)
}

// isStopping reports whether the agent said it stops and has not synced
// since.
func (c *core) isStopping(agentID int64) bool {
	c.stoppingMu.Lock()
	defer c.stoppingMu.Unlock()
	_, ok := c.stopping[agentID]
	return ok
}

// requeuedRequest is a delivered request that went back to queued because
// its agent is not responding (logged after the transaction).
type requeuedRequest struct {
	id    int64
	agent string
	why   string
}

// requeueRequests puts the delivered requests of agent back to queued inside
// the write transaction tx and returns them for the log; why says why the
// agent is not responding.
func requeueRequests(tx models.Writer, a *models.Agent, why string) ([]requeuedRequest, error) {
	ids, err := models.RequeueDeliveredCommandRequests(tx, a.ID)
	if err != nil {
		return nil, err
	}
	name := agentName(tx, a)
	out := make([]requeuedRequest, 0, len(ids))
	for _, id := range ids {
		out = append(out, requeuedRequest{id: id, agent: name, why: why})
	}
	return out, nil
}

// logRequeued writes the log line of each request that waits again.
func (c *core) logRequeued(list []requeuedRequest) {
	for _, r := range list {
		c.logger.Printf("run request %d of %s waits again: %s", r.id, r.agent, r.why)
	}
}

// silentWhy is the reason of the log line for an agent that is not
// responding by the time rule or because it said it stops.
func (c *core) silentWhy(a *models.Agent) string {
	switch {
	case c.isStopping(a.ID):
		return "the agent is stopping"
	case a.LastSyncAt == 0:
		return "the agent has not synced since it was created"
	}
	return "the agent has not synced since " + time.Unix(a.LastSyncAt, 0).UTC().Format(time.RFC3339)
}

// runRequestsStep is the periodic step "run requests", one transaction:
// the delivered requests of every agent that is not responding go back to
// queued (logged after the transaction), then the queued requests whose
// expiry passed end as expired (recorded in the history after the
// transaction, with the set's name read from the agent's database).
func (c *core) runRequestsStep(now time.Time) {
	var list []*models.CommandRequest
	var requeued []requeuedRequest
	err := c.db.Write(func(tx models.Writer) error {
		// The agents with a delivered request, whenever it was requested.
		agents, err := models.ListAgentsWithCommandRequests(tx, math.MaxInt64, models.RequestDelivered)
		if err != nil {
			return err
		}
		for _, a := range agents {
			if c.responding(a, now.Unix()) {
				continue
			}
			rs, err := requeueRequests(tx, a, c.silentWhy(a))
			if err != nil {
				return err
			}
			requeued = append(requeued, rs...)
		}
		list, err = models.ExpireCommandRequests(tx, now.Unix())
		return err
	})
	if err != nil {
		c.logger.Printf("run requests: %v", err)
		return
	}
	c.logRequeued(requeued)
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
