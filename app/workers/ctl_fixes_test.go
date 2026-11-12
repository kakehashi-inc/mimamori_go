package workers

import (
	"strings"
	"testing"

	"mimamori/app/models"
)

func (e *testEnv) adminRaw(method, path string, body any) (int, string) {
	e.t.Helper()
	rec := e.do(e.agent, method, "/admin/v1"+path, body, map[string]string{"Authorization": "Bearer " + e.cliKey}, "127.0.0.1:5000")
	return rec.Code, rec.Body.String()
}

// Updating a template without a description keeps the stored one.
func TestTemplateUpdateKeepsDescription(t *testing.T) {
	e := newTestEnv(t)
	e.admin("POST", "/templates", map[string]any{"name": "tpl", "description": "first", "from_builtin": "base"})
	content := map[string]any{"users": []any{}}
	out := e.admin("PUT", "/templates/tpl", map[string]any{"content": content})
	if out["description"] != "first" {
		t.Errorf("response description = %v", out["description"])
	}
	if tpl, err := models.GetTemplateByName(e.c.db, "tpl"); err != nil || tpl.Description != "first" {
		t.Errorf("stored description = %v, %v", tpl, err)
	}
	e.admin("PUT", "/templates/tpl", map[string]any{"content": content, "description": "second"})
	if tpl, _ := models.GetTemplateByName(e.c.db, "tpl"); tpl.Description != "second" {
		t.Errorf("description not changed: %q", tpl.Description)
	}
	if code, _ := e.adminRaw("PUT", "/templates/missing", map[string]any{"content": content}); code != 404 {
		t.Errorf("missing template: %d", code)
	}
}

// A template copied from a rule without a version uses the latest version.
func TestTemplateFromRuleLatestVersion(t *testing.T) {
	e := newTestEnv(t)
	e.admin("POST", "/rules", map[string]any{"name": "r"})
	e.admin("PUT", "/rules/1", map[string]any{"content": map[string]any{"users": []any{map[string]any{"value": "alice"}}}})
	out := e.admin("POST", "/templates", map[string]any{"name": "fromrule", "from_rule": map[string]any{"rule_id": 1}})
	users, _ := out["content"].(map[string]any)["users"].([]any)
	if len(users) != 1 {
		t.Errorf("content of the latest version not copied: %v", out["content"])
	}
	if code, _ := e.adminRaw("POST", "/templates", map[string]any{"name": "x", "from_rule": map[string]any{"rule_id": 99}}); code != 400 {
		t.Errorf("missing rule: %d", code)
	}
}

// An agent given more than once is applied and counted once.
func TestApplyRuleCountsAgentOnce(t *testing.T) {
	e := newTestEnv(t)
	b, _ := models.GetBuiltinAgent(e.c.db)
	e.admin("POST", "/rules", map[string]any{"name": "r"})
	out := e.admin("POST", "/rules/1/apply", map[string]any{"agent_ids": []int64{b.ID, b.ID}})
	if out["applied"] != float64(1) {
		t.Errorf("applied = %v", out["applied"])
	}
}

// Rule content that is null or not an object is rejected.
func TestRuleContentMustBeObject(t *testing.T) {
	e := newTestEnv(t)
	e.admin("POST", "/rules", map[string]any{"name": "r"})
	for _, c := range []any{nil, []int{1, 2}, "x"} {
		body := map[string]any{"name": "n", "content": c}
		if code, msg := e.adminRaw("POST", "/rules", body); code != 400 || strings.Contains(msg, "Go ") {
			t.Errorf("create %v: %d %s", c, code, msg)
		}
		if code, msg := e.adminRaw("PUT", "/rules/1", map[string]any{"content": c}); code != 400 || strings.Contains(msg, "Go ") {
			t.Errorf("save %v: %d %s", c, code, msg)
		}
		if code, _ := e.adminRaw("POST", "/templates", map[string]any{"name": "t", "content": c}); code != 400 {
			t.Errorf("template %v: %d", c, code)
		}
	}
	if code, msg := e.adminRaw("POST", "/rules", map[string]any{"name": "u", "content": map[string]any{"users": "alice"}}); code != 400 || strings.Contains(msg, "Go ") || !strings.Contains(msg, "users must be an array") {
		t.Errorf("wrong type: %d %s", code, msg)
	}
	// Without content a rule is still created empty.
	e.admin("POST", "/rules", map[string]any{"name": "empty"})
}

// Text is cut at most n bytes on a character boundary.
func TestTruncateKeepsUTF8(t *testing.T) {
	s := "a" + strings.Repeat("あ", 10)
	for n := 0; n <= len(s); n++ {
		got := truncate(s, n)
		if len(got) > n || !strings.HasPrefix(s, got) || strings.ContainsRune(got, '�') {
			t.Errorf("truncate(%d) = %q", n, got)
		}
	}
}
