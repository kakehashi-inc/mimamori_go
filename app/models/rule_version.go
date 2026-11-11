package models

// RuleVersion is a row of the rule_versions table: the content of one saved
// version of a rule (JSON of modules.RuleContent).
type RuleVersion struct {
	ID        int64
	RuleID    int64
	Version   int
	Content   string
	Comment   string
	CreatedAt int64
	CreatedBy string
}

const ruleVersionCols = `id, rule_id, version, content, comment, created_at, created_by`

func scanRuleVersion(s rowScanner) (*RuleVersion, error) {
	v := &RuleVersion{}
	if err := s.Scan(&v.ID, &v.RuleID, &v.Version, &v.Content, &v.Comment, &v.CreatedAt, &v.CreatedBy); err != nil {
		return nil, err
	}
	return v, nil
}

// InsertRuleVersion stores a version.
func InsertRuleVersion(db Execer, v *RuleVersion) error {
	v.CreatedAt = Now()
	res, err := db.Exec(`INSERT INTO rule_versions (rule_id, version, content, comment, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?)`,
		v.RuleID, v.Version, v.Content, v.Comment, v.CreatedAt, v.CreatedBy)
	if err != nil {
		return err
	}
	v.ID, _ = res.LastInsertId()
	return nil
}

// GetRuleVersion returns one version.
func GetRuleVersion(db Execer, ruleID int64, version int) (*RuleVersion, error) {
	return scanRuleVersion(db.QueryRow(`SELECT `+ruleVersionCols+` FROM rule_versions WHERE rule_id = ? AND version = ?`, ruleID, version))
}

// ListRuleVersions returns all versions of a rule, newest first.
func ListRuleVersions(db Execer, ruleID int64) ([]*RuleVersion, error) {
	rows, err := db.Query(`SELECT `+ruleVersionCols+` FROM rule_versions WHERE rule_id = ? ORDER BY version DESC`, ruleID)
	return collect(rows, err, scanRuleVersion)
}
