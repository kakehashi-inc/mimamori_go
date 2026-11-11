package models

import "database/sql"

// Suppression is a row of the suppressions table: a condition an
// administrator judged normal. Matching detections are recorded but not
// notified. Empty fields match anything; exe and target accept "*".
type Suppression struct {
	ID        int64
	AgentID   sql.NullInt64
	Type      string
	OSUser    string
	Exe       string
	Target    string
	Comment   string
	CreatedAt int64
	CreatedBy string
}

const suppressionCols = `id, agent_id, type, os_user, exe, target, comment, created_at, created_by`

func scanSuppression(s rowScanner) (*Suppression, error) {
	x := &Suppression{}
	if err := s.Scan(&x.ID, &x.AgentID, &x.Type, &x.OSUser, &x.Exe, &x.Target, &x.Comment, &x.CreatedAt, &x.CreatedBy); err != nil {
		return nil, err
	}
	return x, nil
}

// InsertSuppression creates a suppression.
func InsertSuppression(db Execer, x *Suppression) error {
	x.CreatedAt = Now()
	res, err := db.Exec(`INSERT INTO suppressions (agent_id, type, os_user, exe, target, comment, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		x.AgentID, x.Type, x.OSUser, x.Exe, x.Target, x.Comment, x.CreatedAt, x.CreatedBy)
	if err != nil {
		return err
	}
	x.ID, _ = res.LastInsertId()
	return nil
}

// GetSuppression returns a suppression by id.
func GetSuppression(db Execer, id int64) (*Suppression, error) {
	return scanSuppression(db.QueryRow(`SELECT `+suppressionCols+` FROM suppressions WHERE id = ?`, id))
}

// ListSuppressions returns all suppressions, newest first.
func ListSuppressions(db Execer) ([]*Suppression, error) {
	rows, err := db.Query(`SELECT ` + suppressionCols + ` FROM suppressions ORDER BY id DESC`)
	return collect(rows, err, scanSuppression)
}

// DeleteSuppression removes a suppression.
func DeleteSuppression(db Execer, id int64) error {
	return execOne(db, `DELETE FROM suppressions WHERE id = ?`, id)
}
