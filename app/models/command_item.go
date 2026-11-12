package models

// CommandItem is a row of the command_items table of a per-target database:
// one command of a set, at Position (from 0). Commands have no identity:
// saving a set replaces all its rows. Patterns is a JSON array of strings.
type CommandItem struct {
	ID          int64
	SetID       int64
	Position    int
	CommandLine string
	Note        string
	SuccessKind string
	SuccessA    int
	SuccessB    int
	FailureKind string
	FailureA    int
	FailureB    int
	OnProblems  string
	TimeoutSec  int
	LineLimit   int
	Patterns    string
}

const commandItemCols = `id, set_id, position, command_line, note, success_kind, success_a, success_b, failure_kind, failure_a,
	failure_b, on_problems, timeout_sec, line_limit, patterns`

func scanCommandItem(s rowScanner) (*CommandItem, error) {
	c := &CommandItem{}
	if err := s.Scan(&c.ID, &c.SetID, &c.Position, &c.CommandLine, &c.Note, &c.SuccessKind, &c.SuccessA, &c.SuccessB,
		&c.FailureKind, &c.FailureA, &c.FailureB, &c.OnProblems, &c.TimeoutSec, &c.LineLimit, &c.Patterns); err != nil {
		return nil, err
	}
	return c, nil
}

// ReplaceCommandItems replaces all commands of a set with items, numbering
// their positions from 0 in the given order. Run it in the transaction that
// saves the set.
func ReplaceCommandItems(db Execer, setID int64, items []*CommandItem) error {
	if _, err := db.Exec(`DELETE FROM command_items WHERE set_id = ?`, setID); err != nil {
		return err
	}
	for i, c := range items {
		c.SetID, c.Position = setID, i
		if c.Patterns == "" {
			c.Patterns = "[]"
		}
		res, err := db.Exec(`INSERT INTO command_items (set_id, position, command_line, note, success_kind, success_a, success_b,
			failure_kind, failure_a, failure_b, on_problems, timeout_sec, line_limit, patterns)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.SetID, c.Position, c.CommandLine, c.Note, c.SuccessKind, c.SuccessA, c.SuccessB, c.FailureKind, c.FailureA,
			c.FailureB, c.OnProblems, c.TimeoutSec, c.LineLimit, c.Patterns)
		if err != nil {
			return err
		}
		c.ID, _ = res.LastInsertId()
	}
	return nil
}

// ListCommandItems returns the commands of a set in order.
func ListCommandItems(db Execer, setID int64) ([]*CommandItem, error) {
	rows, err := db.Query(`SELECT `+commandItemCols+` FROM command_items WHERE set_id = ? ORDER BY position`, setID)
	return collect(rows, err, scanCommandItem)
}
