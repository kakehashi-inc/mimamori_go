package models

// OutboxItem is a row of the outbox table of the agent's local database: a
// report not yet delivered to the central (JSON of modules.ReportRequest).
// CommandBytes is the size of the payload when it carries command set runs
// (0 otherwise); their total is bounded.
type OutboxItem struct {
	ID           int64
	CreatedAt    int64
	Payload      string
	AlertCount   int
	NormalCount  int
	CommandBytes int64
}

// InsertOutbox queues a report.
func InsertOutbox(db Writer, o *OutboxItem) error {
	o.CreatedAt = Now()
	res, err := db.Exec(`INSERT INTO outbox (created_at, payload, alert_count, normal_count, command_bytes) VALUES (?, ?, ?, ?, ?)`,
		o.CreatedAt, o.Payload, o.AlertCount, o.NormalCount, o.CommandBytes)
	if err != nil {
		return err
	}
	o.ID, _ = res.LastInsertId()
	return nil
}

// ListOutbox returns queued reports, oldest first.
func ListOutbox(db Reader) ([]*OutboxItem, error) {
	rows, err := db.Query(`SELECT id, created_at, payload, alert_count, normal_count, command_bytes FROM outbox ORDER BY id`)
	return collect(rows, err, func(s rowScanner) (*OutboxItem, error) {
		o := &OutboxItem{}
		if err := s.Scan(&o.ID, &o.CreatedAt, &o.Payload, &o.AlertCount, &o.NormalCount, &o.CommandBytes); err != nil {
			return nil, err
		}
		return o, nil
	})
}

// UpdateOutbox rewrites a queued report (after dropping detections or
// command output).
func UpdateOutbox(db Writer, o *OutboxItem) error {
	_, err := db.Exec(`UPDATE outbox SET payload = ?, alert_count = ?, normal_count = ?, command_bytes = ? WHERE id = ?`,
		o.Payload, o.AlertCount, o.NormalCount, o.CommandBytes, o.ID)
	return err
}

// DeleteOutbox removes a delivered report.
func DeleteOutbox(db Writer, id int64) error {
	_, err := db.Exec(`DELETE FROM outbox WHERE id = ?`, id)
	return err
}

// OutboxDetectionCount returns the number of queued detections.
func OutboxDetectionCount(db Reader) int {
	var n int
	_ = db.QueryRow(`SELECT COALESCE(SUM(alert_count + normal_count), 0) FROM outbox`).Scan(&n)
	return n
}

// CountOutbox returns the number of queued reports.
func CountOutbox(db Reader) int {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&n)
	return n
}

// OutboxCommandBytes returns the size of the queued reports that carry
// command set runs.
func OutboxCommandBytes(db Reader) int64 {
	var n int64
	_ = db.QueryRow(`SELECT COALESCE(SUM(command_bytes), 0) FROM outbox`).Scan(&n)
	return n
}
