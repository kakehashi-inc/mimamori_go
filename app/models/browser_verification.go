package models

// BrowserVerification is a row of the browser_verifications table: a browser
// in which an account entered a login code. The id column holds the hash of
// the cookie value, never the value itself. Whether a row still lets the
// account log in without a code is decided by the caller from Trusted and
// CreatedAt; ExpiresAt is the time after which the row is of no use and is
// removed.
type BrowserVerification struct {
	ID         string
	AccountID  int64
	Trusted    bool // "trust this browser" was chosen
	CreatedAt  int64
	ExpiresAt  int64
	RemoteAddr string
}

// InsertBrowserVerification stores a verification.
func InsertBrowserVerification(db Writer, v *BrowserVerification) error {
	_, err := db.Exec(`INSERT INTO browser_verifications (id, account_id, trusted, created_at, expires_at, remote_addr) VALUES (?, ?, ?, ?, ?, ?)`,
		v.ID, v.AccountID, boolToInt(v.Trusted), v.CreatedAt, v.ExpiresAt, v.RemoteAddr)
	return err
}

// GetBrowserVerification returns the verification by hashed id.
func GetBrowserVerification(db Reader, id string) (*BrowserVerification, error) {
	v := &BrowserVerification{}
	var trusted int
	err := db.QueryRow(`SELECT id, account_id, trusted, created_at, expires_at, remote_addr FROM browser_verifications WHERE id = ?`, id).
		Scan(&v.ID, &v.AccountID, &trusted, &v.CreatedAt, &v.ExpiresAt, &v.RemoteAddr)
	if err != nil {
		return nil, err
	}
	v.Trusted = trusted != 0
	return v, nil
}

// DeleteBrowserVerification removes one verification.
func DeleteBrowserVerification(db Writer, id string) error {
	_, err := db.Exec(`DELETE FROM browser_verifications WHERE id = ?`, id)
	return err
}

// DeleteAccountBrowserVerifications removes every verification of an account.
func DeleteAccountBrowserVerifications(db Writer, accountID int64) error {
	_, err := db.Exec(`DELETE FROM browser_verifications WHERE account_id = ?`, accountID)
	return err
}

// DeleteExpiredBrowserVerifications removes the verifications that are of
// no use any more.
func DeleteExpiredBrowserVerifications(db Writer, now int64) error {
	_, err := db.Exec(`DELETE FROM browser_verifications WHERE expires_at <= ?`, now)
	return err
}
