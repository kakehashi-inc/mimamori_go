package modules

import (
	"os"
	"testing"
)

// testHashIterations is the PBKDF2 iteration count of the password hashes
// the tests of this package make (central init-admin and reset-password,
// the round trip of TestSecrets).
const testHashIterations = 1000

// TestMain lowers the iteration count of new password hashes for the tests
// of this package: with the program's count (PasswordIterations) a hash
// costs about 0.12 s, and more than 1 s under the race detector. The hashes
// keep their format and verification reads the count from the stored hash.
// The tests of the program's count and of the hook (secrets_test.go) set
// the count they prove themselves.
func TestMain(m *testing.M) {
	UseTestPasswordIterations(testHashIterations)
	os.Exit(m.Run())
}
