package models

// Rule is a row of the rules table (a monitoring rule; contents are versioned
// in rule_versions).
type Rule struct {
	ID            int64
	Name          string
	Description   string
	LatestVersion int
	CreatedAt     int64
	UpdatedAt     int64
	CreatedBy     string
}

const ruleCols = `id, name, description, latest_version, created_at, updated_at, created_by`

func scanRule(s rowScanner) (*Rule, error) {
	r := &Rule{}
	if err := s.Scan(&r.ID, &r.Name, &r.Description, &r.LatestVersion, &r.CreatedAt, &r.UpdatedAt, &r.CreatedBy); err != nil {
		return nil, err
	}
	return r, nil
}

// InsertRule creates a rule row with latest_version 1.
func InsertRule(db Writer, r *Rule) error {
	now := Now()
	res, err := db.Exec(`INSERT INTO rules (name, description, latest_version, created_at, updated_at, created_by) VALUES (?, ?, 1, ?, ?, ?)`,
		r.Name, r.Description, now, now, r.CreatedBy)
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	r.LatestVersion, r.CreatedAt, r.UpdatedAt = 1, now, now
	return nil
}

// GetRule returns the rule by id.
func GetRule(db Reader, id int64) (*Rule, error) {
	return scanRule(db.QueryRow(`SELECT `+ruleCols+` FROM rules WHERE id = ?`, id))
}

// ListRules returns all rules ordered by name.
func ListRules(db Reader) ([]*Rule, error) {
	rows, err := db.Query(`SELECT ` + ruleCols + ` FROM rules ORDER BY name`)
	return collect(rows, err, scanRule)
}

// BumpRuleVersion sets the latest version and description.
func BumpRuleVersion(db Writer, id int64, version int, description string) error {
	return execOne(db, `UPDATE rules SET latest_version = ?, description = ?, updated_at = ? WHERE id = ?`, version, description, Now(), id)
}

// DeleteRule removes a rule (versions cascade; assigned rules are protected
// by the foreign key from agents).
func DeleteRule(db Writer, id int64) error {
	return execOne(db, `DELETE FROM rules WHERE id = ?`, id)
}
