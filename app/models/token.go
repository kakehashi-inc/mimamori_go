package models

// Token statuses (derived, not stored).
const (
	TokenUnused  = "unused"
	TokenActive  = "active"
	TokenExpired = "expired"
	TokenRevoked = "revoked"
)

// Token is a row of the tokens table: the credential of one agent. Value is
// the value in effect (shown in full only to the management CLI); agents are
// looked up by its SHA-256 hash. PendingHash is the hash of the value handed
// out by automatic renewal and not yet used by the agent.
type Token struct {
	ID               int64
	Name             string
	Hash             string
	Value            string
	ExpiresAt        int64
	PendingHash      string
	PendingExpiresAt int64
	RenewCounter     int
	InstanceID       string
	BoundAt          int64
	UsedAt           int64
	LastSeenAt       int64
	RevokedAt        int64
	ReissuedAt       int64
	RenewedAt        int64
	CreatedAt        int64
	CreatedBy        string
}

const tokenCols = `id, name, hash, value, expires_at, pending_hash, pending_expires_at, renew_counter, instance_id, bound_at, used_at, last_seen_at, revoked_at, reissued_at, renewed_at, created_at, created_by`

func scanToken(s rowScanner) (*Token, error) {
	t := &Token{}
	if err := s.Scan(&t.ID, &t.Name, &t.Hash, &t.Value, &t.ExpiresAt, &t.PendingHash, &t.PendingExpiresAt, &t.RenewCounter,
		&t.InstanceID, &t.BoundAt, &t.UsedAt, &t.LastSeenAt, &t.RevokedAt, &t.ReissuedAt, &t.RenewedAt, &t.CreatedAt, &t.CreatedBy); err != nil {
		return nil, err
	}
	return t, nil
}

// Status derives the token status at time now.
func (t *Token) Status(now int64) string {
	switch {
	case t.RevokedAt > 0:
		return TokenRevoked
	case t.ExpiresAt <= now && (t.PendingHash == "" || t.PendingExpiresAt <= now):
		return TokenExpired
	case t.UsedAt == 0:
		return TokenUnused
	default:
		return TokenActive
	}
}

// InsertToken creates a token.
func InsertToken(db Writer, t *Token) error {
	t.CreatedAt = Now()
	res, err := db.Exec(`INSERT INTO tokens (name, hash, value, expires_at, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?)`,
		t.Name, t.Hash, t.Value, t.ExpiresAt, t.CreatedAt, t.CreatedBy)
	if err != nil {
		return err
	}
	t.ID, _ = res.LastInsertId()
	return nil
}

// GetToken returns the token by id.
func GetToken(db Reader, id int64) (*Token, error) {
	return scanToken(db.QueryRow(`SELECT `+tokenCols+` FROM tokens WHERE id = ?`, id))
}

// FindTokenByHash returns the token whose current or pending value has hash.
func FindTokenByHash(db Reader, hash string) (*Token, error) {
	return scanToken(db.QueryRow(`SELECT `+tokenCols+` FROM tokens WHERE hash = ? OR (pending_hash <> '' AND pending_hash = ?)`, hash, hash))
}

// ListTokens returns all tokens ordered by name.
func ListTokens(db Reader) ([]*Token, error) {
	rows, err := db.Query(`SELECT ` + tokenCols + ` FROM tokens ORDER BY name`)
	return collect(rows, err, scanToken)
}

// The administrator operations below write only their own columns, so that
// they never write back values an agent request changed at the same time.

// ReissueToken stores a new value of the token (t.Hash, t.Value,
// t.ExpiresAt, t.ReissuedAt): the pending renewal is dropped, and the token
// is unused and not revoked again. The binding to the agent instance stays.
func ReissueToken(db Writer, t *Token) error {
	return execOne(db, `UPDATE tokens SET hash = ?, value = ?, expires_at = ?, pending_hash = '', pending_expires_at = 0,
		used_at = 0, revoked_at = 0, reissued_at = ? WHERE id = ?`,
		t.Hash, t.Value, t.ExpiresAt, t.ReissuedAt, t.ID)
}

// RevokeToken marks the token as revoked at now and drops its pending
// renewal.
func RevokeToken(db Writer, id, now int64) error {
	return execOne(db, `UPDATE tokens SET revoked_at = ?, pending_hash = '', pending_expires_at = 0 WHERE id = ?`, now, id)
}

// UnbindToken removes the binding of the token to an agent instance.
func UnbindToken(db Writer, id int64) error {
	return execOne(db, `UPDATE tokens SET instance_id = '', bound_at = 0 WHERE id = ?`, id)
}

// DropExpiredTokenRenewal drops the pending renewal of the token when it was
// not used before its expiry (it is left alone when it changed meanwhile).
func DropExpiredTokenRenewal(db Writer, id, now int64) error {
	_, err := db.Exec(`UPDATE tokens SET pending_hash = '', pending_expires_at = 0
		WHERE id = ? AND pending_hash <> '' AND pending_expires_at <= ?`, id, now)
	return err
}

// RecordTokenUse stores what one authenticated agent request changes on its
// token: the switch to the renewed value (when the agent used it), the
// binding to the agent instance and the usage times. Only these columns are
// written, and only while the row still holds the values the request was
// checked against (prev) and is not revoked, so a revoke, reissue or unbind
// done at the same time is never undone. It returns sql.ErrNoRows when the
// token changed in the meantime.
func RecordTokenUse(db Writer, t, prev *Token) error {
	return execOne(db, `UPDATE tokens SET hash = ?, value = ?, expires_at = ?, pending_hash = ?, pending_expires_at = ?, renewed_at = ?,
		instance_id = ?, bound_at = ?, used_at = ?, last_seen_at = ?
		WHERE id = ? AND revoked_at = 0 AND hash = ? AND pending_hash = ? AND instance_id = ?`,
		t.Hash, t.Value, t.ExpiresAt, t.PendingHash, t.PendingExpiresAt, t.RenewedAt, t.InstanceID, t.BoundAt, t.UsedAt, t.LastSeenAt,
		t.ID, prev.Hash, prev.PendingHash, prev.InstanceID)
}

// SetTokenRenewal stores a new renewal value (its hash, expiry and counter)
// of the token, only while the token is not revoked and still holds the
// current value, pending value and counter it was read with. It returns
// sql.ErrNoRows when the token changed in the meantime.
func SetTokenRenewal(db Writer, t *Token, prevPendingHash string, prevCounter int) error {
	return execOne(db, `UPDATE tokens SET renew_counter = ?, pending_hash = ?, pending_expires_at = ?
		WHERE id = ? AND revoked_at = 0 AND hash = ? AND pending_hash = ? AND renew_counter = ?`,
		t.RenewCounter, t.PendingHash, t.PendingExpiresAt, t.ID, t.Hash, prevPendingHash, prevCounter)
}

// DeleteToken removes a token; its agent row, if any, is removed with it
// (ON DELETE CASCADE).
func DeleteToken(db Writer, id int64) error {
	return execOne(db, `DELETE FROM tokens WHERE id = ?`, id)
}
