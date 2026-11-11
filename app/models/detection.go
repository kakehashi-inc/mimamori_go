package models

import (
	"fmt"
	"strings"
)

// Notify states of a detection.
const (
	NotifyPending = "pending" // to be included in the next notification
	NotifySent    = "sent"    // included in a notification
	NotifySkipped = "skipped" // suppressed or already notified within the dedupe period
)

// Detection is a row of the detections table of a per-target database. Audit
// events are aggregated per (type, user, executable, target) and file scan
// findings per file; repeated occurrences increase Count until the row is
// acknowledged, after which a new row is started.
type Detection struct {
	ID             int64
	Fingerprint    string
	Type           string
	Category       string
	OSUser         string
	Exe            string
	Target         string
	Count          int
	FirstAt        int64
	LastAt         int64
	FileCtime      int64
	FileOwner      string
	FileMode       string
	Suppressed     bool
	SuppressionID  int64
	AcknowledgedAt int64
	AcknowledgedBy string
	NotifyState    string
	NotifiedAt     int64
	CreatedAt      int64
}

const detectionCols = `id, fingerprint, type, category, os_user, exe, target, count, first_at, last_at, file_ctime, file_owner, file_mode,
	suppressed, suppression_id, acknowledged_at, acknowledged_by, notify_state, notified_at, created_at`

func scanDetection(s rowScanner) (*Detection, error) {
	d := &Detection{}
	var sup int
	if err := s.Scan(&d.ID, &d.Fingerprint, &d.Type, &d.Category, &d.OSUser, &d.Exe, &d.Target, &d.Count, &d.FirstAt, &d.LastAt,
		&d.FileCtime, &d.FileOwner, &d.FileMode, &sup, &d.SuppressionID, &d.AcknowledgedAt, &d.AcknowledgedBy,
		&d.NotifyState, &d.NotifiedAt, &d.CreatedAt); err != nil {
		return nil, err
	}
	d.Suppressed = sup != 0
	return d, nil
}

// FindOpenDetection returns the unacknowledged row with the fingerprint.
func FindOpenDetection(db Execer, fingerprint string) (*Detection, error) {
	return scanDetection(db.QueryRow(`SELECT `+detectionCols+` FROM detections WHERE fingerprint = ? AND acknowledged_at = 0 ORDER BY id DESC LIMIT 1`, fingerprint))
}

// LastNotifiedAt returns the latest notification time of a fingerprint.
func LastNotifiedAt(db Execer, fingerprint string) int64 {
	var t int64
	_ = db.QueryRow(`SELECT COALESCE(MAX(notified_at), 0) FROM detections WHERE fingerprint = ?`, fingerprint).Scan(&t)
	return t
}

// InsertDetection stores a new detection row.
func InsertDetection(db Execer, d *Detection) error {
	d.CreatedAt = Now()
	res, err := db.Exec(`INSERT INTO detections (fingerprint, type, category, os_user, exe, target, count, first_at, last_at, file_ctime,
		file_owner, file_mode, suppressed, suppression_id, notify_state, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Fingerprint, d.Type, d.Category, d.OSUser, d.Exe, d.Target, d.Count, d.FirstAt, d.LastAt, d.FileCtime, d.FileOwner,
		d.FileMode, boolToInt(d.Suppressed), d.SuppressionID, d.NotifyState, d.CreatedAt)
	if err != nil {
		return err
	}
	d.ID, _ = res.LastInsertId()
	return nil
}

// UpdateDetectionOccurrence merges new occurrences into an open row.
func UpdateDetectionOccurrence(db Execer, d *Detection) error {
	return execOne(db, `UPDATE detections SET category = ?, count = ?, first_at = ?, last_at = ?, file_ctime = ?, file_owner = ?, file_mode = ?,
		suppressed = ?, suppression_id = ?, notify_state = ? WHERE id = ?`,
		d.Category, d.Count, d.FirstAt, d.LastAt, d.FileCtime, d.FileOwner, d.FileMode, boolToInt(d.Suppressed), d.SuppressionID, d.NotifyState, d.ID)
}

// GetDetection returns one detection.
func GetDetection(db Execer, id int64) (*Detection, error) {
	return scanDetection(db.QueryRow(`SELECT `+detectionCols+` FROM detections WHERE id = ?`, id))
}

// DetectionFilter narrows a detection listing.
type DetectionFilter struct {
	Category     string
	Type         string
	From, To     int64
	Acknowledged *bool
	Suppressed   *bool
}

func (f DetectionFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.Category != "" {
		conds = append(conds, "category = ?")
		args = append(args, f.Category)
	}
	if f.Type != "" {
		conds = append(conds, "type = ?")
		args = append(args, f.Type)
	}
	if f.From > 0 {
		conds = append(conds, "last_at >= ?")
		args = append(args, f.From)
	}
	if f.To > 0 {
		conds = append(conds, "last_at <= ?")
		args = append(args, f.To)
	}
	if f.Acknowledged != nil {
		if *f.Acknowledged {
			conds = append(conds, "acknowledged_at > 0")
		} else {
			conds = append(conds, "acknowledged_at = 0")
		}
	}
	if f.Suppressed != nil {
		conds = append(conds, fmt.Sprintf("suppressed = %d", boolToInt(*f.Suppressed)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListDetections returns matching detections, latest first, at most limit
// rows, together with the total number of matches.
func ListDetections(db Execer, f DetectionFilter, limit int) ([]*Detection, int, error) {
	where, args := f.where()
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM detections`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.Query(`SELECT `+detectionCols+` FROM detections`+where+` ORDER BY last_at DESC, id DESC LIMIT ?`, append(args, limit)...)
	items, err := collect(rows, err, scanDetection)
	return items, total, err
}

// CountUnackedAlerts counts unacknowledged, unsuppressed alert detections.
func CountUnackedAlerts(db Execer) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM detections WHERE category = 'alert' AND acknowledged_at = 0 AND suppressed = 0`).Scan(&n)
	return n, err
}

// AcknowledgeDetection marks a detection as acknowledged.
func AcknowledgeDetection(db Execer, id int64, by string, now int64) (bool, error) {
	res, err := db.Exec(`UPDATE detections SET acknowledged_at = ?, acknowledged_by = ? WHERE id = ? AND acknowledged_at = 0`, now, by, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListPendingNotify returns detections waiting for notification.
func ListPendingNotify(db Execer) ([]*Detection, error) {
	rows, err := db.Query(`SELECT ` + detectionCols + ` FROM detections WHERE notify_state = 'pending' ORDER BY CASE category WHEN 'alert' THEN 0 ELSE 1 END, last_at DESC, id DESC`)
	return collect(rows, err, scanDetection)
}

// MarkNotified marks detections as notified.
func MarkNotified(db Execer, ids []int64, now int64) error {
	for _, id := range ids {
		if _, err := db.Exec(`UPDATE detections SET notify_state = 'sent', notified_at = ? WHERE id = ?`, now, id); err != nil {
			return err
		}
	}
	return nil
}

// DeleteDetectionsBefore removes detections whose last occurrence is older
// than t (retention period).
func DeleteDetectionsBefore(db Execer, t int64) (int64, error) {
	res, err := db.Exec(`DELETE FROM detections WHERE last_at < ?`, t)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
