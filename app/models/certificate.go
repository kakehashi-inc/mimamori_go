package models

// Certificate statuses.
const (
	CertActive  = "active"  // CA signing the current server certificate / current server certificate
	CertNext    = "next"    // new CA being distributed before the switch
	CertRetired = "retired" // previous CA still trusted until it expires / replaced server certificate
)

// Certificate is a row of the certificates table: a private CA or a server
// certificate with its private key.
type Certificate struct {
	ID          int64
	Kind        string
	Fingerprint string
	Subject     string
	IssuerFP    string
	Names       string
	CertPEM     string
	KeyPEM      string
	NotBefore   int64
	NotAfter    int64
	Status      string
	CreatedAt   int64
}

const certCols = `id, kind, fingerprint, subject, issuer_fp, names, cert_pem, key_pem, not_before, not_after, status, created_at`

func scanCert(s rowScanner) (*Certificate, error) {
	c := &Certificate{}
	if err := s.Scan(&c.ID, &c.Kind, &c.Fingerprint, &c.Subject, &c.IssuerFP, &c.Names, &c.CertPEM, &c.KeyPEM,
		&c.NotBefore, &c.NotAfter, &c.Status, &c.CreatedAt); err != nil {
		return nil, err
	}
	return c, nil
}

// InsertCertificate stores a certificate.
func InsertCertificate(db Writer, c *Certificate) error {
	c.CreatedAt = Now()
	res, err := db.Exec(`INSERT INTO certificates (kind, fingerprint, subject, issuer_fp, names, cert_pem, key_pem, not_before, not_after, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.Kind, c.Fingerprint, c.Subject, c.IssuerFP, c.Names, c.CertPEM, c.KeyPEM,
		c.NotBefore, c.NotAfter, c.Status, c.CreatedAt)
	if err != nil {
		return err
	}
	c.ID, _ = res.LastInsertId()
	return nil
}

// ListCertificates returns certificates of a kind, newest first.
func ListCertificates(db Reader, kind string) ([]*Certificate, error) {
	rows, err := db.Query(`SELECT `+certCols+` FROM certificates WHERE kind = ? ORDER BY not_before DESC, id DESC`, kind)
	return collect(rows, err, scanCert)
}

// SetCertificateStatus changes the status of a certificate.
func SetCertificateStatus(db Writer, id int64, status string) error {
	return execOne(db, `UPDATE certificates SET status = ? WHERE id = ?`, status, id)
}

// DeleteCertificate removes a certificate (expired CAs are discarded).
func DeleteCertificate(db Writer, id int64) error {
	return execOne(db, `DELETE FROM certificates WHERE id = ?`, id)
}
