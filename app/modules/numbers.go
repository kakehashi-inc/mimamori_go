package modules

import (
	"errors"
	"strconv"
	"strings"
)

// Errors of ParseWholeNumber.
var (
	// ErrNotWholeNumber: the text is not written as a whole number.
	ErrNotWholeNumber = errors.New("not a whole number")
	// ErrWholeNumberRange: the number does not fit the integer type.
	ErrWholeNumberRange = errors.New("out of range")
)

// ParseWholeNumber reads a number that a person wrote or a client sent: an
// argument or a flag value of the command line, the value of an integer item
// of central config set or PUT /config, the port of a listen address, the
// count of agent set-workers, an id in an API path or an integer in an API
// query. The only spelling is a decimal whole number of the digits 0-9
// with one optional leading '-'. A '+', a space around it, a fraction or an
// exponent, a base prefix (0x, 0o, 0b), a '_' and every other character
// make the text no number; leading zeros are decimal (010 is ten). bitSize
// is the size of the integer type the number is for (64 for int64,
// strconv.IntSize for int); a number outside it is ErrWholeNumberRange, any
// other spelling ErrNotWholeNumber.
func ParseWholeNumber(s string, bitSize int) (int64, error) {
	if !decimalDigits(strings.TrimPrefix(s, "-")) {
		return 0, ErrNotWholeNumber
	}
	// The text is decimal digits after an optional '-': only the range can
	// fail here.
	n, err := strconv.ParseInt(s, 10, bitSize)
	if err != nil {
		return 0, ErrWholeNumberRange
	}
	return n, nil
}

// ParseWholeInt is ParseWholeNumber for an int.
func ParseWholeInt(s string) (int, error) {
	n, err := ParseWholeNumber(s, strconv.IntSize)
	return int(n), err
}

// decimalDigits reports whether s is one or more of the digits 0-9 and
// nothing else.
func decimalDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}
