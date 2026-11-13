package workers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mimamori/app/modules"
)

// checkErrorBody checks an error answer: "error_i18n" is present, has the
// expected code ("" for a text that is data only), and its English rendering
// is exactly "error".
func checkErrorBody(t *testing.T, name string, body []byte, wantCode string) {
	t.Helper()
	var out struct {
		Error     *string          `json:"error"`
		ErrorI18n *json.RawMessage `json:"error_i18n"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Errorf("%s: body %q: %v", name, body, err)
		return
	}
	if out.Error == nil || out.ErrorI18n == nil {
		t.Errorf("%s: error or error_i18n missing: %s", name, body)
		return
	}
	var m modules.Msg
	if err := json.Unmarshal(*out.ErrorI18n, &m); err != nil {
		t.Errorf("%s: error_i18n %s: %v", name, *out.ErrorI18n, err)
		return
	}
	if m.Code != wantCode {
		t.Errorf("%s: code %q, want %q (%s)", name, m.Code, wantCode, body)
	}
	if got := m.Text(); got != *out.Error {
		t.Errorf("%s: English rendering %q differs from error %q", name, got, *out.Error)
	}
}

// rawRequest sends a request with a body given as text.
func (e *testEnv) rawRequest(h http.Handler, method, path, body string, hdr map[string]string, remote string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = remote
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Every error answer carries "error_i18n", whose English rendering is the
// text of "error": a representative answer of every file that answers
// errors.
func TestErrorAnswersCarryMessage(t *testing.T) {
	f := newStepUpFixture(t)
	e := f.e
	tok, a, _ := commandAgent(t, e, "host-i18n")
	agent := "/agents/" + itoa64(a.ID)
	cliHdr := map[string]string{"Authorization": "Bearer " + e.cliKey}
	ck := f.login(t)
	web := func(method, path string, body any) *httptest.ResponseRecorder {
		return e.do(e.web, method, path, body, browserHdr(ck, true), "192.0.2.1:1")
	}
	cases := []struct {
		name   string
		rec    *httptest.ResponseRecorder
		status int
		code   string
	}{
		// core.go: request helpers
		{"core readJSON unknown field", e.cli("POST", "/tokens", map[string]any{"name": "x", "bogus": 1}), 400, "common.invalid_body"},
		{"core readJSON trailing data", e.rawRequest(e.agent, "POST", "/admin/v1/tokens", `{"name":"t"} {}`, cliHdr, "127.0.0.1:1"), 400, "common.invalid_body_trailing"},
		{"core readJSON syntax", e.rawRequest(e.agent, "POST", "/admin/v1/tokens", `{"name":`, cliHdr, "127.0.0.1:1"), 400, "common.invalid_body"},
		{"core queryInt", e.cli("GET", "/history?limit=x", nil), 400, "common.query_not_integer"},
		// api_routes.go
		{"api_routes parseTime", e.cli("GET", "/detections?from=garbage", nil), 400, "common.query_bad_time"},
		// api_detections.go
		{"api_detections boolQuery", e.cli("GET", "/detections?acknowledged=yes", nil), 400, "common.query_not_bool"},
		{"api_detections agent_id", e.cli("GET", "/detections?agent_id=abc", nil), 400, "common.query_agent_id"},
		{"api_detections type", e.cli("POST", "/suppressions", map[string]any{"type": "nope", "exe": "/bin/x"}), 400, "suppression.type_invalid"},
		{"api_detections control", e.cli("POST", "/suppressions", map[string]any{"exe": "/bin/a\x00b"}), 400, "suppression.field_control"},
		{"api_detections too long", e.cli("POST", "/suppressions", map[string]any{"exe": "/" + strings.Repeat("x", 4096)}), 400, "suppression.field_too_long_bytes"},
		{"api_detections condition", e.cli("POST", "/suppressions", map[string]any{}), 400, "suppression.condition_required"},
		{"api_detections suppression", e.cli("DELETE", "/suppressions/9999", nil), 404, "suppression.not_found"},
		// api_accounts.go
		{"api_accounts not found", e.cli("PUT", "/accounts/9999/role", map[string]any{"role": "viewer"}), 404, "account.not_found"},
		{"api_accounts role", e.cli("PUT", "/accounts/1/role", map[string]any{"role": "owner"}), 400, "account.role_invalid"},
		{"api_accounts validator", e.cli("POST", "/accounts", map[string]any{"email": "not an address", "display_name": "x", "password": "Some-pass-123", "role": "viewer"}), 400, "account.email_chars"},
		{"api_accounts login", e.do(e.web, "POST", "/api/v1/auth/login", map[string]string{"email": testAdminEmail, "password": "wrong"}, map[string]string{csrfHeader: "1"}, "192.0.2.9:1"), 401, "auth.login_failed"},
		{"api_accounts language", web("PUT", "/api/v1/auth/language", map[string]any{"language": "fr"}), 400, "account.language_invalid"},
		// api_agents.go
		{"api_agents agent", e.cli("GET", "/agents/9999", nil), 404, "agent.not_found"},
		{"api_agents token", e.cli("GET", "/tokens/9999", nil), 404, "token.not_found"},
		{"api_agents token name", e.cli("POST", "/tokens", map[string]any{"name": "bad name!"}), 400, "token.name_invalid"},
		{"api_agents valid days", e.cli("POST", "/tokens", map[string]any{"name": "ok", "valid_days": 5000}), 400, "token.valid_days_range"},
		{"api_agents token rule", e.cli("POST", "/tokens", map[string]any{"name": "ok", "rule_id": 9999}), 400, "rule.not_found"},
		// api_rules.go
		{"api_rules rule", e.cli("GET", "/rules/9999", nil), 404, "rule.not_found"},
		{"api_rules name", e.cli("POST", "/rules", map[string]any{"name": ""}), 400, "rule.name_invalid"},
		{"api_rules select template", e.cli("POST", "/rules/merge-preview", map[string]any{"templates": []string{}}), 400, "rule.select_template"},
		{"api_rules template", e.cli("POST", "/rules", map[string]any{"name": "r", "templates": []string{"nope"}}), 400, "rule.template_not_found_named"},
		{"api_rules problems", e.cli("POST", "/rules", map[string]any{"name": "r", "content": map[string]any{"users": []any{map[string]any{"value": "a b"}}, "read": map[string]any{"dirs": []any{map[string]any{"value": "rel"}}}}}), 400, "common.problems"},
		{"api_rules content", e.cli("POST", "/rules", map[string]any{"name": "r", "content": map[string]any{"users": "alice"}}), 400, "rule.content.wrong_type"},
		{"api_rules unassign", e.cli("POST", "/rules/unassign", map[string]any{"agent_ids": []int64{9999}}), 400, "agent.id_not_found"},
		{"api_rules select agent", e.cli("POST", "/rules/unassign", map[string]any{"agent_ids": []int64{}}), 400, "rule.select_agent"},
		{"api_rules builtin name", e.cli("POST", "/templates", map[string]any{"name": "base", "content": map[string]any{}}), 409, "template.builtin_name"},
		{"api_rules template not found", e.cli("GET", "/templates/nope", nil), 404, "template.not_found"},
		{"api_rules diff", e.cli("GET", "/rules/9999/diff", nil), 404, "rule.not_found"},
		// api_commands.go
		{"api_commands set", e.cli("GET", agent+"/command-sets/9999", nil), 404, "command.set_not_found"},
		{"api_commands invalid set", e.cli("POST", agent+"/command-sets", map[string]any{"name": "", "purpose": "run_only", "commands": []any{}}), 400, "command.invalid_set"},
		{"api_commands decode", e.rawRequest(e.agent, "POST", "/admin/v1"+agent+"/command-sets", `[1]`, cliHdr, "127.0.0.1:1"), 400, "command.spec.not_object"},
		{"api_commands import", e.cli("POST", agent+"/command-sets/import", map[string]any{"source_agent_id": a.ID, "items": []any{}}), 400, "command.import_select"},
		{"api_commands import source", e.cli("POST", agent+"/command-sets/import", map[string]any{"source_agent_id": 9999, "items": []any{map[string]any{"set_id": 1}}}), 400, "command.import_source_not_found"},
		{"api_commands run", e.cli("GET", agent+"/command-runs/9999", nil), 404, "command.run_not_found"},
		{"api_commands request", e.cli("DELETE", agent+"/command-requests/9999", nil), 404, "command.request_not_found"},
		// api_config.go
		{"api_config body", e.cli("PUT", "/config", map[string]any{"bogus": 1}), 400, "common.invalid_body"},
		{"api_config item", e.cli("PUT", "/config", map[string]any{"set": map[string]string{"nope": "1"}}), 400, "config.unknown_item"},
		// api_profile.go
		{"api_profile unchanged", web("POST", "/api/v1/auth/email", map[string]any{"email": testAdminEmail}), 400, "account.email_unchanged"},
		{"api_profile verify", web("POST", "/api/v1/auth/email/verify", map[string]any{"code": "1"}), 410, "auth.email_code_expired"},
		// api_step_up.go
		{"api_step_up required", web("DELETE", "/api/v1"+agent+"/command-sets/"+f.setID, nil), 428, "auth.step_up_required"},
		{"api_step_up verify", web("POST", "/api/v1/auth/step-up/verify", map[string]any{"code": "1"}), 410, "auth.step_up_code_expired"},
		// api_two_step.go
		{"api_two_step challenge", e.do(e.web, "POST", "/api/v1/auth/login/verify", map[string]any{"challenge": "x", "code": "1"}, map[string]string{csrfHeader: "1"}, "192.0.2.9:1"), 410, "auth.login_code_expired"},
		// auth.go
		{"auth header", e.do(e.web, "POST", "/api/v1/auth/logout", nil, browserHdr(ck, false), "192.0.2.1:1"), 403, "auth.missing_header"},
		{"auth login", e.do(e.web, "GET", "/api/v1/overview", nil, nil, "192.0.2.1:1"), 401, "auth.login_required"},
		{"auth cli", e.do(e.agent, "GET", "/admin/v1/overview", nil, map[string]string{"Authorization": "Bearer wrong"}, "127.0.0.1:1"), 401, "auth.unauthorized"},
		// agent_api.go
		{"agent_api token", e.agentCall("sync", "wrong", "m-1", map[string]any{}), 401, "auth.unauthorized"},
		{"agent_api request", e.rawRequest(e.agent, "POST", "/agent/v1/sync", `{`, map[string]string{"Authorization": "Bearer " + tok, modules.HeaderInstanceID: "m-i18n"}, "192.0.2.10:4000"), 400, "common.invalid_request"},
	}
	for _, c := range cases {
		if c.rec.Code != c.status {
			t.Errorf("%s: status %d, want %d (%s)", c.name, c.rec.Code, c.status, c.rec.Body.String())
			continue
		}
		checkErrorBody(t, c.name, c.rec.Body.Bytes(), c.code)
	}

	// core.go: internalError
	rec := httptest.NewRecorder()
	e.c.internalError(rec, httptest.NewRequest("GET", "/x", nil), errors.New("disk on fire"))
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "fire") {
		t.Errorf("internal error: %d %s", rec.Code, rec.Body.String())
	}
	checkErrorBody(t, "core internalError", rec.Body.Bytes(), "common.internal_error")

	// core.go: the refusal of the connection server to read a large body
	m := invalidBody(errBodyRefused)
	if m.Text() != "invalid request body: "+errBodyRefused.Error() {
		t.Errorf("refused body: %q", m.Text())
	}
	if d, ok := m.Params["detail"].(modules.Msg); !ok || d.Code != "common.body_refused" {
		t.Errorf("refused body detail: %#v", m.Params["detail"])
	}
}

// The answers the connection server writes itself carry the message too.
func TestConnServerFixedAnswersCarryMessage(t *testing.T) {
	for _, c := range []struct {
		name   string
		resp   []byte
		status int
		code   string
	}{
		{"busy", busyResponse, http.StatusServiceUnavailable, "common.busy"},
		{"head too large", headTooLargeResponse, http.StatusRequestHeaderFieldsTooLarge, "common.header_too_large"},
		{"length required", lengthRequiredResponse, http.StatusLengthRequired, "common.length_required"},
	} {
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(c.resp)), nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != c.status || int64(len(body)) != resp.ContentLength {
			t.Fatalf("%s: %d %q (length %d) %v", c.name, resp.StatusCode, body, resp.ContentLength, err)
		}
		checkErrorBody(t, c.name, body, c.code)
	}
	checkErrorBody(t, "panic", internalErrorBody, "common.internal_error")
}
