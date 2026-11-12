package models

import "database/sql"

// Roles of an account.
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// Display language settings of the browser screens. "auto" (the default)
// follows the language of the browser.
const (
	LangAuto     = "auto"
	LangEnglish  = "en"
	LangJapanese = "ja"
)

// ValidLanguage reports whether lang is a supported display language setting.
func ValidLanguage(lang string) bool {
	return lang == LangAuto || lang == LangEnglish || lang == LangJapanese
}

// Account is a row of the accounts table (a person using the browser screens).
type Account struct {
	ID int64
	// Email is the login ID (unique, compared without regard to case).
	Email string
	// DisplayName is the name shown on the screens (duplicates allowed).
	DisplayName  string
	PasswordHash string
	Role         string
	Language     string
	Disabled     bool
	// Notify marks the account as a recipient of notification mails (sent
	// only while the account is enabled).
	Notify       bool
	FailedLogins int
	LockedUntil  int64
	CreatedAt    int64
	UpdatedAt    int64
}

const accountCols = `id, email, display_name, password_hash, role, language, disabled, notify, failed_logins, locked_until, created_at, updated_at`

func scanAccount(s rowScanner) (*Account, error) {
	a := &Account{}
	var disabled, notify int
	if err := s.Scan(&a.ID, &a.Email, &a.DisplayName, &a.PasswordHash, &a.Role, &a.Language, &disabled, &notify, &a.FailedLogins, &a.LockedUntil, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	a.Disabled = disabled != 0
	a.Notify = notify != 0
	return a, nil
}

// InsertAccount creates an account.
func InsertAccount(db Execer, a *Account) error {
	now := Now()
	if a.Language == "" {
		a.Language = LangAuto
	}
	res, err := db.Exec(`INSERT INTO accounts (email, display_name, password_hash, role, language, disabled, notify, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Email, a.DisplayName, a.PasswordHash, a.Role, a.Language, boolToInt(a.Disabled), boolToInt(a.Notify), now, now)
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

// GetAccountByEmail returns the account by e-mail address (the login ID),
// compared without regard to case.
func GetAccountByEmail(db Execer, email string) (*Account, error) {
	return scanAccount(db.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE email = ?`, email))
}

// ListAccounts returns all accounts ordered by e-mail address.
func ListAccounts(db Execer) ([]*Account, error) {
	rows, err := db.Query(`SELECT ` + accountCols + ` FROM accounts ORDER BY email`)
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

// UpdateAccountEmail changes the e-mail address (the login ID).
func UpdateAccountEmail(db Execer, id int64, email string) error {
	return execOne(db, `UPDATE accounts SET email = ?, updated_at = ? WHERE id = ?`, email, Now(), id)
}

// UpdateAccountDisplayName changes the display name.
func UpdateAccountDisplayName(db Execer, id int64, name string) error {
	return execOne(db, `UPDATE accounts SET display_name = ?, updated_at = ? WHERE id = ?`, name, Now(), id)
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

// SetAccountNotify turns the receipt of notification mails on or off.
func SetAccountNotify(db Execer, id int64, notify bool) error {
	return execOne(db, `UPDATE accounts SET notify = ?, updated_at = ? WHERE id = ?`, boolToInt(notify), Now(), id)
}

// ListNotifyEmails returns the e-mail addresses of the enabled accounts that
// receive notification mails, ordered by address.
func ListNotifyEmails(db Execer) ([]string, error) {
	rows, err := db.Query(`SELECT email FROM accounts WHERE notify = 1 AND disabled = 0 ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordLoginFailure counts one failed login in a single statement (no
// read-modify-write, so concurrent failures are all counted). When the count
// reaches maxFailures, the counter is reset and the account is locked until
// lockUntil (an existing later lock is kept); locked reports that this call
// set the lock. A lock is never cleared here.
func RecordLoginFailure(db Execer, id int64, maxFailures int, lockUntil int64) (locked bool, err error) {
	var failed int
	err = db.QueryRow(`UPDATE accounts SET
		locked_until = CASE WHEN failed_logins + 1 >= ? THEN MAX(locked_until, ?) ELSE locked_until END,
		failed_logins = CASE WHEN failed_logins + 1 >= ? THEN 0 ELSE failed_logins + 1 END
		WHERE id = ? RETURNING failed_logins`, maxFailures, lockUntil, maxFailures, id).Scan(&failed)
	return err == nil && failed == 0, err
}

// ResetLoginFailures clears the failure counter after a successful login.
func ResetLoginFailures(db Execer, id int64) error {
	_, err := db.Exec(`UPDATE accounts SET failed_logins = 0, locked_until = 0 WHERE id = ?`, id)
	return err
}

// DeleteAccount removes an account (its sessions, codes and browser
// verifications cascade).
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
