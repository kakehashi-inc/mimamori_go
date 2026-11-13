package models

// DBInstance is the single row of the db_instance table of a per-target
// database: a random value created with the database. It goes into the
// signature of the command set definitions, so that an agent notices a
// recreated database even when set IDs and revisions repeat.
type DBInstance struct {
	ID        int64
	Value     string
	CreatedAt int64
}

// GetDBInstance returns the random value of the database.
func GetDBInstance(db Reader) (string, error) {
	var v string
	err := db.QueryRow(`SELECT value FROM db_instance WHERE id = 1`).Scan(&v)
	return v, err
}
