package models

import "database/sql"

// ReceivedReport is the single row of the received_report table of a
// per-target database: the identity of the last report of the agent that the
// central stored, written in the transaction that stored the report. The
// agent sends its reports one at a time and the next only after the previous
// one was answered, so only this one can have been stored without its answer
// reaching the agent; a report with the same identity is not stored again.
type ReceivedReport struct {
	ID         int64
	ReportID   string
	ReceivedAt int64
}

// LastReceivedReport returns the identity of the last stored report ("" when
// none was remembered yet).
func LastReceivedReport(db Reader) (string, error) {
	var id string
	err := db.QueryRow(`SELECT report_id FROM received_report WHERE id = 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// SetLastReceivedReport remembers reportID as the last stored report,
// replacing the one remembered before.
func SetLastReceivedReport(db Writer, reportID string, now int64) error {
	_, err := db.Exec(`INSERT INTO received_report (id, report_id, received_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET report_id = excluded.report_id, received_at = excluded.received_at`, reportID, now)
	return err
}
