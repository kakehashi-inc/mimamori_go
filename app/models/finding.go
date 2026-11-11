package models

// Finding is a row of the findings table of the agent's local database: a
// file scan finding (file and its state) already reported, so an unchanged
// file is not reported again by every scan.
type Finding struct {
	Fingerprint string
	SeenAt      int64
}

// FindingExists reports whether the fingerprint was reported before.
func FindingExists(db Execer, fp string) bool {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM findings WHERE fingerprint = ?`, fp).Scan(&n)
	return n > 0
}

// TouchFinding records the fingerprint as seen at now.
func TouchFinding(db Execer, fp string, now int64) error {
	_, err := db.Exec(`INSERT INTO findings (fingerprint, seen_at) VALUES (?, ?) ON CONFLICT(fingerprint) DO UPDATE SET seen_at = excluded.seen_at`, fp, now)
	return err
}

// PruneFindings removes fingerprints with the prefix not seen since t.
func PruneFindings(db Execer, prefix string, t int64) error {
	_, err := db.Exec(`DELETE FROM findings WHERE fingerprint LIKE ? AND seen_at < ?`, prefix+"%", t)
	return err
}
