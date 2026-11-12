package models

import "database/sql"

// LocalState is a row of the state table of the agent's local database:
// key/value state such as the applied rule and scan watermarks.
type LocalState struct {
	Key   string
	Value string
}

// GetLocalState returns the value of key ("" when absent).
func GetLocalState(db Execer, key string) string {
	var v string
	if err := db.QueryRow(`SELECT value FROM state WHERE key = ?`, key).Scan(&v); err != nil && err != sql.ErrNoRows {
		return ""
	}
	return v
}

// SetLocalState upserts a value.
func SetLocalState(db Execer, key, value string) error {
	_, err := db.Exec(`INSERT INTO state (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// DeleteLocalState removes the keys.
func DeleteLocalState(db Execer, keys ...string) error {
	for _, k := range keys {
		if _, err := db.Exec(`DELETE FROM state WHERE key = ?`, k); err != nil {
			return err
		}
	}
	return nil
}
