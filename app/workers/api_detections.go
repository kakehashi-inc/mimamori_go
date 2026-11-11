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

func (c *core) detectionOut(a *models.Agent, name string, d *models.Detection) detectionJSON {
	out := detectionJSON{ID: d.ID, AgentID: a.ID, AgentName: name, Hostname: a.Hostname, Type: d.Type, Category: d.Category,
		OSUser: d.OSUser, Exe: d.Exe, Target: d.Target, Count: d.Count, FirstAt: ts(d.FirstAt), LastAt: ts(d.LastAt),
		Suppressed: d.Suppressed, AcknowledgedAt: ts(d.AcknowledgedAt), AcknowledgedBy: d.AcknowledgedBy,
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

func boolQuery(r *http.Request, name string) *bool {
	switch r.URL.Query().Get(name) {
	case "true", "1":
		v := true
		return &v
	case "false", "0":
		v := false
		return &v
	}
	return nil
}

// handleListDetections merges detections from the per-target databases.
func (c *core) handleListDetections(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := models.DetectionFilter{
		Category: q.Get("category"), Type: q.Get("type"),
		From: parseTime(q.Get("from")), To: parseTime(q.Get("to")),
		Acknowledged: boolQuery(r, "acknowledged"), Suppressed: boolQuery(r, "suppressed"),
	}
	limit := queryInt(r, "limit", 100, 1, 500)
	offset := queryInt(r, "offset", 0, 0, 1000000)
	agents, err := models.ListAgents(c.db)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	if v := q.Get("agent_id"); v != "" {
		id, _ := strconv.ParseInt(v, 10, 64)
		agents = slices.DeleteFunc(agents, func(a *models.Agent) bool { return a.ID != id })
	}
	var items []detectionJSON
	total := 0
	for _, a := range agents {
		tdb, err := c.targetDB(a)
		if err != nil {
			c.logger.Printf("target database of agent %d: %v", a.ID, err)
			continue
		}
		list, n, err := models.ListDetections(tdb, f, offset+limit)
		if err != nil {
			c.internalError(w, r, err)
			return
		}
		total += n
		name := c.agentDisplayName(a)
		for _, d := range list {
			items = append(items, c.detectionOut(a, name, d))
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
		writeError(w, http.StatusNotFound, "detection not found")
		return
	}
	tdb, err := c.targetDB(a)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	d, err := models.GetDetection(tdb, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "detection not found")
		return
	}
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c.detectionOut(a, c.agentDisplayName(a), d))
}

func (c *core) handleAckDetections(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Items []struct {
			AgentID int64 `json:"agent_id"`
			ID      int64 `json:"id"`
		} `json:"items"`
	}
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
		ok, err := models.AcknowledgeDetection(tdb, it.ID, p.Name, now)
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
		c.history(p, models.HistDetection, "acknowledged", name, strconv.Itoa(n)+" detection(s)")
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

func (c *core) suppressionOut(s *models.Suppression) suppressionJSON {
	out := suppressionJSON{ID: s.ID, Type: s.Type, OSUser: s.OSUser, Exe: s.Exe, Target: s.Target, Comment: s.Comment,
		CreatedAt: ts(s.CreatedAt), CreatedBy: s.CreatedBy}
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
	for _, s := range list {
		out = append(out, c.suppressionOut(s))
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
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s := &models.Suppression{Type: strings.TrimSpace(in.Type), OSUser: strings.TrimSpace(in.OSUser), Exe: strings.TrimSpace(in.Exe),
		Target: strings.TrimSpace(in.Target), Comment: strings.TrimSpace(in.Comment), CreatedBy: principalFrom(r).Name}
	if s.Type != "" && !slices.Contains(modules.DetectionTypes, s.Type) {
		writeError(w, http.StatusBadRequest, "unknown detection type")
		return
	}
	if s.Type == "" && s.OSUser == "" && s.Exe == "" && s.Target == "" {
		writeError(w, http.StatusBadRequest, "at least one condition is required")
		return
	}
	var agentName = "all agents"
	if in.AgentID != nil && *in.AgentID > 0 {
		a, err := models.GetAgent(c.db, *in.AgentID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "agent not found")
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
		describeSuppression(s)+"; "+strconv.Itoa(n)+" open detection(s) now suppressed")
	writeJSON(w, http.StatusOK, c.suppressionOut(s))
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
		list, _, err := models.ListDetections(tdb, models.DetectionFilter{Acknowledged: &f, Suppressed: &f}, 100000)
		if err != nil {
			continue
		}
		for _, d := range list {
			if !matchSuppression(s, a.ID, d.Type, d.OSUser, d.Exe, d.Target) {
				continue
			}
			d.Suppressed, d.SuppressionID = true, s.ID
			if d.NotifyState == models.NotifyPending {
				d.NotifyState = models.NotifySkipped
			}
			if models.UpdateDetectionOccurrence(tdb, d) == nil {
				n++
			}
		}
	}
	return n
}

func (c *core) handleDeleteSuppression(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "suppression not found")
		return
	}
	s, err := models.GetSuppression(c.db, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "suppression not found")
		return
	}
	if err := models.DeleteSuppression(c.db, id); err != nil {
		c.internalError(w, r, err)
		return
	}
	c.history(principalFrom(r), models.HistSuppression, "deleted", strconv.FormatInt(id, 10), describeSuppression(s))
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
	RemoteAddr  string  `json:"remote_addr"`
	Count       int     `json:"count"`
}

func historyOut(h *models.HistoryItem) historyJSON {
	return historyJSON{ID: h.ID, At: ts(h.LastAt), AccountName: h.AccountName, Category: h.Category, Action: h.Action,
		Target: h.Target, Detail: h.Detail, RemoteAddr: h.RemoteAddr, Count: h.Count}
}

func (c *core) handleHistory(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100, 1, 500)
	offset := queryInt(r, "offset", 0, 0, 10000000)
	items, total, err := models.ListHistory(c.db, r.URL.Query().Get("category"), "", limit, offset)
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	out := make([]historyJSON, 0, len(items))
	for _, h := range items {
		out = append(out, historyOut(h))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "total": total})
}

func (c *core) handleOverview(w http.ResponseWriter, r *http.Request) {
	running, msg := c.builtinStatus()
	agents, _ := models.ListAgents(c.db)
	unacked := 0
	for _, a := range agents {
		if tdb, err := c.targetDB(a); err == nil {
			n, _ := models.CountUnackedAlerts(tdb)
			unacked += n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": modules.AppVersion, "central_hostname": c.hostname,
		"builtin_agent": map[string]any{"running": running, "message": msg},
		"agents":        len(agents), "unacked_alerts": unacked,
	})
}

func (c *core) handleGuide(w http.ResponseWriter, r *http.Request) {
	_, port, _ := net.SplitHostPort(c.conf().AgentListen)
	var urls []string
	for _, n := range modules.ServerCertNames(c.conf().ServerNames) {
		if n == "127.0.0.1" || n == "::1" || n == "localhost" {
			continue
		}
		urls = append(urls, "https://"+net.JoinHostPort(n, port))
	}
	fp := ""
	for _, ca := range c.trustedCAs() {
		if ca.Status == models.CertActive {
			fp = modules.FormatFingerprint(ca.Fingerprint)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": modules.AppVersion, "central_hostname": c.hostname, "agent_port": port, "agent_urls": nonNil(urls),
		"ca_fingerprint": fp, "default_valid_days": c.conf().TokenValidDays,
	})
}
