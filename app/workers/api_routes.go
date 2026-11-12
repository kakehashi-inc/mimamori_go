package workers

import (
	"fmt"
	"net/http"
	"time"
)

// registerAPI registers the operations shared by the browser API
// (/api/v1, session) and the management API (/admin/v1, CLI credential).
// Routes any account may use (h) go into both route tables; administrator
// routes (a) go into admin only, so that for a viewer they do not exist
// (see sessionAuth). The management API passes the same table twice.
// Permissions are still checked on every request by requireAny /
// requireAdmin.
func (c *core) registerAPI(admin, viewer *http.ServeMux, prefix string, cli bool) {
	h := func(pattern string, fn http.HandlerFunc) {
		method, path, _ := cutSpace(pattern)
		admin.HandleFunc(method+" "+prefix+path, fn)
		if viewer != admin {
			viewer.HandleFunc(method+" "+prefix+path, fn)
		}
	}
	a := func(pattern string, fn http.HandlerFunc) {
		method, path, _ := cutSpace(pattern)
		admin.HandleFunc(method+" "+prefix+path, requireAdmin(fn))
	}
	if !cli {
		h("GET /health", c.handleHealth)
		h("POST /auth/login", c.handleLogin)
		h("POST /auth/login/verify", c.handleLoginVerify)
		h("POST /auth/logout", requireAny(c.handleLogout))
		h("POST /auth/password", requireAny(c.handleOwnPassword))
		h("PUT /auth/language", requireAny(c.handleOwnLanguage))
		h("PUT /auth/display-name", requireAny(c.handleOwnDisplayName))
		h("POST /auth/email", requireAny(c.handleOwnEmail))
		h("POST /auth/email/verify", requireAny(c.handleOwnEmailVerify))
		a("POST /auth/step-up", c.handleStepUpRequest)
		a("POST /auth/step-up/verify", c.handleStepUpVerify)
		h("GET /ui/modules", requireAny(c.handleUIModules))
	}
	h("GET /auth/me", requireAny(c.handleMe))
	h("GET /overview", requireAny(c.handleOverview))
	h("GET /guide", requireAny(c.handleGuide))

	h("GET /agents", requireAny(c.handleListAgents))
	h("GET /agents/{id}", requireAny(c.handleGetAgent))

	// Command sets: definitions and run requests (administrators; the
	// changing operations pass the step-up hook), results (any account).
	a("GET /agents/{id}/command-sets", c.handleListCommandSets)
	a("POST /agents/{id}/command-sets", c.requireCommandStepUp(c.handleCreateCommandSet))
	a("POST /agents/{id}/command-sets/validate", c.handleValidateCommandSet)
	a("POST /agents/{id}/command-sets/import", c.requireCommandStepUp(c.handleImportCommandSets))
	a("GET /agents/{id}/command-sets/{set}", c.handleGetCommandSet)
	a("PUT /agents/{id}/command-sets/{set}", c.requireCommandStepUp(c.handleUpdateCommandSet))
	a("DELETE /agents/{id}/command-sets/{set}", c.requireCommandStepUp(c.handleDeleteCommandSet))
	a("POST /agents/{id}/command-sets/{set}/run", c.handleRequestCommandRun)
	a("DELETE /agents/{id}/command-requests/{req}", c.handleDiscardCommandRequest)
	h("GET /agents/{id}/command-results", requireAny(c.handleAgentCommandResults))
	h("GET /agents/{id}/command-sets/{set}/runs", requireAny(c.handleListCommandRuns))
	h("GET /agents/{id}/command-runs/{run}", requireAny(c.handleGetCommandRun))
	h("GET /command-results", requireAny(c.handleCommandResultsByName))

	h("GET /tokens", requireAny(c.handleListTokens))
	h("GET /tokens/{id}", requireAny(c.handleGetToken))
	a("POST /tokens", c.handleIssueToken)
	a("POST /tokens/{id}/reissue", c.handleReissueToken)
	a("POST /tokens/{id}/unbind", c.handleUnbindToken)
	a("POST /tokens/{id}/revoke", c.handleRevokeToken)

	h("GET /rules", requireAny(c.handleListRules))
	a("POST /rules", c.handleCreateRule)
	a("POST /rules/merge-preview", c.handleMergePreview)
	a("POST /rules/validate", c.handleValidateRule)
	h("GET /rules/{id}", requireAny(c.handleGetRule))
	a("PUT /rules/{id}", c.handleSaveRule)
	a("DELETE /rules/{id}", c.handleDeleteRule)
	h("GET /rules/{id}/versions/{version}", requireAny(c.handleGetRuleVersion))
	h("GET /rules/{id}/diff", requireAny(c.handleRuleDiff))
	a("POST /rules/{id}/apply", c.handleApplyRule)
	a("POST /rules/{id}/to-template", c.handleRuleToTemplate)

	h("GET /templates", requireAny(c.handleListTemplates))
	a("POST /templates", c.handleCreateTemplate)
	h("GET /templates/{name}", requireAny(c.handleGetTemplate))
	a("PUT /templates/{name}", c.handleUpdateTemplate)
	a("DELETE /templates/{name}", c.handleDeleteTemplate)

	h("GET /detections", requireAny(c.handleListDetections))
	h("GET /detections/{agent}/{id}", requireAny(c.handleGetDetection))
	a("POST /detections/ack", c.handleAckDetections)

	h("GET /suppressions", requireAny(c.handleListSuppressions))
	a("POST /suppressions", c.handleCreateSuppression)
	a("DELETE /suppressions/{id}", c.handleDeleteSuppression)

	h("GET /certificates", requireAny(c.handleCertificates))
	h("GET /history", requireAny(c.handleHistory))

	a("GET /config", c.handleGetConfig)
	a("PUT /config", c.handleUpdateConfig)

	a("GET /accounts", c.handleListAccounts)
	a("POST /accounts", c.handleCreateAccount)
	a("PUT /accounts/{id}/email", c.handleAccountEmail)
	a("PUT /accounts/{id}/display-name", c.handleAccountDisplayName)
	a("PUT /accounts/{id}/role", c.handleAccountRole)
	a("PUT /accounts/{id}/password", c.handleAccountPassword)
	a("POST /accounts/{id}/disable", c.handleAccountDisable)
	a("POST /accounts/{id}/enable", c.handleAccountEnable)
	a("DELETE /accounts/{id}", c.handleDeleteAccount)
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

// parseTime accepts RFC3339 or a datetime-local value ("2006-01-02T15:04")
// for the query parameter name. An empty value gives 0 (not given); any
// other value that cannot be parsed is an error.
func parseTime(name, s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("invalid %s %q: use RFC3339 or YYYY-MM-DD[THH:MM[:SS]]", name, s)
}
