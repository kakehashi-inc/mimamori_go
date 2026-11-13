package models

// Template is a row of the templates table: an administrator-defined template
// (built-in templates are part of the application, not stored here).
type Template struct {
	ID          int64
	Name        string
	Description string
	Content     string
	CreatedAt   int64
	UpdatedAt   int64
	CreatedBy   string
}

const templateCols = `id, name, description, content, created_at, updated_at, created_by`

func scanTemplate(s rowScanner) (*Template, error) {
	t := &Template{}
	if err := s.Scan(&t.ID, &t.Name, &t.Description, &t.Content, &t.CreatedAt, &t.UpdatedAt, &t.CreatedBy); err != nil {
		return nil, err
	}
	return t, nil
}

// InsertTemplate creates a template.
func InsertTemplate(db Writer, t *Template) error {
	now := Now()
	res, err := db.Exec(`INSERT INTO templates (name, description, content, created_at, updated_at, created_by) VALUES (?, ?, ?, ?, ?, ?)`,
		t.Name, t.Description, t.Content, now, now, t.CreatedBy)
	if err != nil {
		return err
	}
	t.ID, _ = res.LastInsertId()
	t.CreatedAt, t.UpdatedAt = now, now
	return nil
}

// GetTemplateByName returns a template by name.
func GetTemplateByName(db Reader, name string) (*Template, error) {
	return scanTemplate(db.QueryRow(`SELECT `+templateCols+` FROM templates WHERE name = ?`, name))
}

// ListTemplates returns all templates ordered by name.
func ListTemplates(db Reader) ([]*Template, error) {
	rows, err := db.Query(`SELECT ` + templateCols + ` FROM templates ORDER BY name`)
	return collect(rows, err, scanTemplate)
}

// UpdateTemplate changes description and content.
func UpdateTemplate(db Writer, name, description, content string) error {
	return execOne(db, `UPDATE templates SET description = ?, content = ?, updated_at = ? WHERE name = ?`, description, content, Now(), name)
}

// DeleteTemplate removes a template.
func DeleteTemplate(db Writer, name string) error {
	return execOne(db, `DELETE FROM templates WHERE name = ?`, name)
}
