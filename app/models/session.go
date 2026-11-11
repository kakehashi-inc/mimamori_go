package models

// Session is a row of the sessions table (a logged-in browser). The id column
// holds the hash of the cookie value, never the value itself. A remembered
// session ("keep me logged in") lasts a fixed period from login; any other
// session expires after a period without activity.
type Session struct {
	ID         string
	AccountID  int64
	CreatedAt  int64
	ExpiresAt  int64
	LastSeenAt int64
	Remember   bool
}

// InsertSession stores a session.
func InsertSession(db Execer, s *Session) error {
	_, err := db.Exec(`INSERT INTO sessions (id, account_id, created_at, expires_at, last_seen_at, remember) VALUES (?, ?, ?, ?, ?, ?)`,
		s.ID, s.AccountID, s.CreatedAt, s.ExpiresAt, s.LastSeenAt, boolToInt(s.Remember))
	return err
}

// GetSession returns the session by hashed id.
func GetSession(db Execer, id string) (*Session, error) {
	s := &Session{}
	var remember int
	err := db.QueryRow(`SELECT id, account_id, created_at, expires_at, last_seen_at, remember FROM sessions WHERE id = ?`, id).
		Scan(&s.ID, &s.AccountID, &s.CreatedAt, &s.ExpiresAt, &s.LastSeenAt, &remember)
	if err != nil {
		return nil, err
	}
	s.Remember = remember != 0
	return s, nil
}

// TouchSession updates the last-seen time and expiry.
func TouchSession(db Execer, id string, now, expires int64) error {
	_, err := db.Exec(`UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?`, now, expires, id)
	return err
}

// DeleteSession removes one session.
func DeleteSession(db Execer, id string) error {
	_, err := db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// DeleteAccountSessions removes every session of an account, optionally
// keeping one (the session that changed its own password).
func DeleteAccountSessions(db Execer, accountID int64, keep string) error {
	_, err := db.Exec(`DELETE FROM sessions WHERE account_id = ? AND id <> ?`, accountID, keep)
	return err
}

// DeleteExpiredSessions removes expired sessions.
func DeleteExpiredSessions(db Execer, now int64) error {
	_, err := db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now)
	return err
}
