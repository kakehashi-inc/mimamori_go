package models

import "database/sql"

// Roles of an account.
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// Display languages of the browser screens. English is the default.
const (
	LangEnglish  = "en"
	LangJapanese = "ja"
)

// ValidLanguage reports whether lang is a supported display language.
func ValidLanguage(lang string) bool {
	return lang == LangEnglish || lang == LangJapanese
}

// Account is a row of the accounts table (a person using the browser or CLI).
type Account struct {
	ID           int64
	Name         string
	PasswordHash string
	Role         string
	Language     string
	Disabled     bool
	FailedLogins int
	LockedUntil  int64
	CreatedAt    int64
	UpdatedAt    int64
}

const accountCols = `id, name, password_hash, role, language, disabled, failed_logins, locked_until, created_at, updated_at`

func scanAccount(s rowScanner) (*Account, error) {
	a := &Account{}
	var disabled int
	if err := s.Scan(&a.ID, &a.Name, &a.PasswordHash, &a.Role, &a.Language, &disabled, &a.FailedLogins, &a.LockedUntil, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	a.Disabled = disabled != 0
	return a, nil
}

// InsertAccount creates an account.
func InsertAccount(db Execer, a *Account) error {
	now := Now()
	if a.Language == "" {
		a.Language = LangEnglish
	}
	res, err := db.Exec(`INSERT INTO accounts (name, password_hash, role, language, disabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.Name, a.PasswordHash, a.Role, a.Language, boolToInt(a.Disabled), now, now)
	if err != nil {
		return err
	}
	a.ID, _ = res.LastInsertId()
	a.CreatedAt, a.UpdatedAt = now, now
	return nil
}

// GetAccount returns the account by id.
func GetAccount(db Execer, id int64) (*Account, error) {
	return scanAccount(db.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE id = ?`, id))
}

// GetAccountByName returns the account by name.
func GetAccountByName(db Execer, name string) (*Account, error) {
	return scanAccount(db.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE name = ?`, name))
}

// ListAccounts returns all accounts ordered by name.
func ListAccounts(db Execer) ([]*Account, error) {
	rows, err := db.Query(`SELECT ` + accountCols + ` FROM accounts ORDER BY name`)
	return collect(rows, err, scanAccount)
}

// CountActiveAdmins counts enabled admin accounts.
func CountActiveAdmins(db Execer) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	return n, err
}

// CountAdmins counts admin accounts including disabled ones.
func CountAdmins(db Execer) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE role = 'admin'`).Scan(&n)
	return n, err
}

// UpdateAccountRole changes the role.
func UpdateAccountRole(db Execer, id int64, role string) error {
	return execOne(db, `UPDATE accounts SET role = ?, updated_at = ? WHERE id = ?`, role, Now(), id)
}

// UpdateAccountLanguage changes the display language.
func UpdateAccountLanguage(db Execer, id int64, lang string) error {
	return execOne(db, `UPDATE accounts SET language = ?, updated_at = ? WHERE id = ?`, lang, Now(), id)
}

// UpdateAccountPassword sets a new password hash and clears the lockout.
func UpdateAccountPassword(db Execer, id int64, hash string) error {
	return execOne(db, `UPDATE accounts SET password_hash = ?, failed_logins = 0, locked_until = 0, updated_at = ? WHERE id = ?`, hash, Now(), id)
}

// SetAccountDisabled enables or disables an account.
func SetAccountDisabled(db Execer, id int64, disabled bool) error {
	return execOne(db, `UPDATE accounts SET disabled = ?, updated_at = ? WHERE id = ?`, boolToInt(disabled), Now(), id)
}

// RecordLoginFailure increments the failure counter and sets the lock.
func RecordLoginFailure(db Execer, id int64, failed int, lockedUntil int64) error {
	_, err := db.Exec(`UPDATE accounts SET failed_logins = ?, locked_until = ? WHERE id = ?`, failed, lockedUntil, id)
	return err
}

// ResetLoginFailures clears the failure counter after a successful login.
func ResetLoginFailures(db Execer, id int64) error {
	_, err := db.Exec(`UPDATE accounts SET failed_logins = 0, locked_until = 0 WHERE id = ?`, id)
	return err
}

// DeleteAccount removes an account (sessions and CLI credentials cascade).
func DeleteAccount(db Execer, id int64) error {
	return execOne(db, `DELETE FROM accounts WHERE id = ?`, id)
}

// execOne runs a statement that must affect exactly one row.
func execOne(db Execer, q string, args ...any) error {
	res, err := db.Exec(q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
