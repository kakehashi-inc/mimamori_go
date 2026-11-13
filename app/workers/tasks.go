package workers

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// periodicTasks runs the central's periodic processing. It is dispatched as
// a job, so it counts against the worker limit like a request.
func (c *core) periodicTasks() {
	now := time.Now()
	c.reloadConfig()
	if err := c.manageCertificates(); err != nil {
		c.logger.Printf("certificates: %v", err)
	}
	c.processTokenExpiry(now)
	c.expireCommandRequests(now)
	c.retention(now)
	c.notify(now)
}

func (c *core) settingInt(key string) int64 {
	v, _ := strconv.ParseInt(models.GetSetting(c.pool, key), 10, 64)
	return v
}

// processTokenExpiry records tokens that expired since the last check and
// drops renewal values that were never used before their expiry.
func (c *core) processTokenExpiry(now time.Time) {
	last := c.settingInt(settingLastTokenCheck)
	tokens, err := models.ListTokens(c.pool)
	if err != nil {
		return
	}
	n := now.Unix()
	for _, t := range tokens {
		if t.RevokedAt == 0 && t.ExpiresAt > last && t.ExpiresAt <= n && (t.PendingHash == "" || t.PendingExpiresAt <= n) {
			c.history(nil, models.HistToken, "expired", t.Name, modules.M("history.detail.token_expired"))
		}
		if t.PendingHash != "" && t.PendingExpiresAt <= n {
			_ = models.DropExpiredTokenRenewal(c.pool, t.ID, n)
		}
	}
	_ = models.SetSetting(c.pool, settingLastTokenCheck, strconv.FormatInt(n, 10))
}

// retention deletes detections, command runs and ended run requests older
// than the retention period, and command runs beyond each set's keep count
// (once an hour).
func (c *core) retention(now time.Time) {
	if now.Unix()-c.settingInt(settingLastRetentionAt) < 3600 {
		return
	}
	cut := now.Add(-time.Duration(c.conf().RetentionDays) * 24 * time.Hour).Unix()
	agents, err := models.ListAgents(c.pool)
	if err != nil {
		return
	}
	for _, a := range agents {
		tdb, err := c.targetDB(a)
		if err != nil {
			continue
		}
		// One transaction for the target. A statement that fails does not
		// undo the others (the failure of the command runs is logged), as
		// when each statement stood on its own.
		var detections, runs int64
		var runsErr error
		err = tdb.Write(func(tx models.Writer) error {
			if n, err := models.DeleteDetectionsBefore(tx, cut); err == nil {
				detections = n
			}
			runs, runsErr = commandRetention(tx, cut)
			return nil
		})
		if err != nil {
			detections, runs, runsErr = 0, 0, err
		}
		if detections > 0 {
			c.logger.Printf("retention: removed %d detection(s) of %s", detections, c.agentDisplayName(a))
		}
		if runsErr != nil {
			c.logger.Printf("retention of command runs of %s: %v", c.agentDisplayName(a), runsErr)
		} else if runs > 0 {
			c.logger.Printf("retention: removed %d command run(s) of %s", runs, c.agentDisplayName(a))
		}
	}
	_ = models.DeleteRejectedHistoryBefore(c.pool, cut)
	_ = models.DeleteExpiredSessions(c.pool, now.Unix())
	_ = models.DeleteExpiredAuthCodes(c.pool, now.Unix())
	_ = models.DeleteExpiredBrowserVerifications(c.pool, now.Unix())
	_ = models.DeleteSentNotificationsBefore(c.pool, now.Add(-30*24*time.Hour).Unix())
	_ = models.SetSetting(c.pool, settingLastRetentionAt, strconv.FormatInt(now.Unix(), 10))
}

type pendingGroup struct {
	agent    *models.Agent
	name     string
	tdbIDs   []int64
	alerts   []*models.Detection
	normals  []*models.Detection
	hostname string
}

// notify (re)sends an undelivered mail, or composes a new one from new
// detections when none is left and the minimum interval has passed since the
// last delivered mail. While a mail is undelivered, new detections stay
// pending and go into the next mail, so at most one mail is sent per minimum
// interval even after a delivery outage. Nothing is composed or sent while
// SMTP cannot send or there is no recipient (detections stay pending).
func (c *core) notify(now time.Time) {
	if !c.conf().SMTP.CanSend() {
		return
	}
	if len(c.notificationRecipients()) == 0 {
		return
	}
	unsent, err := models.ListUnsentNotifications(c.pool)
	if err != nil {
		return
	}
	if len(unsent) == 0 {
		minInterval := int64(c.conf().Notify.MinIntervalMin) * 60
		if now.Unix()-c.settingInt(settingLastNotifyAt) < minInterval {
			return
		}
		c.composeNotification(now)
		if unsent, err = models.ListUnsentNotifications(c.pool); err != nil {
			return
		}
	}
	for _, n := range unsent {
		to := c.notificationRecipients()
		if len(to) == 0 {
			return
		}
		err := c.sendMail(c.conf().SMTP, to, n.Subject, n.Body)
		if err != nil {
			_ = models.MarkNotificationAttempt(c.pool, n.ID, 0, err.Error())
			c.logger.Printf("notification %d: send failed (will retry): %v", n.ID, err)
			return
		}
		sent := time.Now().Unix()
		_ = models.MarkNotificationAttempt(c.pool, n.ID, sent, "")
		_ = models.SetSetting(c.pool, settingLastNotifyAt, strconv.FormatInt(sent, 10))
	}
}

// notificationRecipients returns the current recipients of notification
// mails: the addresses of the enabled accounts chosen as recipients. On a
// database error it returns none, so nothing is sent until the next try.
func (c *core) notificationRecipients() []string {
	accounts, err := models.ListNotifyEmails(c.pool)
	if err != nil {
		c.logger.Printf("notification recipients: %v", err)
		return nil
	}
	return accounts
}

func (c *core) composeNotification(now time.Time) {
	agents, err := models.ListAgents(c.pool)
	if err != nil {
		return
	}
	var groups []*pendingGroup
	totalAlerts, total := 0, 0
	for _, a := range agents {
		tdb, err := c.targetDB(a)
		if err != nil {
			continue
		}
		list, err := models.ListPendingNotify(tdb.Read())
		if err != nil || len(list) == 0 {
			continue
		}
		g := &pendingGroup{agent: a, name: c.agentDisplayName(a), hostname: a.Hostname}
		if g.hostname == "" && a.Kind == models.AgentBuiltin {
			g.hostname = c.hostname
		}
		for _, d := range list {
			g.tdbIDs = append(g.tdbIDs, d.ID)
			if d.Category == modules.CategoryAlert {
				g.alerts = append(g.alerts, d)
			} else {
				g.normals = append(g.normals, d)
			}
		}
		totalAlerts += len(g.alerts)
		total += len(list)
		groups = append(groups, g)
	}
	if total == 0 {
		return
	}
	subject := fmt.Sprintf("%s %d new detection(s)", c.conf().Notify.SubjectPrefix, total)
	if totalAlerts > 0 {
		subject = fmt.Sprintf("%s [ALERT] %d alert(s), %d new detection(s)", c.conf().Notify.SubjectPrefix, totalAlerts, total)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s detected %d new item(s) at %s.\n", modules.AppName, total, now.Local().Format("2006-01-02 15:04:05 MST"))
	lines, omitted := 0, 0
	for _, g := range groups {
		fmt.Fprintf(&b, "\n== %s (%s) ==\n", g.name, g.hostname)
		for _, d := range append(append([]*models.Detection{}, g.alerts...), g.normals...) {
			if lines >= c.conf().Notify.MaxLines {
				omitted++
				continue
			}
			b.WriteString(summarizeDetection(d) + "\n")
			lines++
		}
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "\n... %d more detection(s) not listed. See the detections screen.\n", omitted)
	}
	if err := models.InsertNotification(c.pool, &models.Notification{Subject: subject, Body: b.String()}); err != nil {
		c.logger.Printf("notification: %v", err)
		return
	}
	for _, g := range groups {
		if tdb, err := c.targetDB(g.agent); err == nil {
			_ = tdb.Write(func(tx models.Writer) error {
				return models.MarkNotified(tx, g.tdbIDs, now.Unix())
			})
		}
	}
}

// summarizeDetection renders one detection as one line.
func summarizeDetection(d *models.Detection) string {
	parts := []string{"[" + strings.ToUpper(d.Category) + "]", d.Type}
	if d.OSUser != "" {
		parts = append(parts, "user="+d.OSUser)
	}
	if d.Exe != "" {
		parts = append(parts, "exe="+d.Exe)
	}
	if d.Target != "" {
		t := d.Target
		if r := []rune(t); len(r) > 160 {
			t = string(r[:160]) + "..."
		}
		parts = append(parts, "target="+t)
	}
	if d.FileMode != "" {
		parts = append(parts, "mode="+d.FileMode, "owner="+d.FileOwner)
	}
	parts = append(parts, fmt.Sprintf("count=%d", d.Count), "last="+time.Unix(d.LastAt, 0).Local().Format("2006-01-02 15:04:05"))
	return strings.Join(parts, " ")
}
