package workers

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"mimamori/app/modules"
)

// problemsAnswer is an answer with a list of validation problems.
type problemsAnswer struct {
	Error      string            `json:"error"`
	ErrorI18n  *modules.Msg      `json:"error_i18n"`
	Errors     []string          `json:"errors"`
	ErrorsI18n []modules.Problem `json:"errors_i18n"`
}

// checkProblems decodes a problems answer and checks that "errors_i18n" has
// one coded problem per text of "errors", in the same order and with the
// same English rendering. It returns the answer.
func checkProblems(t *testing.T, name string, rec *httptest.ResponseRecorder, status int) problemsAnswer {
	t.Helper()
	var out problemsAnswer
	if rec.Code != status {
		t.Fatalf("%s: status %d, want %d: %s", name, rec.Code, status, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: %v: %s", name, err, rec.Body.String())
	}
	if out.Errors == nil || out.ErrorsI18n == nil {
		t.Fatalf("%s: errors or errors_i18n missing: %s", name, rec.Body.String())
	}
	if len(out.Errors) != len(out.ErrorsI18n) {
		t.Fatalf("%s: %d errors, %d errors_i18n", name, len(out.Errors), len(out.ErrorsI18n))
	}
	for i, p := range out.ErrorsI18n {
		if p.Code == "" {
			t.Errorf("%s: problem %d has no code: %s", name, i, rec.Body.String())
		}
		if got := p.Text(); got != out.Errors[i] {
			t.Errorf("%s: problem %d renders %q, errors has %q", name, i, got, out.Errors[i])
		}
	}
	if out.ErrorI18n != nil && out.ErrorI18n.Text() != out.Error {
		t.Errorf("%s: error_i18n renders %q, error is %q", name, out.ErrorI18n.Text(), out.Error)
	}
	return out
}

// codesOf returns the codes of problems, each with its location codes
// before it ("loc.item>loc.command>command.spec.required").
func codesOf(ps []modules.Problem) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		parts := []string{}
		for _, w := range p.Where {
			parts = append(parts, w.Code)
		}
		out[i] = strings.Join(append(parts, p.Code), ">")
	}
	return out
}

func containsCode(list []string, code string) bool {
	for _, c := range list {
		if c == code {
			return true
		}
	}
	return false
}

// TestValidationListsCarryProblems checks "errors_i18n" next to "errors" in
// the answers that list validation problems: rule and template content,
// command set check, creation, update and import, and the configuration.
func TestValidationListsCarryProblems(t *testing.T) {
	e := newTestEnv(t)
	_, a, _ := commandAgent(t, e, "host-problems")
	agent := "/agents/" + itoa64(a.ID)

	// Rules and templates.
	bad := map[string]any{"users": []any{map[string]any{"value": "a b"}}, "read": map[string]any{"dirs": []any{map[string]any{"value": "rel"}}},
		"exec_files": map[string]any{"legit": []any{map[string]any{"path": "x", "category": "alert"}}}}
	out := checkProblems(t, "rules validate", e.cli("POST", "/rules/validate", map[string]any{"content": bad}), 200)
	codes := codesOf(out.ErrorsI18n)
	for _, want := range []string{"loc.field>rule.content.user_invalid", "loc.field>rule.content.path_not_absolute", "loc.entry>rule.content.legit_extension_required"} {
		if !containsCode(codes, want) {
			t.Errorf("rules validate: %q missing in %q", want, codes)
		}
	}
	out = checkProblems(t, "rules validate decode", e.cli("POST", "/rules/validate", map[string]any{"content": map[string]any{"users": "alice"}}), 200)
	if got := codesOf(out.ErrorsI18n); len(got) != 1 || got[0] != "rule.content.wrong_type" {
		t.Errorf("rules validate decode: %q", got)
	}
	out = checkProblems(t, "rules validate valid", e.cli("POST", "/rules/validate", map[string]any{"content": map[string]any{}}), 200)
	if len(out.Errors) != 0 {
		t.Errorf("rules validate valid: %q", out.Errors)
	}
	rec := e.cli("POST", "/templates", map[string]any{"name": "tpl-problems", "content": bad})
	checkErrorBody(t, "template create", rec.Body.Bytes(), "common.problems")

	// Command sets.
	badSet := map[string]any{"name": "", "purpose": "collect_output", "os_user": "-x",
		"commands": []any{map[string]any{"command_line": "true", "timeout_sec": -1, "patterns": []string{`(?<1>a)`}}}}
	out = checkProblems(t, "command set validate", e.cli("POST", agent+"/command-sets/validate", badSet), 200)
	codes = codesOf(out.ErrorsI18n)
	for _, want := range []string{"loc.field>command.spec.required", "loc.field>command.spec.os_user_dash",
		"loc.command>loc.field>command.spec.range_1", "loc.command>loc.pattern>pattern.numbered_group"} {
		if !containsCode(codes, want) {
			t.Errorf("command set validate: %q missing in %q", want, codes)
		}
	}
	out = checkProblems(t, "command set validate decode", e.cli("POST", agent+"/command-sets/validate", []any{1}), 200)
	if got := codesOf(out.ErrorsI18n); len(got) != 1 || got[0] != "command.spec.not_object" {
		t.Errorf("command set validate decode: %q", got)
	}
	out = checkProblems(t, "command set create", e.cli("POST", agent+"/command-sets", badSet), 400)
	if out.ErrorI18n == nil || out.ErrorI18n.Code != "command.invalid_set" {
		t.Errorf("command set create: error_i18n %+v", out.ErrorI18n)
	}
	good := map[string]any{"name": "taken", "purpose": "run_only", "os_user": "nobody",
		"commands": []any{map[string]any{"command_line": "true"}}}
	if rec := e.cli("POST", agent+"/command-sets", good); rec.Code != 200 {
		t.Fatalf("command set create: %d %s", rec.Code, rec.Body.String())
	}
	out = checkProblems(t, "command set validate name", e.cli("POST", agent+"/command-sets/validate", good), 200)
	if got := codesOf(out.ErrorsI18n); len(got) != 1 || got[0] != "command.name_taken" {
		t.Errorf("command set validate name: %q", got)
	}
	src := e.cli("GET", agent+"/command-sets", nil)
	var sets struct {
		Sets []struct {
			ID int64 `json:"id"`
		} `json:"sets"`
	}
	_ = json.Unmarshal(src.Body.Bytes(), &sets)
	if len(sets.Sets) != 1 {
		t.Fatalf("command sets: %s", src.Body.String())
	}
	importBody := map[string]any{"source_agent_id": a.ID, "items": []any{
		map[string]any{"set_id": 9999}, map[string]any{"set_id": sets.Sets[0].ID, "name": "bad\x01name"}}}
	out = checkProblems(t, "command set import", e.cli("POST", agent+"/command-sets/import", importBody), 400)
	codes = codesOf(out.ErrorsI18n)
	for _, want := range []string{"loc.item>command.import_item_not_found", "loc.item>loc.field>command.spec.control"} {
		if !containsCode(codes, want) {
			t.Errorf("command set import: %q missing in %q", want, codes)
		}
	}
	if len(out.Errors) == 0 || out.Errors[0] != "item 1: command set 9999 not found in the source" {
		t.Errorf("command set import: errors %q", out.Errors)
	}

	// Configuration.
	out = checkProblems(t, "config", e.cli("PUT", "/config", map[string]any{"set": map[string]string{"smtp.security": "x", "server_names": "a b"}}), 400)
	if out.ErrorI18n == nil || out.ErrorI18n.Code != "common.problems" {
		t.Errorf("config: error_i18n %+v", out.ErrorI18n)
	}
	if got := codesOf(out.ErrorsI18n); len(got) != 2 || got[0] != "config.server_name_invalid" || got[1] != "config.smtp_security" {
		t.Errorf("config: %q", got)
	}
	if want := `server_names: "a b" is not a host name or IP address; smtp.security must be starttls, tls or none`; out.Error != want {
		t.Errorf("config: error %q, want %q", out.Error, want)
	}
	rec = e.cli("PUT", "/config", map[string]any{"set": map[string]string{"workers": "2"}, "unset": []string{"workers"}})
	if rec.Code != 400 {
		t.Fatalf("config set and unset: %d %s", rec.Code, rec.Body.String())
	}
	checkErrorBody(t, "config set and unset", rec.Body.Bytes(), "config.set_and_unset")
}

// TestConfigSaveFailureIsInternal checks that a failure of the database
// while the configuration is saved is answered as an internal error, not
// as a refused value with the database's text.
func TestConfigSaveFailureIsInternal(t *testing.T) {
	e := newTestEnv(t)
	for _, stmt := range []string{
		`CREATE TRIGGER fail_settings_insert BEFORE INSERT ON settings BEGIN SELECT RAISE(ABORT, 'disk on fire'); END`,
		`CREATE TRIGGER fail_settings_update BEFORE UPDATE ON settings BEGIN SELECT RAISE(ABORT, 'disk on fire'); END`,
	} {
		if _, err := e.c.pool.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	rec := e.cli("PUT", "/config", map[string]any{"set": map[string]string{"retention_days": "30"}})
	if rec.Code != 500 {
		t.Fatalf("status %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "disk on fire") {
		t.Errorf("the database's text reached the answer: %s", rec.Body.String())
	}
	checkErrorBody(t, "config save failure", rec.Body.Bytes(), "common.internal_error")
}
