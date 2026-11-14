package modules

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// A number a person writes is a decimal whole number of the digits 0-9 with
// one optional leading '-': leading zeros are decimal, every other spelling
// is refused, and a number outside the target type is out of range.
func TestParseWholeNumber(t *testing.T) {
	good := map[string]int64{
		"0": 0, "7": 7, "-5": -5, "-0": 0, "010": 10, "-010": -10, "0009": 9, "064": 64,
		"9223372036854775807": 9223372036854775807, "-9223372036854775808": -9223372036854775808,
	}
	for s, want := range good {
		if n, err := ParseWholeNumber(s, 64); err != nil || n != want {
			t.Errorf("ParseWholeNumber(%q) = %d, %v; want %d", s, n, err, want)
		}
	}
	notNumbers := []string{
		"", "-", "--5", "+3", "+", " 3", "3 ", " ", "\t3", "3\n", "0x10", "0X10", "0o7", "0b1", "1_0",
		"1e1", "1E1", "1.5", "1.", ".5", "1,000", "abc", "5x", "x5", "- 5", "-+5", "+-5",
		"\uff11",         // FULLWIDTH DIGIT ONE
		"\u0663",         // ARABIC-INDIC DIGIT THREE
		"1\u00a0",        // a no-break space after the digit
		"\x005", "5\x00", // NUL
	}
	for _, s := range notNumbers {
		if n, err := ParseWholeNumber(s, 64); !errors.Is(err, ErrNotWholeNumber) {
			t.Errorf("ParseWholeNumber(%q) = %d, %v; want ErrNotWholeNumber", s, n, err)
		}
	}
	for _, s := range []string{"9223372036854775808", "-9223372036854775809", "99999999999999999999"} {
		if n, err := ParseWholeNumber(s, 64); !errors.Is(err, ErrWholeNumberRange) {
			t.Errorf("ParseWholeNumber(%q) = %d, %v; want ErrWholeNumberRange", s, n, err)
		}
	}
	if n, err := ParseWholeNumber("128", 8); !errors.Is(err, ErrWholeNumberRange) {
		t.Errorf("128 in 8 bits = %d, %v; want ErrWholeNumberRange", n, err)
	}
	if n, err := ParseWholeNumber("-128", 8); err != nil || n != -128 {
		t.Errorf("-128 in 8 bits = %d, %v", n, err)
	}
	if n, err := ParseWholeInt("-12"); err != nil || n != -12 {
		t.Errorf("ParseWholeInt(-12) = %d, %v", n, err)
	}
	if n, err := ParseWholeInt("0x10"); !errors.Is(err, ErrNotWholeNumber) {
		t.Errorf("ParseWholeInt(0x10) = %d, %v", n, err)
	}
	if strconv.IntSize == 64 {
		if _, err := ParseWholeInt("9223372036854775808"); !errors.Is(err, ErrWholeNumberRange) {
			t.Errorf("ParseWholeInt beyond int: %v", err)
		}
	}
}

// The integer items of the central's configuration (central config set and
// PUT /config both go through ApplyCentralConfigChanges) and the port of a
// listen address read their values by the same rule: 010 is ten, every
// other spelling is refused with the item's own message, and an integer
// item is stored as the plain decimal number.
func TestCentralConfigNumberSpelling(t *testing.T) {
	MigrationsFS = os.DirFS("../..")
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "central.db"), SchemaCentral)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	refused := []string{" 3", "3 ", "+3", "0x10", "1_0", "1e1", "1.5", "\uff13", "", "-1", "0", "99999999999999999999"}
	for _, item := range []string{"retention_days", "smtp.timeout_sec", "notify.max_lines"} {
		for _, v := range refused {
			_, err := applyConfig(db, map[string]string{item: v}, nil)
			if want := item + " must be a positive integer"; v == "" {
				want = "a value is required for " + item
				if err == nil || err.Error() != want {
					t.Errorf("%s = %q: %v, want %q", item, v, err, want)
				}
			} else if err == nil || err.Error() != want {
				t.Errorf("%s = %q: %v, want %q", item, v, err, want)
			}
		}
	}
	for _, v := range []string{"+19100", " 19100", "0x4a9c", "19_100"} {
		if _, err := applyConfig(db, map[string]string{"web_listen": "127.0.0.1:" + v}, nil); err == nil || err.Error() != "web_listen: the port must be 1-65535" {
			t.Errorf("web_listen port %q: %v", v, err)
		}
	}
	if _, err := applyConfig(db, map[string]string{"retention_days": "010", "smtp.timeout_sec": "07", "web_listen": "127.0.0.1:019100"}, nil); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCentralConfig(db.Read())
	if err != nil {
		t.Fatal(err)
	}
	if c.RetentionDays != 10 || c.SMTP.TimeoutSec != 7 {
		t.Fatalf("retention_days %d, smtp.timeout_sec %d; want 10 and 7", c.RetentionDays, c.SMTP.TimeoutSec)
	}
	f, _ := FindConfigField("retention_days")
	if got := f.get(c); got != "10" {
		t.Fatalf("retention_days stored as %q", got)
	}
}
