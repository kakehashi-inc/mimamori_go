package workers

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// storePair stores a certificate pair in the certificates table.
func (c *core) storePair(kind, status, issuerFP string, p *modules.CertKeyPair) (*models.Certificate, error) {
	names, _ := json.Marshal(modules.CertNames(p.Cert))
	row := &models.Certificate{
		Kind: kind, Fingerprint: modules.Fingerprint(p.Cert), Subject: p.Cert.Subject.CommonName, IssuerFP: issuerFP,
		Names: string(names), CertPEM: p.CertPEM, KeyPEM: p.KeyPEM,
		NotBefore: p.Cert.NotBefore.Unix(), NotAfter: p.Cert.NotAfter.Unix(), Status: status,
	}
	return row, models.InsertCertificate(c.db, row)
}

func pickStatus(list []*models.Certificate, status string) *models.Certificate {
	for _, x := range list {
		if x.Status == status {
			return x
		}
	}
	return nil
}

// manageCertificates creates, renews and rolls over the private CA and the
// server certificate, then loads the server certificate for TLS. It runs at
// start and as a periodic task.
func (c *core) manageCertificates() error {
	now := time.Now()
	cas, err := models.ListCertificates(c.db, "ca")
	if err != nil {
		return err
	}
	// Discard expired CAs that are not active.
	for _, ca := range cas {
		if ca.Status != models.CertActive && ca.NotAfter < now.Unix() {
			_ = models.DeleteCertificate(c.db, ca.ID)
			c.history(nil, models.HistCertificate, "ca_discarded", modules.FormatFingerprint(ca.Fingerprint), "expired CA removed")
		}
	}
	cas, _ = models.ListCertificates(c.db, "ca")
	active := pickStatus(cas, models.CertActive)
	if active == nil || active.NotAfter < now.Unix() {
		pair, err := modules.GenerateCA(now)
		if err != nil {
			return err
		}
		if active != nil {
			_ = models.SetCertificateStatus(c.db, active.ID, models.CertRetired)
		}
		if active, err = c.storePair("ca", models.CertActive, "", pair); err != nil {
			return err
		}
		c.history(nil, models.HistCertificate, "ca_created", modules.FormatFingerprint(active.Fingerprint), "private CA created")
		cas, _ = models.ListCertificates(c.db, "ca")
	}
	next := pickStatus(cas, models.CertNext)
	// Roll-over step 1: create the next CA and distribute it.
	if next == nil && time.Unix(active.NotAfter, 0).Sub(now) < modules.CARotateBefore {
		pair, err := modules.GenerateCA(now)
		if err != nil {
			return err
		}
		if next, err = c.storePair("ca", models.CertNext, "", pair); err != nil {
			return err
		}
		c.history(nil, models.HistCertificate, "ca_next_created", modules.FormatFingerprint(next.Fingerprint),
			fmt.Sprintf("new CA created; distributing to agents; server certificate switches after %s", now.Add(modules.CASwitchAfter).UTC().Format(time.RFC3339)))
	}
	// Roll-over step 2: after the distribution period, switch to the new CA.
	forceServer := false
	if next != nil && now.Unix() >= next.CreatedAt+int64(modules.CASwitchAfter/time.Second) {
		if err := models.SetCertificateStatus(c.db, active.ID, models.CertRetired); err != nil {
			return err
		}
		if err := models.SetCertificateStatus(c.db, next.ID, models.CertActive); err != nil {
			return err
		}
		c.history(nil, models.HistCertificate, "ca_switched", modules.FormatFingerprint(next.Fingerprint), "server certificate now signed by the new CA")
		active, forceServer = next, true
	}
	activePair, err := modules.LoadPair(active.CertPEM, active.KeyPEM)
	if err != nil {
		return err
	}

	servers, err := models.ListCertificates(c.db, "server")
	if err != nil {
		return err
	}
	cur := pickStatus(servers, models.CertActive)
	wantNames := modules.ServerCertNames(c.conf().ServerNames)
	needNew := forceServer || cur == nil || cur.IssuerFP != active.Fingerprint ||
		time.Unix(cur.NotAfter, 0).Sub(now) < modules.ServerCertRenewBefore
	if cur != nil && !needNew {
		// Both sides are normalized, so names written differently (an IP
		// address in another textual form, a host name in upper case) do not
		// cause a new certificate at every check.
		var names []string
		_ = json.Unmarshal([]byte(cur.Names), &names)
		if strings.Join(modules.ServerCertNames(names), ",") != strings.Join(wantNames, ",") {
			needNew = true
		}
	}
	if needNew {
		pair, err := modules.GenerateServerCert(activePair, c.conf().ServerNames, now)
		if err != nil {
			return err
		}
		row, err := c.storePair("server", models.CertActive, active.Fingerprint, pair)
		if err != nil {
			return err
		}
		for _, s := range servers {
			_ = models.DeleteCertificate(c.db, s.ID)
		}
		c.history(nil, models.HistCertificate, "server_cert_issued", modules.FormatFingerprint(row.Fingerprint),
			"names: "+strings.Join(modules.CertNames(pair.Cert), ", "))
		cur = row
	}
	pair, err := modules.LoadPair(cur.CertPEM, cur.KeyPEM)
	if err != nil {
		return err
	}
	tc, err := pair.TLSCertificate()
	if err != nil {
		return err
	}
	c.serverCert.Store(tc)
	return c.writeCABundle()
}

// trustedCAs returns the CA certificates agents should trust (not expired).
func (c *core) trustedCAs() []*models.Certificate {
	cas, _ := models.ListCertificates(c.db, "ca")
	now := time.Now().Unix()
	var out []*models.Certificate
	for _, ca := range cas {
		if ca.NotAfter > now {
			out = append(out, ca)
		}
	}
	return out
}

// currentCAPEMs returns the active and next CA certificates.
func (c *core) currentCAPEMs() []string {
	var out []string
	for _, ca := range c.trustedCAs() {
		if ca.Status == models.CertActive || ca.Status == models.CertNext {
			out = append(out, ca.CertPEM)
		}
	}
	return out
}

// writeCABundle writes the public CA certificates for the CLI.
func (c *core) writeCABundle() error {
	var b strings.Builder
	for _, p := range c.currentCAPEMs() {
		b.WriteString(p)
	}
	path := filepath.Join(c.dataDir, modules.CABundleFileName)
	return modules.WriteFileAtomic(path, []byte(b.String()), 0o644)
}

// tlsConfig serves the current server certificate (switched without restart).
func (c *core) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return c.serverCert.Load(), nil
		},
	}
}
