package workers

import (
	"net/http"
	"testing"

	"mimamori/app/models"
)

// TestNamesRefuseControlCharactersAtTheEdges: a name (rule, template, token,
// display name, command set, and the new name of an imported set) with a
// control character at its start or end is refused like one with a control
// character inside; white space that is no control character around a name
// is taken away.
func TestNamesRefuseControlCharactersAtTheEdges(t *testing.T) {
	e := newTestEnv(t)
	b, err := models.GetBuiltinAgent(e.c.db.Read())
	if err != nil {
		t.Fatal(err)
	}
	sets := "/agents/" + itoa64(b.ID) + "/command-sets"
	setBody := func(name string) map[string]any {
		return map[string]any{"name": name, "purpose": "run_only", "os_user": "root", "commands": []any{map[string]any{"command_line": "true"}}}
	}
	n := 0
	cases := []struct {
		what string
		call func(name string) (int, string)
	}{
		{"rule", func(name string) (int, string) { return e.adminRaw("POST", "/rules", map[string]any{"name": name}) }},
		{"template", func(name string) (int, string) {
			return e.adminRaw("POST", "/templates", map[string]any{"name": name, "from_builtin": "base"})
		}},
		{"token", func(name string) (int, string) { return e.adminRaw("POST", "/tokens", map[string]any{"name": name}) }},
		{"display name", func(name string) (int, string) {
			n++
			return e.adminRaw("POST", "/accounts", map[string]any{"email": "user" + itoa(n) + "@example.com", "display_name": name,
				"role": "viewer", "password": "Viewer-pass-123"})
		}},
		{"command set", func(name string) (int, string) { return e.adminRaw("POST", sets, setBody(name)) }},
	}
	for _, c := range cases {
		for _, name := range []string{"ab\t", "\u0085zz", "\nname", "name\r", "\tname\t"} {
			if code, body := c.call(name); code != http.StatusBadRequest {
				t.Errorf("%s %q: %d %s, want 400", c.what, name, code, body)
			}
		}
	}
	// Spaces around a name are taken away (also the ideographic space, which
	// is no control character).
	if out := e.admin("POST", "/rules", map[string]any{"name": " web 　"}); out["name"] != "web" {
		t.Errorf("rule name: %v", out["name"])
	}
	if out := e.admin("POST", "/tokens", map[string]any{"name": " t1 "}); out["token"].(map[string]any)["name"] != "t1" {
		t.Errorf("token name: %v", out["token"])
	}
	if out := e.admin("POST", sets, setBody("　local ")); out["name"] != "local" {
		t.Errorf("set name: %v", out["name"])
	}
	// The new name of an imported set.
	src := e.admin("POST", sets, setBody("source"))
	for _, name := range []string{"copy\t", "\u0085copy"} {
		code, body := e.adminRaw("POST", sets+"/import", map[string]any{"source_agent_id": b.ID,
			"items": []any{map[string]any{"set_id": src["id"], "name": name}}})
		if code != http.StatusBadRequest {
			t.Errorf("import as %q: %d %s, want 400", name, code, body)
		}
	}
}
