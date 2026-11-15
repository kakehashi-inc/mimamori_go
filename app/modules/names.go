package modules

import (
	"strings"
	"unicode"
)

// TrimName returns a name typed by a person without the white space around
// it, unless the name holds a control character anywhere (also at its
// start or end, such as a tab, a line break or U+0085): then it is returned
// as it is, so that the check that follows refuses it instead of the
// trimming taking the character away.
func TrimName(s string) string {
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return s
	}
	return strings.TrimSpace(s)
}
