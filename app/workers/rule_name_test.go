package workers

import (
	"net/http"
	"strings"
	"testing"
)

// A rule name is 1 to 100 characters without any control character
// (unicode.IsControl, as display names): C0, DEL and C1 are refused alike
// with their own message, everything else is kept.
func TestRuleNameRefusesControlCharacters(t *testing.T) {
	e := newTestEnv(t)
	for i, name := range []string{"tab\tname", "bad\x00name", "bad\u007fname", "bad\u0085name", "bad\u009bname", "line\nbreak"} {
		rec := e.cli("POST", "/rules", map[string]any{"name": name})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("name %d %q: %d %s, want 400", i, name, rec.Code, rec.Body.String())
			continue
		}
		checkErrorBody(t, name, rec.Body.Bytes(), "rule.name_control")
	}
	for _, name := range []string{"", "   ", strings.Repeat("x", 101), strings.Repeat("\u3042", 101)} {
		rec := e.cli("POST", "/rules", map[string]any{"name": name})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("name of %d characters: %d, want 400", len([]rune(name)), rec.Code)
			continue
		}
		checkErrorBody(t, "length", rec.Body.Bytes(), "rule.name_invalid")
	}
	for _, name := range []string{"web servers", strings.Repeat("\u3042", 100), "a\u200bb", "x"} {
		if rec := e.cli("POST", "/rules", map[string]any{"name": name}); rec.Code != http.StatusOK {
			t.Errorf("name %q: %d %s, want 200", name, rec.Code, rec.Body.String())
		}
	}
}
