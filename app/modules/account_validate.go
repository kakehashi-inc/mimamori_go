package modules

import (
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxEmailLength and MaxDisplayNameLength bound the account fields (the
// display name is counted in characters).
const (
	MaxEmailLength       = 254
	MaxDisplayNameLength = 64
)

// ValidateEmail checks an e-mail address used as the login ID of an account
// and returns it without surrounding white space. The browser API, ctl and
// the central commands all use this check. The address must be a single
// plain address (no display name, no quoting): net/mail must read it back
// unchanged. A dot in the domain is not required (user@localhost is valid).
func ValidateEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", M("account.email_required")
	}
	if len(s) > MaxEmailLength {
		return "", M("account.email_too_long", "max", MaxEmailLength)
	}
	if !utf8.ValidString(s) || strings.ContainsAny(s, "\"<>,") ||
		strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", M("account.email_chars")
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != s {
		return "", M("account.email_invalid")
	}
	return s, nil
}

// ValidateDisplayName checks the display name of an account and returns it
// without surrounding white space: 1 to 64 characters, no control
// characters. Display names need not be unique.
func ValidateDisplayName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", M("account.display_name_required")
	}
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > MaxDisplayNameLength {
		return "", M("account.display_name_too_long", "max", MaxDisplayNameLength)
	}
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return "", M("account.display_name_control")
	}
	return s, nil
}
