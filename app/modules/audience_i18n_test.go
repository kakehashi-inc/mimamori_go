package modules

import (
	"encoding/json"
	"strings"
	"testing"
)

// jsonCodes returns every message code in the JSON form of v (messages,
// their locations and nested parameters).
func jsonCodes(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(any)
	walk = func(x any) {
		switch y := x.(type) {
		case map[string]any:
			if c, ok := y["code"].(string); ok && c != "" {
				out = append(out, c)
			}
			for _, sub := range y {
				walk(sub)
			}
		case []any:
			for _, sub := range y {
				walk(sub)
			}
		}
	}
	walk(doc)
	return out
}

// Texts that any account receives must not use administrator codes (those
// live only in the administrator's frontend bundle, so a viewer would see
// the English): the problems of a rule's content reach every account inside
// the application result (apply.validation_failed), and the application
// results, the built-in agent's state, the program-made version comments and
// the built-in template descriptions are answered to viewers.
func TestViewerTextsUseEveryoneCodes(t *testing.T) {
	for i, c := range englishRuleContents() {
		ps := c.Problems()
		if len(ps) == 0 && i > 0 {
			t.Fatalf("content %d: no problems", i)
		}
		m := M("apply.validation_failed", "problems", firstProblems(ps, 5))
		for _, code := range jsonCodes(t, []any{m, ps}) {
			if def, ok := msgCatalog[code]; !ok || def.admin {
				t.Errorf("content %d: code %s is an administrator code or unknown", i, code)
			}
		}
	}
	for code, def := range msgCatalog {
		for _, prefix := range []string{"apply.", "auditd.", "builtin.", "common.", "sep.", "rule.version.", "template.builtin.", "loc.field", "loc.entry"} {
			if strings.HasPrefix(code, prefix) && def.admin {
				t.Errorf("%s reaches every account but is an administrator code", code)
			}
		}
	}
}
