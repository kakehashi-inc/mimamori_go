package workers

import (
	"net/http"
	"time"
)

// registerAPI registers the operations shared by the browser API
// (/api/v1, session) and the management API (/admin/v1, CLI credential).
// Permissions are checked on every request by requireAny / requireAdmin.
func (c *core) registerAPI(mux *http.ServeMux, prefix string, cli bool) {
	h := func(pattern string, fn http.HandlerFunc) {
		method, path, _ := cutSpace(pattern)
		mux.HandleFunc(method+" "+prefix+path, fn)
	}
	if !cli {
		h("GET /health", c.handleHealth)
		h("POST /auth/login", c.handleLogin)
		h("POST /auth/logout", requireAny(c.handleLogout))
		h("POST /auth/password", requireAny(c.handleOwnPassword))
		h("PUT /auth/language", requireAny(c.handleOwnLanguage))
	}
	h("GET /auth/me", requireAny(c.handleMe))
	h("GET /overview", requireAny(c.handleOverview))
	h("GET /guide", requireAny(c.handleGuide))

	h("GET /agents", requireAny(c.handleListAgents))
	h("GET /agents/{id}", requireAny(c.handleGetAgent))

	h("GET /tokens", requireAny(c.handleListTokens))
	h("GET /tokens/{id}", requireAny(c.handleGetToken))
	h("POST /tokens", requireAdmin(c.handleIssueToken))
	h("POST /tokens/{id}/reissue", requireAdmin(c.handleReissueToken))
	h("POST /tokens/{id}/unbind", requireAdmin(c.handleUnbindToken))
	h("POST /tokens/{id}/revoke", requireAdmin(c.handleRevokeToken))

	h("GET /rules", requireAny(c.handleListRules))
	h("POST /rules", requireAdmin(c.handleCreateRule))
	h("POST /rules/merge-preview", requireAdmin(c.handleMergePreview))
	h("POST /rules/validate", requireAdmin(c.handleValidateRule))
	h("GET /rules/{id}", requireAny(c.handleGetRule))
	h("PUT /rules/{id}", requireAdmin(c.handleSaveRule))
	h("DELETE /rules/{id}", requireAdmin(c.handleDeleteRule))
	h("GET /rules/{id}/versions/{version}", requireAny(c.handleGetRuleVersion))
	h("GET /rules/{id}/diff", requireAny(c.handleRuleDiff))
	h("POST /rules/{id}/apply", requireAdmin(c.handleApplyRule))
	h("POST /rules/{id}/to-template", requireAdmin(c.handleRuleToTemplate))

	h("GET /templates", requireAny(c.handleListTemplates))
	h("POST /templates", requireAdmin(c.handleCreateTemplate))
	h("GET /templates/{name}", requireAny(c.handleGetTemplate))
	h("PUT /templates/{name}", requireAdmin(c.handleUpdateTemplate))
	h("DELETE /templates/{name}", requireAdmin(c.handleDeleteTemplate))

	h("GET /detections", requireAny(c.handleListDetections))
	h("GET /detections/{agent}/{id}", requireAny(c.handleGetDetection))
	h("POST /detections/ack", requireAdmin(c.handleAckDetections))

	h("GET /suppressions", requireAny(c.handleListSuppressions))
	h("POST /suppressions", requireAdmin(c.handleCreateSuppression))
	h("DELETE /suppressions/{id}", requireAdmin(c.handleDeleteSuppression))

	h("GET /certificates", requireAny(c.handleCertificates))
	h("GET /history", requireAny(c.handleHistory))

	h("GET /config", requireAdmin(c.handleGetConfig))
	h("PUT /config", requireAdmin(c.handleUpdateConfig))

	h("GET /accounts", requireAdmin(c.handleListAccounts))
	h("POST /accounts", requireAdmin(c.handleCreateAccount))
	h("PUT /accounts/{id}/role", requireAdmin(c.handleAccountRole))
	h("PUT /accounts/{id}/password", requireAdmin(c.handleAccountPassword))
	h("POST /accounts/{id}/disable", requireAdmin(c.handleAccountDisable))
	h("POST /accounts/{id}/enable", requireAdmin(c.handleAccountEnable))
	h("DELETE /accounts/{id}", requireAdmin(c.handleDeleteAccount))
}

func cutSpace(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// ts formats a Unix time for JSON (nil when not set).
func ts(t int64) *string {
	if t <= 0 {
		return nil
	}
	s := time.Unix(t, 0).UTC().Format(time.RFC3339)
	return &s
}

// parseTime accepts RFC3339 or a datetime-local value ("2006-01-02T15:04").
func parseTime(s string) int64 {
	if s == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.Unix()
		}
	}
	return 0
}
