package workers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	Version   int     `json:"version"`
	CreatedAt *string `json:"created_at"`
	CreatedBy string  `json:"created_by"`
	Comment   string  `json:"comment"`
}

type ruleDetailJSON struct {
	ruleSummaryJSON
	Content  *modules.RuleContent  `json:"content"`
	Versions []ruleVersionMetaJSON `json:"versions"`
}

type ruleVersionJSON struct {
	RuleID    int64                `json:"rule_id"`
	Version   int                  `json:"version"`
	Content   *modules.RuleContent `json:"content"`
	CreatedAt *string              `json:"created_at"`
	CreatedBy string               `json:"created_by"`
	Comment   string               `json:"comment"`
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
		writeError(w, http.StatusNotFound, "rule not found")
		return nil
	}
	rule, err := models.GetRule(c.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "rule not found")
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
		out.Versions = append(out.Versions, ruleVersionMetaJSON{Version: v.Version, CreatedAt: ts(v.CreatedAt), CreatedBy: show(v.CreatedBy), Comment: v.Comment})
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
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	v, err := models.GetRuleVersion(c.db, rule.ID, ver)
	if err != nil {
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	content, err := modules.ParseRuleContent([]byte(v.Content))
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ruleVersionJSON{RuleID: rule.ID, Version: v.Version, Content: content, CreatedAt: ts(v.CreatedAt), CreatedBy: c.operatorShown(r)(v.CreatedBy), Comment: v.Comment})
}

func (c *core) handleRuleDiff(w http.ResponseWriter, r *http.Request) {
	rule := c.loadRule(w, r)
	if rule == nil {
		return
	}
	from, err1 := strconv.Atoi(r.URL.Query().Get("from"))
	to, err2 := strconv.Atoi(r.URL.Query().Get("to"))
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusBadRequest, "from and to are required")
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
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	b, err := text(to)
	if err != nil {
		writeError(w, http.StatusNotFound, "version not found")
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
		return nil, fmt.Errorf("template %q not found", name)
	}
	return modules.ParseRuleContent([]byte(t.Content))
}

func (c *core) mergeTemplates(names []string, choices map[string]string) (*modules.RuleContent, []modules.MergeConflict, error) {
	if len(names) == 0 {
		return nil, nil, errors.New("select at least one template")
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
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	merged, conflicts, err := c.mergeTemplates(in.Templates, in.Choices)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": merged, "conflicts": nonNil(conflicts)})
}

func (c *core) handleValidateRule(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content json.RawMessage `json:"content"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	content, err := modules.ParseRuleContent(in.Content)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"errors": []string{err.Error()}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"errors": nonNil(content.Validate())})
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
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !ruleNameRe.MatchString(in.Name) {
		writeError(w, http.StatusBadRequest, "rule name must be 1-100 characters")
		return
	}
	var content *modules.RuleContent
	comment := "created empty"
	switch {
	case len(in.Templates) > 0:
		merged, conflicts, err := c.mergeTemplates(in.Templates, in.Choices)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(conflicts) > 0 {
			fields := make([]string, 0, len(conflicts))
			for _, cf := range conflicts {
				fields = append(fields, cf.Field)
			}
			writeError(w, http.StatusBadRequest, "choose the template for: "+strings.Join(fields, ", "))
			return
		}
		content = merged
		comment = "expanded from templates: " + strings.Join(in.Templates, ", ")
	case len(in.Content) > 0:
		parsed, err := modules.ParseRuleContent(in.Content)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		content = parsed
		comment = "created"
	default:
		content = emptyContent()
	}
	if errs := content.Validate(); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(errs, "; "))
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
			writeError(w, http.StatusConflict, "a rule with this name already exists")
			return
		}
		c.internalError(w, r, err)
		return
	}
	if err := models.InsertRuleVersion(tx, &models.RuleVersion{RuleID: rule.ID, Version: 1, Content: content.JSON(), Comment: comment, CreatedBy: p.operator()}); err != nil {
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
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	content, err := modules.ParseRuleContent(in.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errs := content.Validate(); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(errs, "; "))
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
	c.history(p, models.HistRule, "saved", rule.Name, "version "+itoa(next)+": "+in.Comment)
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
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Version == 0 {
		in.Version = rule.LatestVersion
	}
	if _, err := models.GetRuleVersion(c.db, rule.ID, in.Version); err != nil {
		writeError(w, http.StatusBadRequest, "version not found")
		return
	}
	if len(in.AgentIDs) == 0 {
		writeError(w, http.StatusBadRequest, "select at least one agent")
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
			writeError(w, http.StatusBadRequest, "agent "+itoa64(id)+" not found")
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
	c.history(p, models.HistRule, "applied", rule.Name, fmt.Sprintf("version %d to %s", in.Version, strings.Join(names, ", ")))
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
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(in.AgentIDs) == 0 {
		writeError(w, http.StatusBadRequest, "select at least one agent")
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
			writeError(w, http.StatusBadRequest, "agent "+itoa64(id)+" not found")
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
		ruleName := "rule " + itoa64(a.AssignedRuleID.Int64)
		if rule, err := models.GetRule(c.db, a.AssignedRuleID.Int64); err == nil {
			ruleName = rule.Name
		}
		c.history(p, models.HistRule, "unassigned", ruleName, fmt.Sprintf("version %d from %s", a.AssignedRuleVersion, c.agentDisplayName(a)))
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
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Version == 0 {
		in.Version = rule.LatestVersion
	}
	v, err := models.GetRuleVersion(c.db, rule.ID, in.Version)
	if err != nil {
		writeError(w, http.StatusBadRequest, "version not found")
		return
	}
	content, err := modules.ParseRuleContent([]byte(v.Content))
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	c.createTemplate(w, r, in.Name, in.Description, content, fmt.Sprintf("from rule %s version %d", rule.Name, in.Version))
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
		writeError(w, http.StatusConflict, "the rule is applied to an agent and cannot be deleted")
		return
	}
	if err := models.DeleteRule(c.db, rule.ID); err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY") {
			writeError(w, http.StatusConflict, "the rule is applied to an agent and cannot be deleted")
			return
		}
		c.internalError(w, r, err)
		return
	}
	c.history(principalFrom(r), models.HistRule, "deleted", rule.Name, "")
	writeJSON(w, http.StatusOK, map[string]any{})
}

// --- templates ---

type templateJSON struct {
	Name        string               `json:"name"`
	Builtin     bool                 `json:"builtin"`
	Description string               `json:"description"`
	UpdatedAt   *string              `json:"updated_at"`
	Content     *modules.RuleContent `json:"content,omitempty"`
}

func (c *core) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	out := []templateJSON{}
	for _, t := range modules.BuiltinTemplates() {
		out = append(out, templateJSON{Name: t.Name, Builtin: true, Description: t.Description})
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
		writeJSON(w, http.StatusOK, templateJSON{Name: t.Name, Builtin: true, Description: t.Description, Content: &content})
		return
	}
	t, err := models.GetTemplateByName(c.db, name)
	if err != nil {
		writeError(w, http.StatusNotFound, "template not found")
		return
	}
	content, err := modules.ParseRuleContent([]byte(t.Content))
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, templateJSON{Name: t.Name, Description: t.Description, UpdatedAt: ts(t.UpdatedAt), Content: content})
}

func (c *core) createTemplate(w http.ResponseWriter, r *http.Request, name, desc string, content *modules.RuleContent, origin string) {
	name = strings.TrimSpace(name)
	if !tokenNameRe.MatchString(name) {
		writeError(w, http.StatusBadRequest, "template name must be 1-64 characters of A-Z a-z 0-9 . _ -")
		return
	}
	if _, ok := modules.FindBuiltinTemplate(name); ok {
		writeError(w, http.StatusConflict, "the name of a built-in template cannot be used")
		return
	}
	if errs := content.Validate(); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(errs, "; "))
		return
	}
	p := principalFrom(r)
	t := &models.Template{Name: name, Description: strings.TrimSpace(desc), Content: content.JSON(), CreatedBy: p.operator()}
	if err := models.InsertTemplate(c.db, t); err != nil {
		if isUniqueErr(err) {
			writeError(w, http.StatusConflict, "a template with this name already exists")
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
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var content *modules.RuleContent
	origin := "created"
	switch {
	case in.FromBuiltin != "":
		t, ok := modules.FindBuiltinTemplate(in.FromBuiltin)
		if !ok {
			writeError(w, http.StatusBadRequest, "built-in template not found")
			return
		}
		content = &t.Content
		origin = "from built-in template " + t.Name
	case in.FromRule != nil:
		if in.FromRule.Version == 0 {
			// Without a version the latest one is used, as elsewhere.
			if rule, err := models.GetRule(c.db, in.FromRule.RuleID); err == nil {
				in.FromRule.Version = rule.LatestVersion
			}
		}
		v, err := models.GetRuleVersion(c.db, in.FromRule.RuleID, in.FromRule.Version)
		if err != nil {
			writeError(w, http.StatusBadRequest, "rule version not found")
			return
		}
		parsed, err := modules.ParseRuleContent([]byte(v.Content))
		if err != nil {
			c.internalError(w, r, err)
			return
		}
		content = parsed
		origin = fmt.Sprintf("from rule %d version %d", in.FromRule.RuleID, in.FromRule.Version)
	case len(in.Content) > 0:
		parsed, err := modules.ParseRuleContent(in.Content)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
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
		writeError(w, http.StatusForbidden, "built-in templates cannot be changed")
		return
	}
	var in struct {
		Description *string         `json:"description"`
		Content     json.RawMessage `json:"content"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	content, err := modules.ParseRuleContent(in.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errs := content.Validate(); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(errs, "; "))
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
				writeError(w, http.StatusNotFound, "template not found")
				return
			}
			c.internalError(w, r, err)
			return
		}
		desc = cur.Description
	}
	if err := models.UpdateTemplate(c.db, name, desc, content.JSON()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "template not found")
			return
		}
		c.internalError(w, r, err)
		return
	}
	c.history(principalFrom(r), models.HistTemplate, "updated", name, "")
	writeJSON(w, http.StatusOK, templateJSON{Name: name, Description: desc, UpdatedAt: ts(time.Now().Unix()), Content: content})
}

func (c *core) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := modules.FindBuiltinTemplate(name); ok {
		writeError(w, http.StatusForbidden, "built-in templates cannot be deleted")
		return
	}
	if err := models.DeleteTemplate(c.db, name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "template not found")
			return
		}
		c.internalError(w, r, err)
		return
	}
	c.history(principalFrom(r), models.HistTemplate, "deleted", name, "")
	writeJSON(w, http.StatusOK, map[string]any{})
}
