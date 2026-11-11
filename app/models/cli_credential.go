package models

// CLICredential is a row of the cli_credentials table. Only the hash of the
// credential value is stored. An account has at most one credential.
type CLICredential struct {
	ID         int64
	AccountID  int64
	Hash       string
	CreatedAt  int64
	LastUsedAt int64
}

// ReplaceCLICredential stores a new credential for the account, replacing
// (and so invalidating) the previous one.
func ReplaceCLICredential(db Execer, accountID int64, hash string) error {
	if _, err := db.Exec(`DELETE FROM cli_credentials WHERE account_id = ?`, accountID); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT INTO cli_credentials (account_id, hash, created_at) VALUES (?, ?, ?)`, accountID, hash, Now())
	return err
}

// GetCLICredentialByHash finds a credential by the hash of its value.
func GetCLICredentialByHash(db Execer, hash string) (*CLICredential, error) {
	c := &CLICredential{}
	err := db.QueryRow(`SELECT id, account_id, hash, created_at, last_used_at FROM cli_credentials WHERE hash = ?`, hash).
		Scan(&c.ID, &c.AccountID, &c.Hash, &c.CreatedAt, &c.LastUsedAt)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// HasCLICredential reports whether the account has a credential.
func HasCLICredential(db Execer, accountID int64) bool {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM cli_credentials WHERE account_id = ?`, accountID).Scan(&n)
	return n > 0
}

// TouchCLICredential records the last use.
func TouchCLICredential(db Execer, id, now int64) error {
	_, err := db.Exec(`UPDATE cli_credentials SET last_used_at = ? WHERE id = ?`, now, id)
	return err
}

// DeleteCLICredentials removes the account's credential.
func DeleteCLICredentials(db Execer, accountID int64) error {
	_, err := db.Exec(`DELETE FROM cli_credentials WHERE account_id = ?`, accountID)
	return err
}
