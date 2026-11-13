package models

// Notification is a row of the notifications table: a composed mail kept
// until it is delivered, so a failed send is retried later.
type Notification struct {
	ID            int64
	CreatedAt     int64
	Subject       string
	Body          string
	Attempts      int
	LastError     string
	LastAttemptAt int64
	SentAt        int64
}

const notificationCols = `id, created_at, subject, body, attempts, last_error, last_attempt_at, sent_at`

func scanNotification(s rowScanner) (*Notification, error) {
	n := &Notification{}
	if err := s.Scan(&n.ID, &n.CreatedAt, &n.Subject, &n.Body, &n.Attempts, &n.LastError, &n.LastAttemptAt, &n.SentAt); err != nil {
		return nil, err
	}
	return n, nil
}

// InsertNotification stores a composed mail.
func InsertNotification(db Writer, n *Notification) error {
	n.CreatedAt = Now()
	res, err := db.Exec(`INSERT INTO notifications (created_at, subject, body) VALUES (?, ?, ?)`, n.CreatedAt, n.Subject, n.Body)
	if err != nil {
		return err
	}
	n.ID, _ = res.LastInsertId()
	return nil
}

// ListUnsentNotifications returns mails not delivered yet, oldest first.
func ListUnsentNotifications(db Reader) ([]*Notification, error) {
	rows, err := db.Query(`SELECT ` + notificationCols + ` FROM notifications WHERE sent_at = 0 ORDER BY id`)
	return collect(rows, err, scanNotification)
}

// MarkNotificationAttempt records a delivery attempt.
func MarkNotificationAttempt(db Writer, id int64, sentAt int64, errMsg string) error {
	_, err := db.Exec(`UPDATE notifications SET attempts = attempts + 1, last_attempt_at = ?, last_error = ?, sent_at = ? WHERE id = ?`,
		Now(), errMsg, sentAt, id)
	return err
}

// DeleteSentNotificationsBefore removes delivered mails older than t.
func DeleteSentNotificationsBefore(db Writer, t int64) error {
	_, err := db.Exec(`DELETE FROM notifications WHERE sent_at > 0 AND sent_at < ?`, t)
	return err
}
