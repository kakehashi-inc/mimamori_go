package modules

import (
	"errors"
	"io"
	"net"
	"strconv"
	"time"

	"mimamori/app/models"
)

// The communication flow of the agent: every check interval one round, a
// sync (POST /agent/v1/sync) followed by one report (POST /agent/v1/report)
// that carries everything the outbox holds. It is not an entry of the queue
// and no worker count limits it: it goes on while every worker waits for a
// command. What a sync brings is handed on: the rule assignment to the
// monitoring step and the check interval the answer gives to the rounds and
// the monitoring entry (syncLink; the interval is stored too), the command
// set definitions to the queue (stored first), the run requests to the
// queue.

// roundGap is the least time between the end of a round and the start of
// the next one.
const roundGap = time.Second

// requestCutter is a transport whose requests the agent's stop cuts: when
// the channel given to cutRequests is closed, a sync or report in progress
// ends at once with errRequestCut and none starts afterwards, except the
// last report, which says that the agent stops (RemoteTransport; the
// built-in agent hands its requests over in process and waits for nothing
// outside).
type requestCutter interface {
	cutRequests(stop <-chan struct{})
}

// commState is what only the communication flow uses: the transport and the
// agent's configuration.
type commState struct {
	transport AgentTransport
	config    *AgentConfig // nil for the built-in agent
	// onConfigSaved is called with the configuration after a change from the
	// central (renewed token, name, expiry, CAs) was saved, so that the
	// transport uses the saved token and CAs from the next request on.
	onConfigSaved func(cfg *AgentConfig)
}

// useConfig gives the communication flow the agent's configuration and the
// function called after a change of it was saved (a remote agent; the
// built-in agent has none). It is called before Run.
func (rt *AgentRuntime) useConfig(cfg *AgentConfig, onSaved func(cfg *AgentConfig)) {
	rt.comm.config, rt.comm.onConfigSaved = cfg, onSaved
	rt.link.setCAs(cfg.CAPEMs)
}

// roundsHook, when not nil, is called at the top of every turn of the
// communication flow's loop (tests use it to make the loop panic).
var roundsHook func()

// communicate is the communication flow. The first round comes at once;
// each further one a check interval after the start of the previous one, at
// least roundGap after its end. When a manually requested run ended
// (AgentRuntime.early), the next round comes as soon as roundGap after the
// end of the previous one allows. After the stop no round starts; when
// workersDone is closed (every worker returned), the last report is sent
// with stopping true. A panic of a round is logged; the next round comes at
// its time. A panic in the loop itself is logged too; the flow waits one
// second and goes on.
func (rt *AgentRuntime) communicate(workersDone <-chan struct{}) {
	var lastEnd time.Time
	next := time.Now()
	for {
		more := false
		if rt.recovered("communication panic", func() { more = rt.communicateOnce(&next, &lastEnd) }) {
			if !rt.pause(time.Second) {
				break
			}
			continue
		}
		if !more {
			break
		}
	}
	<-workersDone
	rt.recovered("communication panic", rt.flushStopping)
}

// communicateOnce waits for the time of the next round and does it; it
// reports false at the stop.
func (rt *AgentRuntime) communicateOnce(next *time.Time, lastEnd *time.Time) bool {
	if roundsHook != nil {
		roundsHook()
	}
	if !rt.waitRound(next, *lastEnd) {
		return false
	}
	start := time.Now()
	rt.recovered("communication panic", rt.round)
	*lastEnd = time.Now()
	if rt.roundPace != nil {
		*next = lastEnd.Add(rt.roundPace())
		return true
	}
	*next = start.Add(rt.link.interval())
	if gap := lastEnd.Add(roundGap); gap.After(*next) {
		*next = gap
	}
	return true
}

// waitRound waits until the time of the next round (moved forward by a
// request for an early round) and reports true then, or false at the stop.
func (rt *AgentRuntime) waitRound(next *time.Time, lastEnd time.Time) bool {
	for {
		if isClosed(rt.stop) {
			return false
		}
		d := time.Until(*next)
		if d <= 0 {
			return true
		}
		t := time.NewTimer(d)
		select {
		case <-rt.stop:
			t.Stop()
			return false
		case <-rt.early:
			gap := roundGap
			if rt.roundPace != nil {
				gap = rt.roundPace()
			}
			*next = lastEnd.Add(gap)
		case <-t.C:
		}
		t.Stop()
	}
}

// Round performs one round, a sync and then the report of what is queued,
// also after the agent stopped (tests send what is left after Run with it;
// a transport whose requests the stop cut, requestCutter, sends nothing
// after the stop, so they give a new runtime a new transport for that).
func (rt *AgentRuntime) Round() {
	rt.sync()
	rt.flushOutbox()
}

// round is one round: a sync, then the report of what is queued. After the
// stop the report of the round is not sent (the last report carries what is
// queued).
func (rt *AgentRuntime) round() {
	rt.sync()
	if isClosed(rt.stop) {
		return
	}
	rt.flushOutbox()
}

// sync sends the sync request and hands on what the answer brings. The
// definitions are stored before the run requests of the same answer are
// taken; when they cannot be stored, the requests are not taken either (the
// central delivers both again).
func (rt *AgentRuntime) sync() {
	running, err := rt.runningRequests()
	if err != nil {
		// Without the list the central could take a running request for
		// ended; the sync is left out and the round goes on as after a
		// failed sync.
		rt.logf("sync: reading the running command set runs failed: %v", err)
		return
	}
	ruleID, ruleVersion, lastApply := rt.link.applied()
	req := &SyncRequest{AgentVersion: AppVersion, RuleID: ruleID, RuleVersion: ruleVersion, LastApply: lastApply,
		CommandSig: rt.sched.signature(), RunningRequests: running, FreeWorker: rt.sched.freeWorker()}
	if rt.comm.config != nil {
		req.CAFingerprints = CAFingerprintsOf(rt.comm.config.CAPEMs)
	}
	resp, err := rt.comm.transport.Sync(req)
	if err != nil {
		rt.logConnectionError("sync", err)
		return
	}
	if rt.comm.config != nil {
		rt.saveConfigUpdates(resp)
	}
	// A running set keeps the definition it started with.
	stored := true
	if resp.CommandSets != nil {
		stored = rt.storeCommandDefinitions(resp.CommandSets)
	}
	if stored {
		rt.queueAcks(rt.sched.receive(resp.CommandRequests))
	}
	if resp.Rule != nil {
		resp.Rule.Content.Normalize()
	}
	differs, intervalChanged := rt.link.offer(resp.Rule, resp.CheckInterval())
	if intervalChanged {
		rt.storeCheckInterval(resp.CheckInterval())
	}
	if differs {
		rt.sched.monitorNow()
	}
}

// storeCheckInterval stores the check interval a sync answer gave, for the
// time before the first sync after the next start. A failure is logged; the
// interval is in use all the same.
func (rt *AgentRuntime) storeCheckInterval(sec int) {
	if err := rt.DB.Write(func(tx models.Writer) error {
		return models.SetLocalState(tx, stateCheckInterval, strconv.Itoa(sec))
	}); err != nil {
		rt.logf("failed to store the check interval: %v", err)
	}
}

// runningRequests returns the request IDs of the manual runs that were
// started and are not finished, from the local database (never nil).
func (rt *AgentRuntime) runningRequests() ([]int64, error) {
	runs, err := models.ListUnfinishedCommandExecutions(rt.DB.Read())
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for _, e := range runs {
		if e.RequestID > 0 {
			ids = append(ids, e.RequestID)
		}
	}
	return ids, nil
}

func (rt *AgentRuntime) saveConfigUpdates(resp *SyncResponse) {
	cfg := *rt.comm.config
	cfg.CAPEMs = append([]string(nil), rt.comm.config.CAPEMs...)
	changed := false
	if resp.TokenExpiresAt > 0 && resp.TokenExpiresAt != cfg.TokenExpiresAt {
		cfg.TokenExpiresAt = resp.TokenExpiresAt
		changed = true
	}
	if resp.AgentName != "" && resp.AgentName != cfg.TokenName {
		cfg.TokenName = resp.AgentName
		changed = true
	}
	renewed := false
	if resp.TokenRenewal != nil && resp.TokenRenewal.Token != "" && resp.TokenRenewal.Token != cfg.Token {
		cfg.Token = resp.TokenRenewal.Token
		cfg.TokenExpiresAt = resp.TokenRenewal.ExpiresAt
		changed, renewed = true, true
	}
	if cfg.MergeCAs(resp.CAPEMs, time.Now()) {
		changed = true
	}
	if !changed {
		return
	}
	if err := rt.DB.Write(cfg.Save); err != nil {
		// Keep using the old token; the central hands out the same new value
		// again on the next connection.
		rt.logf("failed to save configuration (token renewal or CA update postponed): %v", err)
		return
	}
	*rt.comm.config = cfg
	rt.link.setCAs(cfg.CAPEMs)
	if renewed {
		rt.logf("token renewed; new expiry %s", time.Unix(cfg.TokenExpiresAt, 0).Local().Format("2006-01-02 15:04:05 MST"))
	}
	if rt.comm.onConfigSaved != nil {
		rt.comm.onConfigSaved(rt.comm.config)
	}
}

func (rt *AgentRuntime) logConnectionError(op string, err error) {
	var he *HTTPError
	var ne net.Error
	switch {
	case errors.Is(err, errRequestCut):
		// A cut report keeps its batch (the last report sends it again); the
		// requests a cut sync was handed are given back by the last report.
		rt.logf("%s: cut by the agent's stop", op)
		return
	case errors.As(err, &he) && he.Status == 401:
		rt.logf("%s: the central rejected the token (expired, revoked, reissued or bound to another agent instance). Re-initialize the agent with a valid token", op)
	case IsSilenceCut(err):
		// The batch of a cut report stays recorded and is sent again in the
		// next round.
		rt.logf("%s: the central sent nothing for %d seconds: %v", op, int(IdleTimeout/time.Second), err)
	case IsCertificateError(err):
		rt.logf("%s: the central certificate is not signed by a trusted CA (%v). If the CA was rolled over while this agent was offline, re-initialize the agent", op, err)
	case errors.As(err, &he):
		rt.logf("%s: the central answered %v", op, err)
	case errors.As(err, &ne), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		// A network error, or a connection the peer closed before the
		// answer was complete (in the TLS handshake, after the request, in
		// the middle of the answer).
		rt.logf("%s: cannot reach the central: %v", op, err)
	case rt.comm.config == nil:
		// The built-in agent hands its requests to the central in process.
		rt.logf("%s: the central could not take it: %v", op, err)
	default:
		rt.logf("%s failed: %v", op, err)
	}
	if rt.comm.config != nil && rt.comm.config.TokenExpiresAt > 0 {
		left := time.Until(time.Unix(rt.comm.config.TokenExpiresAt, 0))
		if left < time.Duration(DefaultTokenRenewDays)*24*time.Hour {
			rt.logf("warning: the token expires at %s (in %s) and could not be renewed",
				time.Unix(rt.comm.config.TokenExpiresAt, 0).Local().Format("2006-01-02 15:04:05 MST"), left.Round(time.Minute))
		}
	}
}

// flushOutbox sends the queued items in one report request (see
// outbox.flush); a failure is logged as a failed report.
func (rt *AgentRuntime) flushOutbox() {
	rt.out.flush(rt.comm.transport, false, func(err error) { rt.logConnectionError("report", err) })
}

// flushStopping sends the last report of the agent before it ends: the
// queued items with stopping true.
func (rt *AgentRuntime) flushStopping() {
	rt.out.flush(rt.comm.transport, true, func(err error) { rt.logConnectionError("report", err) })
}
