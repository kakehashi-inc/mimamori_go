package models

// History categories.
const (
	HistAccount     = "account"
	HistRule        = "rule"
	HistToken       = "token"
	HistCertificate = "certificate"
	HistRejected    = "rejected"
	HistTemplate    = "template"
	HistSuppression = "suppression"
	HistDetection   = "detection"
	HistConfig      = "config"
)

// HistoryItem is a row of the history table: an operation with the account
// that performed it, or a system event such as a rejected connection.
type HistoryItem struct {
	ID          int64
	At          int64
	LastAt      int64
	AccountName string
	Category    string
	Action      string
	Target      string
	Detail      string
	RemoteAddr  string
	Count       int
}

const historyCols = `id, at, last_at, account_name, category, action, target, detail, remote_addr, count`

func scanHistory(s rowScanner) (*HistoryItem, error) {
	h := &HistoryItem{}
	if err := s.Scan(&h.ID, &h.At, &h.LastAt, &h.AccountName, &h.Category, &h.Action, &h.Target, &h.Detail, &h.RemoteAddr, &h.Count); err != nil {
		return nil, err
	}
	return h, nil
}

// InsertHistory records an event.
func InsertHistory(db Execer, h *HistoryItem) error {
	now := Now()
	_, err := db.Exec(`INSERT INTO history (at, last_at, account_name, category, action, target, detail, remote_addr, count) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		now, now, h.AccountName, h.Category, h.Action, h.Target, h.Detail, h.RemoteAddr)
	return err
}

// RecordRejected records a rejected connection. Repeated rejections from the
// same address for the same reason within window seconds are counted on one row.
func RecordRejected(db Execer, remote, reason, target string, window int64) error {
	now := Now()
	res, err := db.Exec(`UPDATE history SET count = count + 1, last_at = ? WHERE id = (
		SELECT id FROM history WHERE category = 'rejected' AND remote_addr = ? AND action = ? AND target = ? AND last_at >= ? ORDER BY id DESC LIMIT 1)`,
		now, remote, reason, target, now-window)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	return InsertHistory(db, &HistoryItem{AccountName: "system", Category: HistRejected, Action: reason, Target: target, RemoteAddr: remote})
}

// ListHistory returns history rows newest first with optional filters.
func ListHistory(db Execer, category, target string, limit, offset int) ([]*HistoryItem, int, error) {
	where := `WHERE (? = '' OR category = ?) AND (? = '' OR target = ?)`
	args := []any{category, category, target, target}
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.Query(`SELECT `+historyCols+` FROM history `+where+` ORDER BY last_at DESC, id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	items, err := collect(rows, err, scanHistory)
	return items, total, err
}

// DeleteRejectedHistoryBefore removes rejected-connection rows older than t.
func DeleteRejectedHistoryBefore(db Execer, t int64) error {
	_, err := db.Exec(`DELETE FROM history WHERE category = 'rejected' AND last_at < ?`, t)
	return err
}
