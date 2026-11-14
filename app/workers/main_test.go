package workers

import (
	"os"
	"testing"

	"mimamori/app/modules"
)

// testPasswordIterations is the PBKDF2 iteration count of the password
// hashes the tests of this package make.
const testPasswordIterations = 1000

// TestMain lowers the iteration count of new password hashes for the tests
// of this package. They create accounts, change passwords and log in several
// hundred times; with the program's count (modules.PasswordIterations) each
// hash costs about 0.12 s, and more than 1 s under the race detector. The
// hashes keep their format and verification reads the count from the
// stored hash, so every path runs as in the program; app/modules proves the
// program's count.
func TestMain(m *testing.M) {
	modules.UseTestPasswordIterations(testPasswordIterations)
	os.Exit(m.Run())
}
