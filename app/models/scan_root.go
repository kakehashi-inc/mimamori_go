package models

// ScanRoot is a row of the scan_roots table of the agent's local database: a
// directory matching an allowed path, found by the last full scan. The
// executed-file scan walks only these directories between full scans.
type ScanRoot struct {
	Path string
}

// ReplaceScanRoots stores the roots found by a full scan.
func ReplaceScanRoots(db Execer, paths []string) error {
	if _, err := db.Exec(`DELETE FROM scan_roots`); err != nil {
		return err
	}
	for _, p := range paths {
		if _, err := db.Exec(`INSERT OR IGNORE INTO scan_roots (path) VALUES (?)`, p); err != nil {
			return err
		}
	}
	return nil
}

// ListScanRoots returns the stored roots.
func ListScanRoots(db Execer) ([]string, error) {
	rows, err := db.Query(`SELECT path FROM scan_roots ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
