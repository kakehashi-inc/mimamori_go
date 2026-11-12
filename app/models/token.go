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
func InsertToken(db Execer, t *Token) error {
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
func GetToken(db Execer, id int64) (*Token, error) {
	return scanToken(db.QueryRow(`SELECT `+tokenCols+` FROM tokens WHERE id = ?`, id))
}

// FindTokenByHash returns the token whose current or pending value has hash.
func FindTokenByHash(db Execer, hash string) (*Token, error) {
	return scanToken(db.QueryRow(`SELECT `+tokenCols+` FROM tokens WHERE hash = ? OR (pending_hash <> '' AND pending_hash = ?)`, hash, hash))
}

// ListTokens returns all tokens ordered by name.
func ListTokens(db Execer) ([]*Token, error) {
	rows, err := db.Query(`SELECT ` + tokenCols + ` FROM tokens ORDER BY name`)
	return collect(rows, err, scanToken)
}

// SaveToken writes every mutable column of the token.
func SaveToken(db Execer, t *Token) error {
	return execOne(db, `UPDATE tokens SET hash = ?, value = ?, expires_at = ?, pending_hash = ?, pending_expires_at = ?, renew_counter = ?,
		instance_id = ?, bound_at = ?, used_at = ?, last_seen_at = ?, revoked_at = ?, reissued_at = ?, renewed_at = ? WHERE id = ?`,
		t.Hash, t.Value, t.ExpiresAt, t.PendingHash, t.PendingExpiresAt, t.RenewCounter, t.InstanceID, t.BoundAt, t.UsedAt,
		t.LastSeenAt, t.RevokedAt, t.ReissuedAt, t.RenewedAt, t.ID)
}
