package models

import "database/sql"

// LocalState is a row of the state table of the agent's local database:
// key/value state such as the applied rule and scan watermarks.
type LocalState struct {
	Key   string
	Value string
}

// GetLocalState returns the value of key ("" when absent).
func GetLocalState(db Reader, key string) string {
	var v string
	if err := db.QueryRow(`SELECT value FROM state WHERE key = ?`, key).Scan(&v); err != nil && err != sql.ErrNoRows {
		return ""
	}
	return v
}

// FindLocalState returns the value of key and whether the row exists; unlike
// GetLocalState it tells a missing row from an empty value and returns the
// error of the read.
func FindLocalState(db Reader, key string) (value string, found bool, err error) {
	err = db.QueryRow(`SELECT value FROM state WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetLocalState upserts a value.
func SetLocalState(db Writer, key, value string) error {
	_, err := db.Exec(`INSERT INTO state (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// DeleteLocalState removes the keys.
func DeleteLocalState(db Writer, keys ...string) error {
	for _, k := range keys {
		if _, err := db.Exec(`DELETE FROM state WHERE key = ?`, k); err != nil {
			return err
		}
	}
	return nil
}
