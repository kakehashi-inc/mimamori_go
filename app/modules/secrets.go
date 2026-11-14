package modules

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// GenerateSecret returns prefix + base64url of SecretRandomBytes random bytes.
func GenerateSecret(prefix string) (string, error) {
	b := make([]byte, SecretRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashSecret returns the SHA-256 hash (hex) of a high-entropy secret value.
// Session ids are stored only as this hash; tokens are looked up by it (the
// tokens table also keeps the value in effect).
func HashSecret(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// DeriveRenewalToken derives the automatically renewed token value from the
// central server key, the token id and the renewal counter. Deriving instead
// of storing lets the central hand out the same new value again after an
// agent failed to save it, while only its hash is kept in the database until
// the agent uses it (the value is stored then).
func DeriveRenewalToken(serverKey []byte, tokenID int64, counter int) string {
	m := hmac.New(sha256.New, serverKey)
	fmt.Fprintf(m, "token-renewal:%d:%d", tokenID, counter)
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// GenerateNumericCode returns a random code of the given number of decimal
// digits (leading zeros kept).
func GenerateNumericCode(digits int) (string, error) {
	limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", digits, n), nil
}

// HashAuthCode returns the keyed hash (HMAC-SHA256 with the central server
// key, hex) under which a verification code is stored. binding ties the hash
// to the row it belongs to (purpose, account and challenge or session), so a
// stored hash is of no use for another request.
func HashAuthCode(serverKey []byte, binding, code string) string {
	m := hmac.New(sha256.New, serverKey)
	fmt.Fprintf(m, "auth-code:%s:%s", binding, code)
	return hex.EncodeToString(m.Sum(nil))
}

// HashPassword hashes a password with PBKDF2-SHA256, PasswordIterations
// iterations and a random salt.
func HashPassword(password string) (string, error) {
	iter := PasswordIterations
	if n := testPasswordIterations.Load(); n > 0 {
		iter = int(n)
	}
	return hashPassword(password, iter)
}

// hashPassword hashes a password with iter iterations in the format that
// VerifyPassword reads: pbkdf2-sha256$<iterations>$<salt>$<hash>, salt and
// hash in unpadded standard base64.
func hashPassword(password string, iter int) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iter,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// testPasswordIterations is the iteration count HashPassword uses instead
// of PasswordIterations when it is above zero. Only UseTestPasswordIterations
// sets it, and only in a test binary.
var testPasswordIterations atomic.Int64

// UseTestPasswordIterations is for tests only: it makes HashPassword use n
// iterations (n = 0: PasswordIterations again), so that tests that create
// many accounts and log in many times do not spend most of their time in
// PBKDF2. Verification reads the count from the stored hash, so such hashes
// are checked like any other. Outside a binary built by go test it panics
// and changes nothing; no code of the program calls it (the test
// TestTestHooksOnlyInTests reads the sources).
func UseTestPasswordIterations(n int) {
	setTestPasswordIterations(testing.Testing(), n)
}

// setTestPasswordIterations is UseTestPasswordIterations with the answer of
// testing.Testing() as inTest.
func setTestPasswordIterations(inTest bool, n int) {
	if !inTest {
		panic("UseTestPasswordIterations is only for tests")
	}
	if n < 0 {
		panic("UseTestPasswordIterations: negative iteration count")
	}
	testPasswordIterations.Store(int64(n))
}

// VerifyPassword checks password against a hash made by HashPassword.
func VerifyPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ValidatePassword enforces the password policy.
func ValidatePassword(p string) error {
	if len([]rune(p)) < MinPasswordLength {
		return M("account.password_too_short", "min", MinPasswordLength)
	}
	if len(p) > 256 {
		return M("account.password_too_long")
	}
	return nil
}

// ShortenSecret returns a recognizable but unusable form of a secret value:
// its prefix with the first and last four characters ("mmr_AbCd...WxYz").
// An empty value stays empty.
func ShortenSecret(v string) string {
	prefix := ""
	if i := strings.Index(v, "_"); i >= 0 && i < 8 {
		prefix, v = v[:i+1], v[i+1:]
	}
	if v == "" {
		return ""
	}
	if len(v) <= 8 {
		return prefix + "..."
	}
	return prefix + v[:4] + "..." + v[len(v)-4:]
}
