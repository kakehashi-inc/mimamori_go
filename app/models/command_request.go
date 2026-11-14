package models

import "errors"

// States of a manual run request.
const (
	RequestQueued    = "queued"    // waiting for a free worker of the central
	RequestPending   = "pending"   // a worker was given; waiting for the agent's next sync
	RequestDelivered = "delivered" // handed to the agent with a sync, no answer yet
	RequestRunning   = "running"   // the agent answered that it started the run (RunUID)
	RequestDone      = "done"      // the central learned that the run ended
	RequestExpired   = "expired"   // not handed over in time, or the agent answered that it arrived too late or that it does not hold the set
	RequestDiscarded = "discarded" // discarded by an administrator before it was handed over, or with its set
)

// ErrRequestActive is returned when the set already has an unanswered
// (queued, pending or delivered) request.
var ErrRequestActive = errors.New("the command set already has a run request waiting")

// CommandRequest is a row of the command_requests table of the central
// database: a manual run request of a set of one agent. A set has at most
// one unanswered request.
type CommandRequest struct {
	ID          int64
	AgentID     int64
	SetID       int64
	State       string
	RequestedAt int64
	RequestedBy string
	ExpiresAt   int64
	DeliveredAt int64
	DoneAt      int64
	RunUID      string
	DiscardedAt int64
	DiscardedBy string
}

const commandRequestCols = `id, agent_id, set_id, state, requested_at, requested_by, expires_at, delivered_at, done_at, run_uid,
	discarded_at, discarded_by`

func scanCommandRequest(s rowScanner) (*CommandRequest, error) {
	r := &CommandRequest{}
	if err := s.Scan(&r.ID, &r.AgentID, &r.SetID, &r.State, &r.RequestedAt, &r.RequestedBy, &r.ExpiresAt, &r.DeliveredAt,
		&r.DoneAt, &r.RunUID, &r.DiscardedAt, &r.DiscardedBy); err != nil {
		return nil, err
	}
	return r, nil
}

// execChanged runs a conditional UPDATE and reports whether it changed a row.
func execChanged(db Writer, query string, args ...any) (bool, error) {
	res, err := db.Exec(query, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// InsertCommandRequest creates a queued request. It returns
// ErrRequestActive when the set already has an unanswered one.
func InsertCommandRequest(db Writer, r *CommandRequest) error {
	r.State = RequestQueued
	res, err := db.Exec(`INSERT INTO command_requests (agent_id, set_id, state, requested_at, requested_by, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		r.AgentID, r.SetID, r.State, r.RequestedAt, r.RequestedBy, r.ExpiresAt)
	if isUniqueViolation(err) {
		return ErrRequestActive
	}
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

// GetCommandRequest returns a request of an agent (sql.ErrNoRows when the
// agent has no request with this id).
func GetCommandRequest(db Reader, agentID, id int64) (*CommandRequest, error) {
	return scanCommandRequest(db.QueryRow(`SELECT `+commandRequestCols+` FROM command_requests WHERE id = ? AND agent_id = ?`, id, agentID))
}

// UnansweredCommandRequests returns the unanswered (queued, pending,
// delivered) requests of an agent by set id.
func UnansweredCommandRequests(db Reader, agentID int64) (map[int64]*CommandRequest, error) {
	rows, err := db.Query(`SELECT `+commandRequestCols+` FROM command_requests
		WHERE agent_id = ? AND state IN ('queued', 'pending', 'delivered')`, agentID)
	list, err := collect(rows, err, scanCommandRequest)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]*CommandRequest, len(list))
	for _, r := range list {
		out[r.SetID] = r
	}
	return out, nil
}

// ListOpenCommandRequests returns the requests of all agents in queued,
// pending, delivered and running, by id.
func ListOpenCommandRequests(db Reader) ([]*CommandRequest, error) {
	rows, err := db.Query(`SELECT ` + commandRequestCols + ` FROM command_requests
		WHERE state IN ('queued', 'pending', 'delivered', 'running') ORDER BY id`)
	return collect(rows, err, scanCommandRequest)
}

// GrantCommandRequest gives a worker to a queued request (queued ->
// pending).
func GrantCommandRequest(db Writer, id int64) (bool, error) {
	return execChanged(db, `UPDATE command_requests SET state = 'pending' WHERE id = ? AND state = 'queued'`, id)
}

// ReleaseCommandRequest puts a granted request back into the queue
// (pending -> queued); it keeps its id and so its place.
func ReleaseCommandRequest(db Writer, id int64) (bool, error) {
	return execChanged(db, `UPDATE command_requests SET state = 'queued' WHERE id = ? AND state = 'pending'`, id)
}

// HandOverCommandRequests marks the pending requests of an agent whose
// expiry has not passed as delivered and returns every delivered request of
// the agent, by id (a delivered request is sent again until the agent
// answers it).
func HandOverCommandRequests(db Writer, agentID, now int64) ([]*CommandRequest, error) {
	if _, err := db.Exec(`UPDATE command_requests SET state = 'delivered', delivered_at = ?
		WHERE agent_id = ? AND state = 'pending' AND expires_at > ?`, now, agentID, now); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT `+commandRequestCols+` FROM command_requests WHERE agent_id = ? AND state = 'delivered' ORDER BY id`, agentID)
	return collect(rows, err, scanCommandRequest)
}

// MarkCommandRequestRunning records that the agent started the run of a
// delivered request (delivered -> running).
func MarkCommandRequestRunning(db Writer, agentID, id int64, runUID string) (bool, error) {
	return execChanged(db, `UPDATE command_requests SET state = 'running', run_uid = ? WHERE id = ? AND agent_id = ? AND state = 'delivered'`,
		runUID, id, agentID)
}

// MarkCommandRequestDone records that the run of a delivered or running
// request ended (-> done). An empty runUID keeps the stored one.
func MarkCommandRequestDone(db Writer, agentID, id int64, runUID string, now int64) (bool, error) {
	return execChanged(db, `UPDATE command_requests SET state = 'done', done_at = ?, run_uid = CASE WHEN ? = '' THEN run_uid ELSE ? END
		WHERE id = ? AND agent_id = ? AND state IN ('delivered', 'running')`, now, runUID, runUID, id, agentID)
}

// MarkCommandRequestExpired ends a delivered request as expired (the agent
// answered that it arrived too late or that it does not hold the set).
func MarkCommandRequestExpired(db Writer, agentID, id, now int64) (bool, error) {
	return execChanged(db, `UPDATE command_requests SET state = 'expired', done_at = ? WHERE id = ? AND agent_id = ? AND state = 'delivered'`,
		now, id, agentID)
}

// EndUnheldCommandRequests ends the running requests of an agent whose id
// is not in held (the agent no longer runs them) and returns how many. An
// empty held ends every running request of the agent.
func EndUnheldCommandRequests(db Writer, agentID int64, held []int64, now int64) (int64, error) {
	rows, err := db.Query(`SELECT `+commandRequestCols+` FROM command_requests WHERE agent_id = ? AND state = 'running' ORDER BY id`, agentID)
	list, err := collect(rows, err, scanCommandRequest)
	if err != nil {
		return 0, err
	}
	keep := make(map[int64]bool, len(held))
	for _, id := range held {
		keep[id] = true
	}
	var n int64
	for _, r := range list {
		if keep[r.ID] {
			continue
		}
		ok, err := execChanged(db, `UPDATE command_requests SET state = 'done', done_at = ? WHERE id = ? AND agent_id = ? AND state = 'running'`,
			now, r.ID, agentID)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// ExpireCommandRequests ends the queued and pending requests whose expiry
// passed and returns them (with their new state), by id.
func ExpireCommandRequests(db Writer, now int64) ([]*CommandRequest, error) {
	rows, err := db.Query(`SELECT `+commandRequestCols+` FROM command_requests
		WHERE state IN ('queued', 'pending') AND expires_at <= ? ORDER BY id`, now)
	list, err := collect(rows, err, scanCommandRequest)
	if err != nil {
		return nil, err
	}
	var out []*CommandRequest
	for _, r := range list {
		ok, err := execChanged(db, `UPDATE command_requests SET state = 'expired', done_at = ? WHERE id = ? AND state IN ('queued', 'pending')`, now, r.ID)
		if err != nil {
			return out, err
		}
		if ok {
			r.State, r.DoneAt = RequestExpired, now
			out = append(out, r)
		}
	}
	return out, nil
}

// DiscardCommandRequest discards a request of an agent that was not yet
// handed over (queued or pending).
func DiscardCommandRequest(db Writer, agentID, id int64, by string, now int64) (bool, error) {
	return execChanged(db, `UPDATE command_requests SET state = 'discarded', discarded_at = ?, discarded_by = ?
		WHERE id = ? AND agent_id = ? AND state IN ('queued', 'pending')`, now, by, id, agentID)
}

// DiscardCommandRequestsOfSet discards the queued and pending requests of a
// set (with its deletion) and returns their ids.
func DiscardCommandRequestsOfSet(db Writer, agentID, setID int64, by string, now int64) ([]int64, error) {
	rows, err := db.Query(`SELECT `+commandRequestCols+` FROM command_requests
		WHERE agent_id = ? AND set_id = ? AND state IN ('queued', 'pending') ORDER BY id`, agentID, setID)
	list, err := collect(rows, err, scanCommandRequest)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, r := range list {
		ok, err := DiscardCommandRequest(db, agentID, r.ID, by, now)
		if err != nil {
			return ids, err
		}
		if ok {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

// DeleteEndedCommandRequestsBefore removes the done, expired and discarded
// requests of all agents requested before t (retention).
func DeleteEndedCommandRequestsBefore(db Writer, t int64) (int64, error) {
	res, err := db.Exec(`DELETE FROM command_requests WHERE state IN ('done', 'expired', 'discarded') AND requested_at < ?`, t)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
