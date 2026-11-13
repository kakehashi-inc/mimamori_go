package workers

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

func (c *core) settingInt(key string) int64 {
	v, _ := strconv.ParseInt(models.GetSetting(c.db.Read(), key), 10, 64)
	return v
}

// processTokenExpiry records tokens that expired since the last check and
// drops renewal values that were never used before their expiry.
//
// The time of the last check, the tokens, the history rows of the newly
// expired tokens, the dropped values and the new time of the check are read
// and written in one transaction: either all of it is stored or none, and
// then the next run does it. So a check never records an expiry twice or
// not at all.
func (c *core) processTokenExpiry(now time.Time) {
	n := now.Unix()
	err := c.db.Write(func(tx models.Writer) error {
		last, _ := strconv.ParseInt(models.GetSetting(tx, settingLastTokenCheck), 10, 64)
		tokens, err := models.ListTokens(tx)
		if err != nil {
			return err
		}
		for _, t := range tokens {
			if t.RevokedAt == 0 && t.ExpiresAt > last && t.ExpiresAt <= n && (t.PendingHash == "" || t.PendingExpiresAt <= n) {
				if err := c.recordHistory(tx, models.HistToken, "expired", t.Name, modules.M("history.detail.token_expired")); err != nil {
					return fmt.Errorf("history of token %s: %w", t.Name, err)
				}
			}
			if t.PendingHash != "" && t.PendingExpiresAt <= n {
				if err := models.DropExpiredTokenRenewal(tx, t.ID, n); err != nil {
					return fmt.Errorf("renewal value of token %s: %w", t.Name, err)
				}
			}
		}
		return models.SetSetting(tx, settingLastTokenCheck, strconv.FormatInt(n, 10))
	})
	if err != nil {
		c.logger.Printf("token expiry (repeated at the next run): %v", err)
	}
}

// retention deletes detections, command runs and ended run requests older
// than the retention period, and command runs beyond each set's keep count
// (once an hour).
//
// Every part that fails is logged with the agent and what failed, and the
// other parts go on. The mark of the last run is written all the same: it
// means that a run happened and the next one is due in an hour, which
// repeats the failed part; holding it back would repeat every deletion of
// every agent at every tick while one database fails.
func (c *core) retention(now time.Time) {
	if now.Unix()-c.settingInt(settingLastRetentionAt) < 3600 {
		return
	}
	cut := now.Add(-time.Duration(c.conf().RetentionDays) * 24 * time.Hour).Unix()
	agents, err := models.ListAgents(c.db.Read())
	if err != nil {
		c.logger.Printf("retention: list of agents: %v", err)
	}
	for _, a := range agents {
		name := c.agentDisplayName(a)
		tdb, err := c.targetDB(a)
		if err != nil {
			c.logger.Printf("retention of %s: database: %v", name, err)
			continue
		}
		// One transaction for the target. A statement that fails does not
		// undo the others, as when each statement stood on its own.
		var detections, runs int64
		var detectionsErr, runsErr error
		err = tdb.Write(func(tx models.Writer) error {
			detections, detectionsErr = models.DeleteDetectionsBefore(tx, cut)
			runs, runsErr = commandRetention(tx, cut)
			return nil
		})
		if err != nil {
			c.logger.Printf("retention of %s: %v", name, err)
			continue
		}
		if detectionsErr != nil {
			c.logger.Printf("retention of detections of %s: %v", name, detectionsErr)
		} else if detections > 0 {
			c.logger.Printf("retention: removed %d detection(s) of %s", detections, name)
		}
		if runs > 0 {
			c.logger.Printf("retention: removed %d command run(s) of %s", runs, name)
		}
		if runsErr != nil {
			c.logger.Printf("retention of command runs of %s: %v", name, runsErr)
		}
	}
	// One transaction for the central database; as for the targets, a
	// statement that fails does not undo the others.
	parts := []struct {
		what string
		run  func(tx models.Writer) error
	}{
		{"rejected requests", func(tx models.Writer) error { return models.DeleteRejectedHistoryBefore(tx, cut) }},
		{"expired sessions", func(tx models.Writer) error { return models.DeleteExpiredSessions(tx, now.Unix()) }},
		{"expired codes", func(tx models.Writer) error { return models.DeleteExpiredAuthCodes(tx, now.Unix()) }},
		{"expired browser verifications", func(tx models.Writer) error { return models.DeleteExpiredBrowserVerifications(tx, now.Unix()) }},
		{"ended run requests", func(tx models.Writer) error {
			_, err := models.DeleteEndedCommandRequestsBefore(tx, cut)
			return err
		}},
		{"sent notifications", func(tx models.Writer) error {
			return models.DeleteSentNotificationsBefore(tx, now.Add(-30*24*time.Hour).Unix())
		}},
		{"mark of the last run", func(tx models.Writer) error {
			return models.SetSetting(tx, settingLastRetentionAt, strconv.FormatInt(now.Unix(), 10))
		}},
	}
	var failed []string
	err = c.db.Write(func(tx models.Writer) error {
		failed = failed[:0]
		for _, p := range parts {
			if err := p.run(tx); err != nil {
				failed = append(failed, fmt.Sprintf("retention: %s: %v", p.what, err))
			}
		}
		return nil
	})
	for _, line := range failed {
		c.logger.Print(line)
	}
	if err != nil {
		c.logger.Printf("retention of the central database: %v", err)
	}
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
	unsent, err := models.ListUnsentNotifications(c.db.Read())
	if err != nil {
		return
	}
	if len(unsent) == 0 {
		minInterval := int64(c.conf().Notify.MinIntervalMin) * 60
		if now.Unix()-c.settingInt(settingLastNotifyAt) < minInterval {
			return
		}
		c.composeNotification(now)
		if unsent, err = models.ListUnsentNotifications(c.db.Read()); err != nil {
			return
		}
	}
	for _, n := range unsent {
		// A mail that was sent but could not be marked as sent is not sent
		// again; only the marking is retried.
		if sent, ok := c.sentUnmarked(n.ID); ok {
			if err := c.markNotificationSent(n.ID, sent); err != nil {
				c.logger.Printf("notification %d: sent, but still not marked as sent (will retry the marking): %v", n.ID, err)
				return
			}
			continue
		}
		to := c.notificationRecipients()
		if len(to) == 0 {
			return
		}
		err := c.sendMail(c.conf().SMTP, to, n.Subject, n.Body)
		if err != nil {
			if merr := c.db.Write(func(tx models.Writer) error { return models.MarkNotificationAttempt(tx, n.ID, 0, err.Error()) }); merr != nil {
				c.logger.Printf("notification %d: recording the failed attempt: %v", n.ID, merr)
			}
			c.logger.Printf("notification %d: send failed (will retry): %v", n.ID, err)
			return
		}
		sent := time.Now().Unix()
		if err := c.markNotificationSent(n.ID, sent); err != nil {
			c.rememberSentUnmarked(n.ID, sent)
			c.logger.Printf("notification %d: sent, but not marked as sent (will retry the marking, not the sending): %v", n.ID, err)
			return
		}
	}
}

// markNotificationSent records the delivered mail and the time of the last
// delivered mail in one transaction (both or neither), and forgets the mail
// in sentButUnmarked when it is stored.
func (c *core) markNotificationSent(id, sent int64) error {
	err := c.db.Write(func(tx models.Writer) error {
		if err := models.MarkNotificationAttempt(tx, id, sent, ""); err != nil {
			return err
		}
		return models.SetSetting(tx, settingLastNotifyAt, strconv.FormatInt(sent, 10))
	})
	if err == nil {
		c.notifyMu.Lock()
		delete(c.sentButUnmarked, id)
		c.notifyMu.Unlock()
	}
	return err
}

// sentUnmarked returns the time a mail was sent at when it was sent but not
// marked as sent.
func (c *core) sentUnmarked(id int64) (int64, bool) {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	sent, ok := c.sentButUnmarked[id]
	return sent, ok
}

func (c *core) rememberSentUnmarked(id, sent int64) {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	if c.sentButUnmarked == nil {
		c.sentButUnmarked = map[int64]int64{}
	}
	c.sentButUnmarked[id] = sent
}

// notificationRecipients returns the current recipients of notification
// mails: the addresses of the enabled accounts chosen as recipients. On a
// database error it returns none, so nothing is sent until the next try.
func (c *core) notificationRecipients() []string {
	accounts, err := models.ListNotifyEmails(c.db.Read())
	if err != nil {
		c.logger.Printf("notification recipients: %v", err)
		return nil
	}
	return accounts
}

func (c *core) composeNotification(now time.Time) {
	agents, err := models.ListAgents(c.db.Read())
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
	// The mail is stored in central.db; the detections are marked in their
	// agents' databases after it (another file each). A failed marking is
	// logged: the detection is then listed again in the next mail, never in
	// none.
	err = c.db.Write(func(tx models.Writer) error {
		return models.InsertNotification(tx, &models.Notification{Subject: subject, Body: b.String()})
	})
	if err != nil {
		c.logger.Printf("notification: %v", err)
		return
	}
	for _, g := range groups {
		tdb, err := c.targetDB(g.agent)
		if err == nil {
			err = tdb.Write(func(tx models.Writer) error {
				return models.MarkNotified(tx, g.tdbIDs, now.Unix())
			})
		}
		if err != nil {
			c.logger.Printf("notification: marking %d detection(s) of %s as notified: %v", len(g.tdbIDs), g.name, err)
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
