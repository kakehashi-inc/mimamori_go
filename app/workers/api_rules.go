package workers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

var (
	ruleNameRe  = regexp.MustCompile(`^[^\x00-\x1f]{1,100}$`)
	tokenNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

func itoa(v int) string     { return strconv.Itoa(v) }
func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

type appliedAgentJSON struct {
	AgentID     int64  `json:"agent_id"`
	DisplayName string `json:"display_name"`
	Version     int    `json:"version"`
}

type ruleSummaryJSON struct {
	ID            int64              `json:"id"`
	Name          string             `json:"name"`
	Description   string             `json:"description"`
	LatestVersion int                `json:"latest_version"`
	CreatedAt     *string            `json:"created_at"`
	UpdatedAt     *string            `json:"updated_at"`
	AppliedAgents []appliedAgentJSON `json:"applied_agents"`
}

type ruleVersionMetaJSON struct {
	Version     int          `json:"version"`
	CreatedAt   *string      `json:"created_at"`
	CreatedBy   string       `json:"created_by"`
	Comment     string       `json:"comment"`
	CommentI18n *modules.Msg `json:"comment_i18n,omitempty"`
}

type ruleDetailJSON struct {
	ruleSummaryJSON
	Content  *modules.RuleContent  `json:"content"`
	Versions []ruleVersionMetaJSON `json:"versions"`
}

type ruleVersionJSON struct {
	RuleID      int64                `json:"rule_id"`
	Version     int                  `json:"version"`
	Content     *modules.RuleContent `json:"content"`
	CreatedAt   *string              `json:"created_at"`
	CreatedBy   string               `json:"created_by"`
	Comment     string               `json:"comment"`
	CommentI18n *modules.Msg         `json:"comment_i18n,omitempty"`
}

// versionCommentI18n returns the coded comment of a version that the
// program wrote (nil for a comment written by a person, and when the stored
// one does not match the English comment).
func versionCommentI18n(v *models.RuleVersion) *modules.Msg {
	if v.CommentI18n == "" {
		return nil
	}
	var m modules.Msg
	if json.Unmarshal([]byte(v.CommentI18n), &m) != nil || m.Text() != v.Comment {
		return nil
	}
	return &m
}

func (c *core) ruleSummary(r *models.Rule, agents []*models.Agent) ruleSummaryJSON {
	s := ruleSummaryJSON{ID: r.ID, Name: r.Name, Description: r.Description, LatestVersion: r.LatestVersion,
		CreatedAt: ts(r.CreatedAt), UpdatedAt: ts(r.UpdatedAt), AppliedAgents: []appliedAgentJSON{}}
	for _, a := range agents {
		if a.AssignedRuleID.Valid && a.AssignedRuleID.Int64 == r.ID {
			s.AppliedAgents = append(s.AppliedAgents, appliedAgentJSON{AgentID: a.ID, DisplayName: c.agentDisplayName(a), Version: a.AssignedRuleVersion})
		}
	}
	return s
}

func (c *core) handleListRules(w http.ResponseWriter, r *http.Request) {
	list, err := models.ListRules(c.db)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	agents, _ := models.ListAgents(c.db)
	out := make([]ruleSummaryJSON, 0, len(list))
	for _, rule := range list {
		out = append(out, c.ruleSummary(rule, agents))
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *core) loadRule(w http.ResponseWriter, r *http.Request) *models.Rule {
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, modules.M("rule.not_found"))
		return nil
	}
	rule, err := models.GetRule(c.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, modules.M("rule.not_found"))
		return nil
	}
	if err != nil {
		c.internalError(w, r, err)
		return nil
	}
	return rule
}

// ruleDetail converts a rule with its versions for the API; show converts
// the recorded operators (see operatorShown).
func (c *core) ruleDetail(rule *models.Rule, show func(string) string) (*ruleDetailJSON, error) {
	agents, _ := models.ListAgents(c.db)
	out := &ruleDetailJSON{ruleSummaryJSON: c.ruleSummary(rule, agents), Versions: []ruleVersionMetaJSON{}}
	versions, err := models.ListRuleVersions(c.db, rule.ID)
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		out.Versions = append(out.Versions, ruleVersionMetaJSON{Version: v.Version, CreatedAt: ts(v.CreatedAt), CreatedBy: show(v.CreatedBy), Comment: v.Comment,
			CommentI18n: versionCommentI18n(v)})
		if v.Version == rule.LatestVersion {
			content, err := modules.ParseRuleContent([]byte(v.Content))
			if err != nil {
				return nil, err
			}
			out.Content = content
		}
	}
	if out.Content == nil {
		out.Content = emptyContent()
	}
	return out, nil
}

func emptyContent() *modules.RuleContent {
	c := &modules.RuleContent{}
	c.Normalize()
	return c
}

func (c *core) handleGetRule(w http.ResponseWriter, r *http.Request) {
	rule := c.loadRule(w, r)
	if rule == nil {
		return
	}
	out, err := c.ruleDetail(rule, c.operatorShown(r))
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *core) handleGetRuleVersion(w http.ResponseWriter, r *http.Request) {
	rule := c.loadRule(w, r)
	if rule == nil {
		return
	}
	ver, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeError(w, http.StatusNotFound, modules.M("rule.version_not_found"))
		return
	}
	v, err := models.GetRuleVersion(c.db, rule.ID, ver)
	if err != nil {
		writeError(w, http.StatusNotFound, modules.M("rule.version_not_found"))
		return
	}
	content, err := modules.ParseRuleContent([]byte(v.Content))
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ruleVersionJSON{RuleID: rule.ID, Version: v.Version, Content: content, CreatedAt: ts(v.CreatedAt), CreatedBy: c.operatorShown(r)(v.CreatedBy), Comment: v.Comment,
		CommentI18n: versionCommentI18n(v)})
}

func (c *core) handleRuleDiff(w http.ResponseWriter, r *http.Request) {
	rule := c.loadRule(w, r)
	if rule == nil {
		return
	}
	from, err1 := strconv.Atoi(r.URL.Query().Get("from"))
	to, err2 := strconv.Atoi(r.URL.Query().Get("to"))
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusBadRequest, modules.M("rule.diff_from_to"))
		return
	}
	text := func(ver int) (string, error) {
		v, err := models.GetRuleVersion(c.db, rule.ID, ver)
		if err != nil {
			return "", err
		}
		content, err := modules.ParseRuleContent([]byte(v.Content))
		if err != nil {
			return "", err
		}
		return content.JSON(), nil
	}
	a, err := text(from)
	if err != nil {
		writeError(w, http.StatusNotFound, modules.M("rule.version_not_found"))
		return
	}
	b, err := text(to)
	if err != nil {
		writeError(w, http.StatusNotFound, modules.M("rule.version_not_found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "lines": modules.DiffLines(a, b)})
}

// templateContent returns the content of a built-in or administrator template.
func (c *core) templateContent(name string) (*modules.RuleContent, error) {
	if t, ok := modules.FindBuiltinTemplate(name); ok {
		return &t.Content, nil
	}
	t, err := models.GetTemplateByName(c.db, name)
	if err != nil {
		return nil, modules.M("rule.template_not_found_named", "name", strconv.Quote(name))
	}
	return modules.ParseRuleContent([]byte(t.Content))
}

func (c *core) mergeTemplates(names []string, choices map[string]string) (*modules.RuleContent, []modules.MergeConflict, error) {
	if len(names) == 0 {
		return nil, nil, modules.M("rule.select_template")
	}
	var items []modules.NamedContent
	for _, n := range names {
		content, err := c.templateContent(n)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, modules.NamedContent{Name: n, Content: content})
	}
	merged, conflicts := modules.MergeTemplates(items, choices)
	return merged, conflicts, nil
}

func (c *core) handleMergePreview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Templates []string          `json:"templates"`
		Choices   map[string]string `json:"choices"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	merged, conflicts, err := c.mergeTemplates(in.Templates, in.Choices)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": merged, "conflicts": nonNil(conflicts)})
}

func (c *core) handleValidateRule(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content json.RawMessage `json:"content"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	content, err := modules.ParseRuleContent(in.Content)
	if err != nil {
		writeJSON(w, http.StatusOK, problemsBody([]modules.Problem{{Msg: modules.AsMsg(err)}}))
		return
	}
	writeJSON(w, http.StatusOK, problemsBody(content.Problems()))
}

func (c *core) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string            `json:"name"`
		Description string            `json:"description"`
		Templates   []string          `json:"templates"`
		Choices     map[string]string `json:"choices"`
		Content     json.RawMessage   `json:"content"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !ruleNameRe.MatchString(in.Name) {
		writeError(w, http.StatusBadRequest, modules.M("rule.name_invalid"))
		return
	}
	var content *modules.RuleContent
	comment := modules.M("rule.version.created_empty")
	switch {
	case len(in.Templates) > 0:
		merged, conflicts, err := c.mergeTemplates(in.Templates, in.Choices)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if len(conflicts) > 0 {
			fields := make([]string, 0, len(conflicts))
			for _, cf := range conflicts {
				fields = append(fields, cf.Field)
			}
			writeError(w, http.StatusBadRequest, modules.M("rule.choose_template", "fields", fields))
			return
		}
		content = merged
		comment = modules.M("rule.version.from_templates", "names", in.Templates)
	case len(in.Content) > 0:
		parsed, err := modules.ParseRuleContent(in.Content)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		content = parsed
		comment = modules.M("rule.version.created")
	default:
		content = emptyContent()
	}
	if ps := content.Problems(); len(ps) > 0 {
		writeError(w, http.StatusBadRequest, modules.M("common.problems", "problems", ps))
		return
	}
	p := principalFrom(r)
	tx, err := c.db.Begin()
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	defer tx.Rollback()
	rule := &models.Rule{Name: in.Name, Description: strings.TrimSpace(in.Description), CreatedBy: p.operator()}
	if err := models.InsertRule(tx, rule); err != nil {
		if isUniqueErr(err) {
			writeError(w, http.StatusConflict, modules.M("rule.name_taken"))
			return
		}
		c.internalError(w, r, err)
		return
	}
	commentI18n, _ := json.Marshal(comment)
	if err := models.InsertRuleVersion(tx, &models.RuleVersion{RuleID: rule.ID, Version: 1, Content: content.JSON(), Comment: comment.Text(),
		CommentI18n: string(commentI18n), CreatedBy: p.operator()}); err != nil {
		c.internalError(w, r, err)
		return
	}
	if err := tx.Commit(); err != nil {
		c.internalError(w, r, err)
		return
	}
	c.history(p, models.HistRule, "created", rule.Name, comment)
	out, err := c.ruleDetail(rule, c.operatorShown(r))
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *core) handleSaveRule(w http.ResponseWriter, r *http.Request) {
	rule := c.loadRule(w, r)
	if rule == nil {
		return
	}
	var in struct {
		Description *string         `json:"description"`
		Content     json.RawMessage `json:"content"`
		Comment     string          `json:"comment"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	content, err := modules.ParseRuleContent(in.Content)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if ps := content.Problems(); len(ps) > 0 {
		writeError(w, http.StatusBadRequest, modules.M("common.problems", "problems", ps))
		return
	}
	desc := rule.Description
	if in.Description != nil {
		desc = strings.TrimSpace(*in.Description)
	}
	p := principalFrom(r)
	tx, err := c.db.Begin()
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	defer tx.Rollback()
	cur, err := models.GetRule(tx, rule.ID)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	next := cur.LatestVersion + 1
	if err := models.InsertRuleVersion(tx, &models.RuleVersion{RuleID: rule.ID, Version: next, Content: content.JSON(), Comment: strings.TrimSpace(in.Comment), CreatedBy: p.operator()}); err != nil {
		c.internalError(w, r, err)
		return
	}
	if err := models.BumpRuleVersion(tx, rule.ID, next, desc); err != nil {
		c.internalError(w, r, err)
		return
	}
	if err := tx.Commit(); err != nil {
		c.internalError(w, r, err)
		return
	}
	c.history(p, models.HistRule, "saved", rule.Name, modules.M("history.detail.rule_saved", "version", next, "comment", in.Comment))
	rule, _ = models.GetRule(c.db, rule.ID)
	out, err := c.ruleDetail(rule, c.operatorShown(r))
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *core) handleApplyRule(w http.ResponseWriter, r *http.Request) {
	rule := c.loadRule(w, r)
	if rule == nil {
		return
	}
	var in struct {
		Version  int     `json:"version"`
		AgentIDs []int64 `json:"agent_ids"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.Version == 0 {
		in.Version = rule.LatestVersion
	}
	if _, err := models.GetRuleVersion(c.db, rule.ID, in.Version); err != nil {
		writeError(w, http.StatusBadRequest, modules.M("rule.version_not_found"))
		return
	}
	if len(in.AgentIDs) == 0 {
		writeError(w, http.StatusBadRequest, modules.M("rule.select_agent"))
		return
	}
	// An agent given more than once is applied and counted once.
	seen := map[int64]bool{}
	ids := in.AgentIDs[:0]
	for _, id := range in.AgentIDs {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	in.AgentIDs = ids
	p := principalFrom(r)
	var names []string
	for _, id := range in.AgentIDs {
		a, err := models.GetAgent(c.db, id)
		if err != nil {
			writeError(w, http.StatusBadRequest, modules.M("agent.id_not_found", "id", id))
			return
		}
		names = append(names, c.agentDisplayName(a))
	}
	for _, id := range in.AgentIDs {
		if err := models.AssignRule(c.db, id, rule.ID, in.Version); err != nil {
			c.internalError(w, r, err)
			return
		}
	}
	c.history(p, models.HistRule, "applied", rule.Name, modules.M("history.detail.rule_applied", "version", in.Version, "agents", names))
	writeJSON(w, http.StatusOK, map[string]int{"applied": len(in.AgentIDs)})
}

// handleUnassignRule takes the rule away from agents: they remove their
// audit rules at the next sync and report that nothing is applied. An
// agent without a rule is left as it is.
func (c *core) handleUnassignRule(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentIDs []int64 `json:"agent_ids"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(in.AgentIDs) == 0 {
		writeError(w, http.StatusBadRequest, modules.M("rule.select_agent"))
		return
	}
	seen := map[int64]bool{}
	var agents []*models.Agent
	for _, id := range in.AgentIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		a, err := models.GetAgent(c.db, id)
		if err != nil {
			writeError(w, http.StatusBadRequest, modules.M("agent.id_not_found", "id", id))
			return
		}
		agents = append(agents, a)
	}
	p := principalFrom(r)
	n := 0
	for _, a := range agents {
		if !a.AssignedRuleID.Valid {
			continue
		}
		if err := models.UnassignRule(c.db, a.ID); err != nil {
			c.internalError(w, r, err)
			return
		}
		ruleName := modules.M("history.detail.rule_fallback", "id", a.AssignedRuleID.Int64)
		if rule, err := models.GetRule(c.db, a.AssignedRuleID.Int64); err == nil {
			ruleName = modules.Raw(rule.Name)
		}
		c.historyMsg(p, models.HistRule, "unassigned", ruleName,
			modules.M("history.detail.rule_unassigned", "version", a.AssignedRuleVersion, "agent", c.agentDisplayName(a)))
		n++
	}
	writeJSON(w, http.StatusOK, map[string]int{"unassigned": n})
}

func (c *core) handleRuleToTemplate(w http.ResponseWriter, r *http.Request) {
	rule := c.loadRule(w, r)
	if rule == nil {
		return
	}
	var in struct {
		Version     int    `json:"version"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.Version == 0 {
		in.Version = rule.LatestVersion
	}
	v, err := models.GetRuleVersion(c.db, rule.ID, in.Version)
	if err != nil {
		writeError(w, http.StatusBadRequest, modules.M("rule.version_not_found"))
		return
	}
	content, err := modules.ParseRuleContent([]byte(v.Content))
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	c.createTemplate(w, r, in.Name, in.Description, content,
		modules.M("history.detail.template_from_rule_name", "rule", rule.Name, "version", in.Version))
}

func (c *core) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	rule := c.loadRule(w, r)
	if rule == nil {
		return
	}
	n, err := models.CountAgentsUsingRule(c.db, rule.ID)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if n > 0 {
		writeError(w, http.StatusConflict, modules.M("rule.in_use"))
		return
	}
	if err := models.DeleteRule(c.db, rule.ID); err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY") {
			writeError(w, http.StatusConflict, modules.M("rule.in_use"))
			return
		}
		c.internalError(w, r, err)
		return
	}
	c.history(principalFrom(r), models.HistRule, "deleted", rule.Name, modules.Msg{})
	writeJSON(w, http.StatusOK, map[string]any{})
}

// --- templates ---

type templateJSON struct {
	Name            string               `json:"name"`
	Builtin         bool                 `json:"builtin"`
	Description     string               `json:"description"`
	DescriptionI18n *modules.Msg         `json:"description_i18n,omitempty"`
	UpdatedAt       *string              `json:"updated_at"`
	Content         *modules.RuleContent `json:"content,omitempty"`
}

// builtinDescriptionI18n returns the coded description of a built-in
// template (its code is "template.builtin." and the name).
func builtinDescriptionI18n(t *modules.BuiltinTemplate) *modules.Msg {
	m := modules.M("template.builtin." + t.Name)
	return &m
}

func (c *core) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	out := []templateJSON{}
	for _, t := range modules.BuiltinTemplates() {
		out = append(out, templateJSON{Name: t.Name, Builtin: true, Description: t.Description, DescriptionI18n: builtinDescriptionI18n(&t)})
	}
	list, err := models.ListTemplates(c.db)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	for _, t := range list {
		out = append(out, templateJSON{Name: t.Name, Description: t.Description, UpdatedAt: ts(t.UpdatedAt)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *core) handleGetTemplate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if t, ok := modules.FindBuiltinTemplate(name); ok {
		content := t.Content
		writeJSON(w, http.StatusOK, templateJSON{Name: t.Name, Builtin: true, Description: t.Description, DescriptionI18n: builtinDescriptionI18n(t),
			Content: &content})
		return
	}
	t, err := models.GetTemplateByName(c.db, name)
	if err != nil {
		writeError(w, http.StatusNotFound, modules.M("template.not_found"))
		return
	}
	content, err := modules.ParseRuleContent([]byte(t.Content))
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, templateJSON{Name: t.Name, Description: t.Description, UpdatedAt: ts(t.UpdatedAt), Content: content})
}

func (c *core) createTemplate(w http.ResponseWriter, r *http.Request, name, desc string, content *modules.RuleContent, origin modules.Msg) {
	name = strings.TrimSpace(name)
	if !tokenNameRe.MatchString(name) {
		writeError(w, http.StatusBadRequest, modules.M("template.name_invalid"))
		return
	}
	if _, ok := modules.FindBuiltinTemplate(name); ok {
		writeError(w, http.StatusConflict, modules.M("template.builtin_name"))
		return
	}
	if ps := content.Problems(); len(ps) > 0 {
		writeError(w, http.StatusBadRequest, modules.M("common.problems", "problems", ps))
		return
	}
	p := principalFrom(r)
	t := &models.Template{Name: name, Description: strings.TrimSpace(desc), Content: content.JSON(), CreatedBy: p.operator()}
	if err := models.InsertTemplate(c.db, t); err != nil {
		if isUniqueErr(err) {
			writeError(w, http.StatusConflict, modules.M("template.name_taken"))
			return
		}
		c.internalError(w, r, err)
		return
	}
	c.history(p, models.HistTemplate, "created", name, origin)
	writeJSON(w, http.StatusOK, templateJSON{Name: t.Name, Description: t.Description, UpdatedAt: ts(t.UpdatedAt), Content: content})
}

func (c *core) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Content     json.RawMessage `json:"content"`
		FromBuiltin string          `json:"from_builtin"`
		FromRule    *struct {
			RuleID  int64 `json:"rule_id"`
			Version int   `json:"version"`
		} `json:"from_rule"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var content *modules.RuleContent
	origin := modules.M("history.detail.template_created")
	switch {
	case in.FromBuiltin != "":
		t, ok := modules.FindBuiltinTemplate(in.FromBuiltin)
		if !ok {
			writeError(w, http.StatusBadRequest, modules.M("template.builtin_not_found"))
			return
		}
		content = &t.Content
		origin = modules.M("history.detail.template_from_builtin", "name", t.Name)
	case in.FromRule != nil:
		if in.FromRule.Version == 0 {
			// Without a version the latest one is used, as elsewhere.
			if rule, err := models.GetRule(c.db, in.FromRule.RuleID); err == nil {
				in.FromRule.Version = rule.LatestVersion
			}
		}
		v, err := models.GetRuleVersion(c.db, in.FromRule.RuleID, in.FromRule.Version)
		if err != nil {
			writeError(w, http.StatusBadRequest, modules.M("rule.rule_version_not_found"))
			return
		}
		parsed, err := modules.ParseRuleContent([]byte(v.Content))
		if err != nil {
			c.internalError(w, r, err)
			return
		}
		content = parsed
		origin = modules.M("history.detail.template_from_rule_id", "id", in.FromRule.RuleID, "version", in.FromRule.Version)
	case len(in.Content) > 0:
		parsed, err := modules.ParseRuleContent(in.Content)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		content = parsed
	default:
		content = emptyContent()
	}
	c.createTemplate(w, r, in.Name, in.Description, content, origin)
}

func (c *core) handleUpdateTemplate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := modules.FindBuiltinTemplate(name); ok {
		writeError(w, http.StatusForbidden, modules.M("template.builtin_readonly"))
		return
	}
	var in struct {
		Description *string         `json:"description"`
		Content     json.RawMessage `json:"content"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	content, err := modules.ParseRuleContent(in.Content)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if ps := content.Problems(); len(ps) > 0 {
		writeError(w, http.StatusBadRequest, modules.M("common.problems", "problems", ps))
		return
	}
	var desc string
	if in.Description != nil {
		desc = strings.TrimSpace(*in.Description)
	} else {
		// Without a description the stored one is kept.
		cur, err := models.GetTemplateByName(c.db, name)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, modules.M("template.not_found"))
				return
			}
			c.internalError(w, r, err)
			return
		}
		desc = cur.Description
	}
	if err := models.UpdateTemplate(c.db, name, desc, content.JSON()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, modules.M("template.not_found"))
			return
		}
		c.internalError(w, r, err)
		return
	}
	c.history(principalFrom(r), models.HistTemplate, "updated", name, modules.Msg{})
	writeJSON(w, http.StatusOK, templateJSON{Name: name, Description: desc, UpdatedAt: ts(time.Now().Unix()), Content: content})
}

func (c *core) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := modules.FindBuiltinTemplate(name); ok {
		writeError(w, http.StatusForbidden, modules.M("template.builtin_undeletable"))
		return
	}
	if err := models.DeleteTemplate(c.db, name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, modules.M("template.not_found"))
			return
		}
		c.internalError(w, r, err)
		return
	}
	c.history(principalFrom(r), models.HistTemplate, "deleted", name, modules.Msg{})
	writeJSON(w, http.StatusOK, map[string]any{})
}
