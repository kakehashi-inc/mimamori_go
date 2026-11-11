package models

// OutboxItem is a row of the outbox table of the agent's local database: a
// report not yet delivered to the central (JSON of modules.ReportRequest).
type OutboxItem struct {
	ID          int64
	CreatedAt   int64
	Payload     string
	AlertCount  int
	NormalCount int
}

// InsertOutbox queues a report.
func InsertOutbox(db Execer, o *OutboxItem) error {
	o.CreatedAt = Now()
	res, err := db.Exec(`INSERT INTO outbox (created_at, payload, alert_count, normal_count) VALUES (?, ?, ?, ?)`, o.CreatedAt, o.Payload, o.AlertCount, o.NormalCount)
	if err != nil {
		return err
	}
	o.ID, _ = res.LastInsertId()
	return nil
}

// ListOutbox returns queued reports, oldest first.
func ListOutbox(db Execer) ([]*OutboxItem, error) {
	rows, err := db.Query(`SELECT id, created_at, payload, alert_count, normal_count FROM outbox ORDER BY id`)
	return collect(rows, err, func(s rowScanner) (*OutboxItem, error) {
		o := &OutboxItem{}
		if err := s.Scan(&o.ID, &o.CreatedAt, &o.Payload, &o.AlertCount, &o.NormalCount); err != nil {
			return nil, err
		}
		return o, nil
	})
}

// UpdateOutbox rewrites a queued report (after dropping detections).
func UpdateOutbox(db Execer, o *OutboxItem) error {
	_, err := db.Exec(`UPDATE outbox SET payload = ?, alert_count = ?, normal_count = ? WHERE id = ?`, o.Payload, o.AlertCount, o.NormalCount, o.ID)
	return err
}

// DeleteOutbox removes a delivered report.
func DeleteOutbox(db Execer, id int64) error {
	_, err := db.Exec(`DELETE FROM outbox WHERE id = ?`, id)
	return err
}

// OutboxDetectionCount returns the number of queued detections.
func OutboxDetectionCount(db Execer) int {
	var n int
	_ = db.QueryRow(`SELECT COALESCE(SUM(alert_count + normal_count), 0) FROM outbox`).Scan(&n)
	return n
}

// CountOutbox returns the number of queued reports.
func CountOutbox(db Execer) int {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&n)
	return n
}
