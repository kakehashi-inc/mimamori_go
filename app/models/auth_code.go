package models

// Purposes of a verification code.
const (
	CodePurposeLogin       = "login"
	CodePurposeEmailChange = "email_change"
	CodePurposeStepUp      = "step_up"
)

// AuthCode is a row of the auth_codes table: a verification code mailed to
// an account and not yet entered. The code itself is never stored, only its
// hash. A login code is found by its challenge (stored hashed); an e-mail
// address change code and a re-authentication (step-up) code by the session
// that asked for it.
type AuthCode struct {
	ID            int64
	Purpose       string
	AccountID     int64
	ChallengeHash string // login: hash of the challenge given to the browser
	SessionID     string // email_change, step_up: hashed id of the requesting session
	NewEmail      string // email_change: the requested address
	CodeHash      string
	Remember      bool // login: "keep me logged in" was chosen
	Attempts      int
	RemoteAddr    string
	CreatedAt     int64
	ExpiresAt     int64
}

const authCodeCols = `id, purpose, account_id, challenge_hash, session_id, new_email, code_hash, remember, attempts, remote_addr, created_at, expires_at`

func scanAuthCode(s rowScanner) (*AuthCode, error) {
	a := &AuthCode{}
	var remember int
	if err := s.Scan(&a.ID, &a.Purpose, &a.AccountID, &a.ChallengeHash, &a.SessionID, &a.NewEmail, &a.CodeHash, &remember, &a.Attempts,
		&a.RemoteAddr, &a.CreatedAt, &a.ExpiresAt); err != nil {
		return nil, err
	}
	a.Remember = remember != 0
	return a, nil
}

// InsertAuthCode stores a code and sets its ID.
func InsertAuthCode(db Execer, a *AuthCode) error {
	res, err := db.Exec(`INSERT INTO auth_codes (purpose, account_id, challenge_hash, session_id, new_email, code_hash, remember, attempts, remote_addr, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Purpose, a.AccountID, a.ChallengeHash, a.SessionID, a.NewEmail, a.CodeHash, boolToInt(a.Remember), a.Attempts, a.RemoteAddr, a.CreatedAt, a.ExpiresAt)
	if err != nil {
		return err
	}
	a.ID, _ = res.LastInsertId()
	return nil
}

// GetAuthCode returns the code by id.
func GetAuthCode(db Execer, id int64) (*AuthCode, error) {
	return scanAuthCode(db.QueryRow(`SELECT `+authCodeCols+` FROM auth_codes WHERE id = ?`, id))
}

// GetLoginCodeByChallenge returns the login code of a challenge (hashed).
func GetLoginCodeByChallenge(db Execer, challengeHash string) (*AuthCode, error) {
	return scanAuthCode(db.QueryRow(`SELECT `+authCodeCols+` FROM auth_codes WHERE purpose = ? AND challenge_hash = ? AND challenge_hash <> ''`,
		CodePurposeLogin, challengeHash))
}

// GetEmailChangeCode returns the pending e-mail address change of an
// account requested from the given session (hashed id).
func GetEmailChangeCode(db Execer, accountID int64, sessionID string) (*AuthCode, error) {
	return GetSessionAuthCode(db, CodePurposeEmailChange, accountID, sessionID)
}

// GetSessionAuthCode returns the latest code of the purpose that the given
// session (hashed id) of an account asked for.
func GetSessionAuthCode(db Execer, purpose string, accountID int64, sessionID string) (*AuthCode, error) {
	return scanAuthCode(db.QueryRow(`SELECT `+authCodeCols+` FROM auth_codes WHERE purpose = ? AND account_id = ? AND session_id = ? AND session_id <> ''
		ORDER BY id DESC LIMIT 1`, purpose, accountID, sessionID))
}

// DeleteSessionAuthCodes removes the codes of the purpose that the given
// session (hashed id) of an account asked for.
func DeleteSessionAuthCodes(db Execer, purpose string, accountID int64, sessionID string) error {
	_, err := db.Exec(`DELETE FROM auth_codes WHERE purpose = ? AND account_id = ? AND session_id = ?`, purpose, accountID, sessionID)
	return err
}

// RecordAuthCodeAttempt counts one wrong entry of a code in a single
// statement and returns the new number of wrong entries.
func RecordAuthCodeAttempt(db Execer, id int64) (int, error) {
	var n int
	err := db.QueryRow(`UPDATE auth_codes SET attempts = attempts + 1 WHERE id = ? RETURNING attempts`, id).Scan(&n)
	return n, err
}

// ConsumeAuthCode removes a code that was entered correctly, in a single
// statement, and reports whether this call removed it. It reports false when
// the code is gone (used, replaced or discarded by a concurrent request),
// has expired or has reached maxAttempts wrong entries, so one code admits
// at most one request and concurrent guesses cannot pass the attempt limit.
func ConsumeAuthCode(db Execer, id int64, maxAttempts int, now int64) (bool, error) {
	res, err := db.Exec(`DELETE FROM auth_codes WHERE id = ? AND attempts < ? AND expires_at > ?`, id, maxAttempts, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeleteAuthCode removes one code (used, expired or failed too often).
func DeleteAuthCode(db Execer, id int64) error {
	_, err := db.Exec(`DELETE FROM auth_codes WHERE id = ?`, id)
	return err
}

// DeleteAccountAuthCodes removes the codes of an account with the given
// purpose, or every code of the account when purpose is empty.
func DeleteAccountAuthCodes(db Execer, accountID int64, purpose string) error {
	_, err := db.Exec(`DELETE FROM auth_codes WHERE account_id = ? AND (? = '' OR purpose = ?)`, accountID, purpose, purpose)
	return err
}

// DeleteExpiredAuthCodes removes the codes whose lifetime has passed.
func DeleteExpiredAuthCodes(db Execer, now int64) error {
	_, err := db.Exec(`DELETE FROM auth_codes WHERE expires_at <= ?`, now)
	return err
}
