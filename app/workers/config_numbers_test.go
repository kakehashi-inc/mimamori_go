package workers

import (
	"strings"
	"testing"
)

// PUT /config reads the value of an integer item as a whole number of the
// digits 0-9 (leading zeros are decimal): any other spelling is answered
// 400 with the item's own message and nothing is saved.
func TestPutConfigNumberSpelling(t *testing.T) {
	e := newTestEnv(t)
	for _, v := range []string{"+3", " 3", "3 ", "0x10", "1_0", "1e1", "1.5", "\uff13", "-1"} {
		code, body := e.adminRaw("PUT", "/config", map[string]any{"set": map[string]string{"retention_days": v}})
		if code != 400 || !strings.Contains(body, "retention_days must be a positive integer") {
			t.Errorf("retention_days %q: %d %s", v, code, body)
		}
	}
	if days := e.c.conf().RetentionDays; days != 180 {
		t.Fatalf("retention_days in use after refused changes: %d", days)
	}
	e.admin("PUT", "/config", map[string]any{"set": map[string]string{"retention_days": "090"}})
	if days := e.c.conf().RetentionDays; days != 90 {
		t.Fatalf("retention_days 090 in use as %d, want 90", days)
	}
}
