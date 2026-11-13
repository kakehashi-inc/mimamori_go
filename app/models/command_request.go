package models

import (
	"database/sql"
	"errors"
)

// States of a manual run request.
const (
	RequestPending   = "pending"   // waiting for delivery with a sync
	RequestDelivered = "delivered" // handed to the agent, no answer yet
	RequestDone      = "done"      // the agent started a run for it (RunUID)
	RequestExpired   = "expired"   // not delivered in time (or arrived after its expiry)
	RequestDiscarded = "discarded" // discarded by an administrator before delivery
)

// ErrRequestActive is returned when the set already has a pending or
// delivered request.
var ErrRequestActive = errors.New("the command set already has a run request waiting")

// CommandRequest is a row of the command_requests table of a per-target
// database: a manual run request of a set. A set has at most one pending or
// delivered request.
type CommandRequest struct {
	ID          int64
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

const commandRequestCols = `id, set_id, state, requested_at, requested_by, expires_at, delivered_at, done_at, run_uid,
	discarded_at, discarded_by`

func scanCommandRequest(s rowScanner) (*CommandRequest, error) {
	r := &CommandRequest{}
	if err := s.Scan(&r.ID, &r.SetID, &r.State, &r.RequestedAt, &r.RequestedBy, &r.ExpiresAt, &r.DeliveredAt, &r.DoneAt,
		&r.RunUID, &r.DiscardedAt, &r.DiscardedBy); err != nil {
		return nil, err
	}
	return r, nil
}

// InsertCommandRequest creates a pending request. It returns
// ErrRequestActive when the set already has a pending or delivered one.
func InsertCommandRequest(db Writer, r *CommandRequest) error {
	r.State = RequestPending
	res, err := db.Exec(`INSERT INTO command_requests (set_id, state, requested_at, requested_by, expires_at) VALUES (?, ?, ?, ?, ?)`,
		r.SetID, r.State, r.RequestedAt, r.RequestedBy, r.ExpiresAt)
	if isUniqueViolation(err) {
		return ErrRequestActive
	}
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

// GetCommandRequest returns a request.
func GetCommandRequest(db Reader, id int64) (*CommandRequest, error) {
	return scanCommandRequest(db.QueryRow(`SELECT `+commandRequestCols+` FROM command_requests WHERE id = ?`, id))
}

// ActiveCommandRequest returns the pending or delivered request of a set
// (nil when there is none).
func ActiveCommandRequest(db Reader, setID int64) (*CommandRequest, error) {
	r, err := scanCommandRequest(db.QueryRow(`SELECT `+commandRequestCols+` FROM command_requests
		WHERE set_id = ? AND state IN ('pending', 'delivered')`, setID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// ListActiveCommandRequests returns the pending and delivered requests,
// oldest first.
func ListActiveCommandRequests(db Reader) ([]*CommandRequest, error) {
	rows, err := db.Query(`SELECT ` + commandRequestCols + ` FROM command_requests WHERE state IN ('pending', 'delivered') ORDER BY id`)
	return collect(rows, err, scanCommandRequest)
}

// MarkCommandRequestDelivered records the first delivery of a pending
// request. It returns sql.ErrNoRows when the request is not pending.
func MarkCommandRequestDelivered(db Writer, id, now int64) error {
	return execOne(db, `UPDATE command_requests SET state = 'delivered', delivered_at = ? WHERE id = ? AND state = 'pending'`, now, id)
}

// MarkCommandRequestDone records that the agent started a run for a pending
// or delivered request. It returns sql.ErrNoRows otherwise.
func MarkCommandRequestDone(db Writer, id int64, runUID string, now int64) error {
	return execOne(db, `UPDATE command_requests SET state = 'done', done_at = ?, run_uid = ? WHERE id = ? AND state IN ('pending', 'delivered')`,
		now, runUID, id)
}

// MarkCommandRequestExpired ends a pending or delivered request as expired
// (the agent answered that it arrived too late). It returns sql.ErrNoRows
// otherwise.
func MarkCommandRequestExpired(db Writer, id, now int64) error {
	return execOne(db, `UPDATE command_requests SET state = 'expired', done_at = ? WHERE id = ? AND state IN ('pending', 'delivered')`, now, id)
}

// DiscardCommandRequest discards a request that was not yet delivered. It
// returns sql.ErrNoRows when the request is not pending.
func DiscardCommandRequest(db Writer, id int64, by string, now int64) error {
	return execOne(db, `UPDATE command_requests SET state = 'discarded', discarded_at = ?, discarded_by = ? WHERE id = ? AND state = 'pending'`,
		now, by, id)
}

// ExpireCommandRequests ends the pending requests whose expiry passed and
// returns them (with their new state).
func ExpireCommandRequests(db Writer, now int64) ([]*CommandRequest, error) {
	rows, err := db.Query(`SELECT `+commandRequestCols+` FROM command_requests WHERE state = 'pending' AND expires_at <= ? ORDER BY id`, now)
	list, err := collect(rows, err, scanCommandRequest)
	if err != nil {
		return nil, err
	}
	var out []*CommandRequest
	for _, r := range list {
		res, err := db.Exec(`UPDATE command_requests SET state = 'expired', done_at = ? WHERE id = ? AND state = 'pending'`, now, r.ID)
		if err != nil {
			return out, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			r.State, r.DoneAt = RequestExpired, now
			out = append(out, r)
		}
	}
	return out, nil
}

// DeleteEndedCommandRequestsBefore removes done, expired and discarded
// requests requested before t (retention).
func DeleteEndedCommandRequestsBefore(db Writer, t int64) (int64, error) {
	res, err := db.Exec(`DELETE FROM command_requests WHERE state IN ('done', 'expired', 'discarded') AND requested_at < ?`, t)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
