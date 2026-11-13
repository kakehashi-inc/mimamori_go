package workers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mimamori/app/models"
	"mimamori/app/modules"
)

type fileJSON struct {
	Ctime *string `json:"ctime"`
	Owner string  `json:"owner"`
	Mode  string  `json:"mode"`
}

type detectionJSON struct {
	ID             int64     `json:"id"`
	AgentID        int64     `json:"agent_id"`
	AgentName      string    `json:"agent_name"`
	Hostname       string    `json:"hostname"`
	Type           string    `json:"type"`
	Category       string    `json:"category"`
	OSUser         string    `json:"os_user"`
	Exe            string    `json:"exe"`
	Target         string    `json:"target"`
	Count          int       `json:"count"`
	FirstAt        *string   `json:"first_at"`
	LastAt         *string   `json:"last_at"`
	File           *fileJSON `json:"file"`
	Suppressed     bool      `json:"suppressed"`
	SuppressionID  *int64    `json:"suppression_id"`
	AcknowledgedAt *string   `json:"acknowledged_at"`
	AcknowledgedBy string    `json:"acknowledged_by"`
	NotifyState    string    `json:"notify_state"`
	lastAt         int64
}

// detectionOut converts a detection for the API; show converts the recorded
// operator (see operatorShown).
func (c *core) detectionOut(a *models.Agent, name string, d *models.Detection, show func(string) string) detectionJSON {
	out := detectionJSON{ID: d.ID, AgentID: a.ID, AgentName: name, Hostname: a.Hostname, Type: d.Type, Category: d.Category,
		OSUser: d.OSUser, Exe: d.Exe, Target: d.Target, Count: d.Count, FirstAt: ts(d.FirstAt), LastAt: ts(d.LastAt),
		Suppressed: d.Suppressed, AcknowledgedAt: ts(d.AcknowledgedAt), AcknowledgedBy: show(d.AcknowledgedBy),
		NotifyState: d.NotifyState, lastAt: d.LastAt}
	if a.Kind == models.AgentBuiltin && out.Hostname == "" {
		out.Hostname = c.hostname
	}
	if d.SuppressionID > 0 {
		id := d.SuppressionID
		out.SuppressionID = &id
	}
	if d.Type == modules.TypeWritable || d.Type == modules.TypeExecFile {
		out.File = &fileJSON{Ctime: ts(d.FileCtime), Owner: d.FileOwner, Mode: d.FileMode}
	}
	return out
}

// boolQuery reads an optional true/false query parameter. An empty or
// absent value means "not given"; any other unknown value is an error.
func boolQuery(r *http.Request, name string) (*bool, error) {
	switch v := r.URL.Query().Get(name); v {
	case "":
		return nil, nil
	case "true", "1":
		b := true
		return &b, nil
	case "false", "0":
		b := false
		return &b, nil
	default:
		return nil, modules.M("common.query_not_bool", "name", name, "value", strconv.Quote(v))
	}
}

// handleListDetections merges detections from the per-target databases.
func (c *core) handleListDetections(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := models.DetectionFilter{Category: q.Get("category"), Type: q.Get("type")}
	var errs [7]error
	f.From, errs[0] = parseTime("from", q.Get("from"))
	f.To, errs[1] = parseTime("to", q.Get("to"))
	f.Acknowledged, errs[2] = boolQuery(r, "acknowledged")
	f.Suppressed, errs[3] = boolQuery(r, "suppressed")
	var limit, offset int
	limit, errs[4] = queryInt(r, "limit", 100, 1, 500)
	offset, errs[5] = queryInt(r, "offset", 0, 0, 1000000)
	var agentID int64
	if v := q.Get("agent_id"); v != "" {
		if agentID, errs[6] = strconv.ParseInt(v, 10, 64); errs[6] != nil {
			errs[6] = modules.M("common.query_agent_id", "value", strconv.Quote(v))
		}
	}
	for _, err := range errs {
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	agents, err := models.ListAgents(c.db)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if q.Get("agent_id") != "" {
		agents = slices.DeleteFunc(agents, func(a *models.Agent) bool { return a.ID != agentID })
	}
	var items []detectionJSON
	total := 0
	show := c.operatorShown(r)
	for _, a := range agents {
		tdb, err := c.targetDB(a)
		if err != nil {
			// The list of one target fails with its database; the list
			// of every target shows the others.
			if q.Get("agent_id") != "" {
				c.internalError(w, r, err)
				return
			}
			c.logger.Printf("target database of agent %d: %v", a.ID, err)
			continue
		}
		list, n, err := models.ListDetections(tdb.Read(), f, offset+limit)
		if err != nil {
			c.internalError(w, r, err)
			return
		}
		total += n
		name := c.agentDisplayName(a)
		for _, d := range list {
			items = append(items, c.detectionOut(a, name, d, show))
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].lastAt > items[j].lastAt })
	if offset >= len(items) {
		items = nil
	} else {
		items = items[offset:min(len(items), offset+limit)]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(items), "total": total})
}

func (c *core) handleGetDetection(w http.ResponseWriter, r *http.Request) {
	a := c.loadAgent(w, r, "agent")
	if a == nil {
		return
	}
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, modules.M("detection.not_found"))
		return
	}
	tdb, err := c.targetDB(a)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	d, err := models.GetDetection(tdb.Read(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, modules.M("detection.not_found"))
		return
	}
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c.detectionOut(a, c.agentDisplayName(a), d, c.operatorShown(r)))
}

func (c *core) handleAckDetections(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Items []struct {
			AgentID int64 `json:"agent_id"`
			ID      int64 `json:"id"`
		} `json:"items"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	p := principalFrom(r)
	now := time.Now().Unix()
	count := 0
	perAgent := map[string]int{}
	for _, it := range in.Items {
		a, err := models.GetAgent(c.db, it.AgentID)
		if err != nil {
			continue
		}
		tdb, err := c.targetDB(a)
		if err != nil {
			continue
		}
		var ok bool
		err = tdb.Write(func(tx models.Writer) error {
			ok, err = models.AcknowledgeDetection(tx, it.ID, p.operator(), now)
			return err
		})
		if err != nil {
			c.internalError(w, r, err)
			return
		}
		if ok {
			count++
			perAgent[c.agentDisplayName(a)]++
		}
	}
	for name, n := range perAgent {
		c.history(p, models.HistDetection, "acknowledged", name, modules.M("history.detail.acknowledged", "count", n))
	}
	writeJSON(w, http.StatusOK, map[string]int{"acknowledged": count})
}

// --- suppressions ---

type suppressionJSON struct {
	ID        int64   `json:"id"`
	AgentID   *int64  `json:"agent_id"`
	AgentName string  `json:"agent_name"`
	Type      string  `json:"type"`
	OSUser    string  `json:"os_user"`
	Exe       string  `json:"exe"`
	Target    string  `json:"target"`
	Comment   string  `json:"comment"`
	CreatedAt *string `json:"created_at"`
	CreatedBy string  `json:"created_by"`
}

// suppressionOut converts a suppression condition for the API; show converts
// the recorded operator (see operatorShown).
func (c *core) suppressionOut(s *models.Suppression, show func(string) string) suppressionJSON {
	out := suppressionJSON{ID: s.ID, Type: s.Type, OSUser: s.OSUser, Exe: s.Exe, Target: s.Target, Comment: s.Comment,
		CreatedAt: ts(s.CreatedAt), CreatedBy: show(s.CreatedBy)}
	if s.AgentID.Valid {
		id := s.AgentID.Int64
		out.AgentID = &id
		if a, err := models.GetAgent(c.db, id); err == nil {
			out.AgentName = c.agentDisplayName(a)
		}
	}
	return out
}

func (c *core) handleListSuppressions(w http.ResponseWriter, r *http.Request) {
	list, err := models.ListSuppressions(c.db)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	out := make([]suppressionJSON, 0, len(list))
	show := c.operatorShown(r)
	for _, s := range list {
		out = append(out, c.suppressionOut(s, show))
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *core) handleCreateSuppression(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID *int64 `json:"agent_id"`
		Type    string `json:"type"`
		OSUser  string `json:"os_user"`
		Exe     string `json:"exe"`
		Target  string `json:"target"`
		Comment string `json:"comment"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s := &models.Suppression{Type: strings.TrimSpace(in.Type), OSUser: strings.TrimSpace(in.OSUser), Exe: strings.TrimSpace(in.Exe),
		Target: strings.TrimSpace(in.Target), Comment: strings.TrimSpace(in.Comment), CreatedBy: principalFrom(r).operator()}
	if s.Type != "" && !slices.Contains(modules.DetectionTypes, s.Type) {
		writeError(w, http.StatusBadRequest, modules.M("suppression.type_invalid"))
		return
	}
	if s.Type == "" && s.OSUser == "" && s.Exe == "" && s.Target == "" {
		writeError(w, http.StatusBadRequest, modules.M("suppression.condition_required"))
		return
	}
	if err := validateSuppressionFields(s); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var agentName = "all agents"
	if in.AgentID != nil && *in.AgentID > 0 {
		a, err := models.GetAgent(c.db, *in.AgentID)
		if err != nil {
			writeError(w, http.StatusBadRequest, modules.M("agent.not_found"))
			return
		}
		s.AgentID = sql.NullInt64{Int64: a.ID, Valid: true}
		agentName = c.agentDisplayName(a)
	}
	if err := models.InsertSuppression(c.db, s); err != nil {
		c.internalError(w, r, err)
		return
	}
	n := c.applySuppression(s)
	c.history(principalFrom(r), models.HistSuppression, "created", agentName,
		modules.M("history.detail.suppression_created", "cond", describeSuppression(s), "count", n))
	writeJSON(w, http.StatusOK, c.suppressionOut(s, c.operatorShown(r)))
}

// Limits of the text fields of a suppression condition.
const (
	suppressionMaxPathBytes     = 4096 // exe and target
	suppressionMaxOSUserBytes   = 256
	suppressionMaxCommentLength = 1000 // characters
)

// validateSuppressionFields rejects control characters (NUL, CR, LF, tab and
// the like) and over-long values in the text fields of a suppression.
func validateSuppressionFields(s *models.Suppression) error {
	for _, f := range []struct {
		name, value string
		max         int
		chars       bool
	}{
		{"os_user", s.OSUser, suppressionMaxOSUserBytes, false},
		{"exe", s.Exe, suppressionMaxPathBytes, false},
		{"target", s.Target, suppressionMaxPathBytes, false},
		{"comment", s.Comment, suppressionMaxCommentLength, true},
	} {
		if strings.IndexFunc(f.value, unicode.IsControl) >= 0 {
			return modules.M("suppression.field_control", "field", f.name)
		}
		if f.chars && utf8.RuneCountInString(f.value) > f.max {
			return modules.M("suppression.field_too_long_chars", "field", f.name, "max", f.max)
		}
		if !f.chars && len(f.value) > f.max {
			return modules.M("suppression.field_too_long_bytes", "field", f.name, "max", f.max)
		}
	}
	return nil
}

func describeSuppression(s *models.Suppression) string {
	var parts []string
	for _, kv := range [][2]string{{"type", s.Type}, {"user", s.OSUser}, {"exe", s.Exe}, {"target", s.Target}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+kv[1])
		}
	}
	return strings.Join(parts, " ")
}

// applySuppression marks open detections matching a new suppression so they
// are not notified.
func (c *core) applySuppression(s *models.Suppression) int {
	agents, err := models.ListAgents(c.db)
	if err != nil {
		return 0
	}
	n := 0
	f := false
	for _, a := range agents {
		if s.AgentID.Valid && s.AgentID.Int64 != a.ID {
			continue
		}
		tdb, err := c.targetDB(a)
		if err != nil {
			continue
		}
		// The detections are listed and marked in one transaction, so a
		// detection acknowledged meanwhile is not marked.
		marked := 0
		err = tdb.Write(func(tx models.Writer) error {
			list, _, err := models.ListDetections(tx, models.DetectionFilter{Acknowledged: &f, Suppressed: &f}, 100000)
			if err != nil {
				return err
			}
			for _, d := range list {
				if !matchSuppression(s, a.ID, d.Type, d.OSUser, d.Exe, d.Target) {
					continue
				}
				if models.MarkDetectionSuppressed(tx, d.ID, s.ID) == nil {
					marked++
				}
			}
			return nil
		})
		if err == nil {
			n += marked
		}
	}
	return n
}

// releaseSuppression runs after a suppression was deleted: the detections it
// suppressed that are not acknowledged become not suppressed again, unless
// another suppression matches them (they are then recorded under that one).
// A released detection that was skipped takes part in the next notification
// unless the same detection was notified within the dedupe period, as when a
// new occurrence is merged. Acknowledged detections stay as they are. The
// detections of one target change in one transaction. It returns the number
// of released detections.
func (c *core) releaseSuppression(s *models.Suppression) int {
	agents, err := models.ListAgents(c.db)
	if err != nil {
		c.logger.Printf("release suppression %d: %v", s.ID, err)
		return 0
	}
	sups, err := models.ListSuppressions(c.db)
	if err != nil {
		c.logger.Printf("release suppression %d: %v", s.ID, err)
		return 0
	}
	n := 0
	for _, a := range agents {
		if s.AgentID.Valid && s.AgentID.Int64 != a.ID {
			continue
		}
		tdb, err := c.targetDB(a)
		if err != nil {
			continue
		}
		released, err := c.releaseSuppressionOf(tdb, a.ID, s.ID, sups)
		if err != nil {
			c.logger.Printf("release suppression %d: agent %d: %v", s.ID, a.ID, err)
			continue
		}
		n += released
	}
	return n
}

// releaseSuppressionOf does releaseSuppression for the detections of one
// target, in one transaction.
func (c *core) releaseSuppressionOf(tdb *modules.DB, agentID, suppressionID int64, sups []*models.Suppression) (int, error) {
	now := time.Now().Unix()
	dedupe := int64(c.conf().Notify.DedupeHours) * 3600
	n := 0
	err := tdb.Write(func(tx models.Writer) error {
		t, f := true, false
		list, _, err := models.ListDetections(tx, models.DetectionFilter{Acknowledged: &f, Suppressed: &t}, 100000)
		if err != nil {
			return err
		}
		for _, d := range list {
			if d.SuppressionID != suppressionID {
				continue
			}
			if other := findSuppression(sups, agentID, d.Type, d.OSUser, d.Exe, d.Target); other != nil {
				if err := models.MoveDetectionSuppression(tx, d.ID, suppressionID, other.ID); err != nil {
					return err
				}
				continue
			}
			state := d.NotifyState
			if state == models.NotifySkipped && now-models.LastNotifiedAt(tx, d.Fingerprint) >= dedupe {
				state = models.NotifyPending
			}
			if err := models.ReleaseDetectionSuppression(tx, d.ID, suppressionID, state); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (c *core) handleDeleteSuppression(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, modules.M("suppression.not_found"))
		return
	}
	s, err := models.GetSuppression(c.db, id)
	if err != nil {
		writeError(w, http.StatusNotFound, modules.M("suppression.not_found"))
		return
	}
	if err := models.DeleteSuppression(c.db, id); err != nil {
		c.internalError(w, r, err)
		return
	}
	n := c.releaseSuppression(s)
	c.history(principalFrom(r), models.HistSuppression, "deleted", strconv.FormatInt(id, 10),
		modules.M("history.detail.suppression_deleted", "cond", describeSuppression(s), "count", n))
	writeJSON(w, http.StatusOK, map[string]any{})
}

// --- certificates, history, overview, guide ---

type certInfoJSON struct {
	Fingerprint string  `json:"fingerprint"`
	Subject     string  `json:"subject"`
	NotBefore   *string `json:"not_before"`
	NotAfter    *string `json:"not_after"`
	Status      string  `json:"status"`
}

func certOut(x *models.Certificate) certInfoJSON {
	return certInfoJSON{Fingerprint: modules.FormatFingerprint(x.Fingerprint), Subject: x.Subject, NotBefore: ts(x.NotBefore), NotAfter: ts(x.NotAfter), Status: x.Status}
}

func (c *core) handleCertificates(w http.ResponseWriter, r *http.Request) {
	cas, err := models.ListCertificates(c.db, "ca")
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	servers, _ := models.ListCertificates(c.db, "server")
	out := map[string]any{}
	caList := []certInfoJSON{}
	var next *models.Certificate
	for _, ca := range cas {
		caList = append(caList, certOut(ca))
		if ca.Status == models.CertNext {
			next = ca
		}
	}
	out["cas"] = caList
	if cur := pickStatus(servers, models.CertActive); cur != nil {
		var names []string
		_ = json.Unmarshal([]byte(cur.Names), &names)
		out["server"] = map[string]any{
			"fingerprint": modules.FormatFingerprint(cur.Fingerprint), "subject": cur.Subject, "not_before": ts(cur.NotBefore),
			"not_after": ts(cur.NotAfter), "status": cur.Status, "names": nonNil(names), "issuer_fingerprint": modules.FormatFingerprint(cur.IssuerFP),
		}
	}
	rotation := map[string]any{"state": "none", "next_ca_created_at": nil, "switch_after": nil}
	missing := []map[string]any{}
	if next != nil {
		rotation["state"] = "distributing"
		rotation["next_ca_created_at"] = ts(next.CreatedAt)
		rotation["switch_after"] = ts(next.CreatedAt + int64(modules.CASwitchAfter/time.Second))
	} else {
		for _, ca := range cas {
			if ca.Status == models.CertRetired {
				rotation["state"] = "switched"
			}
		}
	}
	latest := c.latestCAFingerprints()
	agents, _ := models.ListAgents(c.db)
	for _, a := range agents {
		if a.Kind != models.AgentToken || a.LastSyncAt == 0 {
			continue
		}
		var fps []string
		_ = json.Unmarshal([]byte(a.CAFingerprints), &fps)
		for _, fp := range latest {
			if !slices.Contains(fps, fp) {
				missing = append(missing, map[string]any{"agent_id": a.ID, "display_name": c.agentDisplayName(a)})
				break
			}
		}
	}
	out["rotation"] = rotation
	out["agents_missing_new_ca"] = missing
	writeJSON(w, http.StatusOK, out)
}

type historyJSON struct {
	ID          int64   `json:"id"`
	At          *string `json:"at"`
	AccountName string  `json:"account_name"`
	Category    string  `json:"category"`
	Action      string  `json:"action"`
	Target      string  `json:"target"`
	Detail      string  `json:"detail"`
	// TargetI18n and DetailI18n are the coded target and detail, present
	// only when the row keeps a code whose English is the recorded text
	// (a target that is a plain value and rows of older builds have none).
	TargetI18n *modules.Msg `json:"target_i18n,omitempty"`
	DetailI18n *modules.Msg `json:"detail_i18n,omitempty"`
	RemoteAddr string       `json:"remote_addr"`
	Count      int          `json:"count"`
}

// historyOut converts a history entry for the API; show converts the
// recorded operator (see operatorShown).
func historyOut(h *models.HistoryItem, show func(string) string) historyJSON {
	coded := modules.ParseHistoryTexts(h.I18n, h.Target, h.Detail)
	return historyJSON{ID: h.ID, At: ts(h.LastAt), AccountName: show(h.AccountName), Category: h.Category, Action: h.Action,
		Target: h.Target, Detail: h.Detail, TargetI18n: coded.Target, DetailI18n: coded.Detail, RemoteAddr: h.RemoteAddr, Count: h.Count}
}

// viewerHiddenHistory are the history categories a viewer does not receive.
var viewerHiddenHistory = []string{models.HistAccount, models.HistConfig, models.HistCommand}

func (c *core) handleHistory(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 100, 1, 500)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	offset, err := queryInt(r, "offset", 0, 0, 10000000)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Viewers do not receive the history of accounts, of the system
	// settings and of command sets (it lists every account, the configured
	// values and changes of definitions that viewers do not see).
	var exclude []string
	if p := principalFrom(r); !p.isAdmin() {
		exclude = viewerHiddenHistory
	}
	items, total, err := models.ListHistory(c.db, r.URL.Query().Get("category"), "", exclude, limit, offset)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	out := make([]historyJSON, 0, len(items))
	show := c.operatorShown(r)
	for _, h := range items {
		out = append(out, historyOut(h, show))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "total": total})
}

func (c *core) handleOverview(w http.ResponseWriter, r *http.Request) {
	running, msg := c.builtinStatus()
	// enabled is the configuration the central started with: a built-in
	// agent disabled there is the administrator's choice, so the screens do
	// not report it as stopped.
	enabled := *c.conf().BuiltinAgent
	agents, _ := models.ListAgents(c.db)
	unacked := 0
	for _, a := range agents {
		if tdb, err := c.targetDB(a); err == nil {
			n, _ := models.CountUnackedAlerts(tdb.Read())
			unacked += n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": modules.AppVersion, "central_hostname": c.hostname,
		"builtin_agent": map[string]any{"enabled": enabled, "running": running, "message": msg.Text(), "message_i18n": msgOrNil(msg)},
		"agents":        len(agents), "unacked_alerts": unacked,
	})
}

func (c *core) handleGuide(w http.ResponseWriter, r *http.Request) {
	_, portText, _ := net.SplitHostPort(c.conf().AgentListen)
	port, _ := strconv.Atoi(portText)
	// The host names an agent can use for the central are the names of the
	// server certificate (the loopback names only work on the central host).
	var hosts []string
	for _, n := range modules.ServerCertNames(c.conf().ServerNames) {
		if n == "127.0.0.1" || n == "::1" || n == "localhost" {
			continue
		}
		hosts = append(hosts, n)
	}
	fp := ""
	for _, ca := range c.trustedCAs() {
		if ca.Status == models.CertActive {
			fp = modules.FormatFingerprint(ca.Fingerprint)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": modules.AppVersion, "central_hostname": c.hostname, "central_hosts": nonNil(hosts), "central_port": port,
		"ca_fingerprint": fp, "default_valid_days": c.conf().TokenValidDays,
	})
}
