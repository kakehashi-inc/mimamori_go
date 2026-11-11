package models

// HostReport is a row of the host_reports table of a per-target database:
// the latest host information (JSON of modules.HostInfo). Only the newest
// rows are kept.
type HostReport struct {
	ID         int64
	ReceivedAt int64
	HostInfo   string
}

// InsertHostReport stores host information and keeps only the latest keep rows.
func InsertHostReport(db Execer, info string, keep int) error {
	if _, err := db.Exec(`INSERT INTO host_reports (received_at, host_info) VALUES (?, ?)`, Now(), info); err != nil {
		return err
	}
	_, err := db.Exec(`DELETE FROM host_reports WHERE id NOT IN (SELECT id FROM host_reports ORDER BY id DESC LIMIT ?)`, keep)
	return err
}

// LatestHostReport returns the newest host information.
func LatestHostReport(db Execer) (*HostReport, error) {
	h := &HostReport{}
	err := db.QueryRow(`SELECT id, received_at, host_info FROM host_reports ORDER BY id DESC LIMIT 1`).Scan(&h.ID, &h.ReceivedAt, &h.HostInfo)
	if err != nil {
		return nil, err
	}
	return h, nil
}
