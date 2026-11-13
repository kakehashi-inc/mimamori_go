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

// certRow builds the certificates row of a certificate pair.
func certRow(kind, status, issuerFP string, p *modules.CertKeyPair) *models.Certificate {
	names, _ := json.Marshal(modules.CertNames(p.Cert))
	return &models.Certificate{
		Kind: kind, Fingerprint: modules.Fingerprint(p.Cert), Subject: p.Cert.Subject.CommonName, IssuerFP: issuerFP,
		Names: string(names), CertPEM: p.CertPEM, KeyPEM: p.KeyPEM,
		NotBefore: p.Cert.NotBefore.Unix(), NotAfter: p.Cert.NotAfter.Unix(), Status: status,
	}
}

func pickStatus(list []*models.Certificate, status string) *models.Certificate {
	for _, x := range list {
		if x.Status == status {
			return x
		}
	}
	return nil
}

// certID is the ID of a certificate row, 0 for none.
func certID(x *models.Certificate) int64 {
	if x == nil {
		return 0
	}
	return x.ID
}

// recordHistory inserts the history row of an operation inside a write
// transaction (see historyRow).
func (c *core) recordHistory(tx models.Writer, category, action, target string, detail modules.Msg) error {
	return models.InsertHistory(tx, c.historyRow(nil, category, action, modules.Raw(target), detail))
}

// manageCertificates creates, renews and rolls over the private CA and the
// server certificate, then loads the server certificate for TLS. It runs at
// start and as a periodic task.
//
// A key is generated between reading the certificates and writing them, so
// the reads are on the read side and every group of writes (with its
// history rows) is one transaction that reads the rows again and checks
// that they are still as they were read; when they are not (another run
// changed them), the step is left to that run.
func (c *core) manageCertificates() error {
	now := time.Now()
	// Discard expired CAs that are not active.
	err := c.db.Write(func(tx models.Writer) error {
		cas, err := models.ListCertificates(tx, "ca")
		if err != nil {
			return err
		}
		for _, ca := range cas {
			if ca.Status != models.CertActive && ca.NotAfter < now.Unix() {
				if err := models.DeleteCertificate(tx, ca.ID); err != nil {
					return err
				}
				if err := c.recordHistory(tx, models.HistCertificate, "ca_discarded", modules.FormatFingerprint(ca.Fingerprint), modules.M("history.detail.ca_discarded")); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	cas, err := models.ListCertificates(c.db.Read(), "ca")
	if err != nil {
		return err
	}
	active := pickStatus(cas, models.CertActive)
	if active == nil || active.NotAfter < now.Unix() {
		pair, err := modules.GenerateCA(now)
		if err != nil {
			return err
		}
		row := certRow("ca", models.CertActive, "", pair)
		err = c.db.Write(func(tx models.Writer) error {
			list, err := models.ListCertificates(tx, "ca")
			if err != nil {
				return err
			}
			if certID(pickStatus(list, models.CertActive)) != certID(active) {
				return nil
			}
			if active != nil {
				if err := models.SetCertificateStatus(tx, active.ID, models.CertRetired); err != nil {
					return err
				}
			}
			if err := models.InsertCertificate(tx, row); err != nil {
				return err
			}
			return c.recordHistory(tx, models.HistCertificate, "ca_created", modules.FormatFingerprint(row.Fingerprint), modules.M("history.detail.ca_created"))
		})
		if err != nil {
			return err
		}
		if cas, err = models.ListCertificates(c.db.Read(), "ca"); err != nil {
			return err
		}
		if active = pickStatus(cas, models.CertActive); active == nil {
			return fmt.Errorf("no active CA")
		}
	}
	next := pickStatus(cas, models.CertNext)
	// Roll-over step 1: create the next CA and distribute it.
	if next == nil && time.Unix(active.NotAfter, 0).Sub(now) < modules.CARotateBefore {
		pair, err := modules.GenerateCA(now)
		if err != nil {
			return err
		}
		row := certRow("ca", models.CertNext, "", pair)
		err = c.db.Write(func(tx models.Writer) error {
			list, err := models.ListCertificates(tx, "ca")
			if err != nil {
				return err
			}
			if next = pickStatus(list, models.CertNext); next != nil {
				return nil
			}
			if err := models.InsertCertificate(tx, row); err != nil {
				return err
			}
			next = row
			return c.recordHistory(tx, models.HistCertificate, "ca_next_created", modules.FormatFingerprint(row.Fingerprint),
				modules.M("history.detail.ca_next_created", "at", now.Add(modules.CASwitchAfter).UTC().Format(time.RFC3339)))
		})
		if err != nil {
			return err
		}
	}
	// Roll-over step 2: after the distribution period, switch to the new CA.
	forceServer := false
	if next != nil && now.Unix() >= next.CreatedAt+int64(modules.CASwitchAfter/time.Second) {
		switched := false
		err := c.db.Write(func(tx models.Writer) error {
			list, err := models.ListCertificates(tx, "ca")
			if err != nil {
				return err
			}
			if certID(pickStatus(list, models.CertActive)) != active.ID || certID(pickStatus(list, models.CertNext)) != next.ID {
				return nil
			}
			if err := models.SetCertificateStatus(tx, active.ID, models.CertRetired); err != nil {
				return err
			}
			if err := models.SetCertificateStatus(tx, next.ID, models.CertActive); err != nil {
				return err
			}
			switched = true
			return c.recordHistory(tx, models.HistCertificate, "ca_switched", modules.FormatFingerprint(next.Fingerprint), modules.M("history.detail.ca_switched"))
		})
		if err != nil {
			return err
		}
		if switched {
			active, forceServer = next, true
		} else if cas, err = models.ListCertificates(c.db.Read(), "ca"); err != nil {
			return err
		} else if active = pickStatus(cas, models.CertActive); active == nil {
			return fmt.Errorf("no active CA")
		}
	}
	activePair, err := modules.LoadPair(active.CertPEM, active.KeyPEM)
	if err != nil {
		return err
	}

	servers, err := models.ListCertificates(c.db.Read(), "server")
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
		row := certRow("server", models.CertActive, active.Fingerprint, pair)
		err = c.db.Write(func(tx models.Writer) error {
			list, err := models.ListCertificates(tx, "server")
			if err != nil {
				return err
			}
			if found := pickStatus(list, models.CertActive); certID(found) != certID(cur) {
				cur = found
				return nil
			}
			if err := models.InsertCertificate(tx, row); err != nil {
				return err
			}
			for _, s := range list {
				if err := models.DeleteCertificate(tx, s.ID); err != nil {
					return err
				}
			}
			cur = row
			return c.recordHistory(tx, models.HistCertificate, "server_cert_issued", modules.FormatFingerprint(row.Fingerprint),
				modules.M("history.detail.server_cert_names", "names", modules.CertNames(pair.Cert)))
		})
		if err != nil {
			return err
		}
		if cur == nil {
			return fmt.Errorf("no server certificate")
		}
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
	cas, _ := models.ListCertificates(c.db.Read(), "ca")
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
