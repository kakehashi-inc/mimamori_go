package workers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// caList reads the CA list of the browser listener without a session.
func caList(t *testing.T, e *testEnv) []webCAJSON {
	t.Helper()
	rec := e.do(e.web, "GET", "/api/v1/ca", nil, nil, "192.0.2.1:1")
	if rec.Code != http.StatusOK {
		t.Fatalf("CA list: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		CAs []webCAJSON `json:"cas"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.CAs
}

// The CA list and each CA certificate are served without a login; only the
// active and the next CA are offered, each as one certificate.
func TestWebCADownload(t *testing.T) {
	e := newTestEnv(t)
	list := caList(t, e)
	if len(list) != 1 || list[0].Status != models.CertActive {
		t.Fatalf("CA list: %+v", list)
	}
	rec := e.do(e.web, "GET", "/api/v1/ca/"+list[0].File, nil, nil, "192.0.2.1:1")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-x509-ca-cert" ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), `filename="mimamori-ca-`) {
		t.Fatalf("CA file: %d %v", rec.Code, rec.Header())
	}
	cert, err := modules.ParseCertPEM(rec.Body.String())
	if err != nil || !cert.IsCA || modules.FormatFingerprint(modules.Fingerprint(cert)) != list[0].Fingerprint {
		t.Fatalf("CA file content: %v %q", err, rec.Body.String())
	}
	if n := strings.Count(rec.Body.String(), "BEGIN CERTIFICATE"); n != 1 {
		t.Fatalf("CA file holds %d certificates", n)
	}
	for _, file := range []string{strings.TrimSuffix(list[0].File, caFileSuffix), strings.Repeat("0", 64) + caFileSuffix, "x"} {
		if rec := e.do(e.web, "GET", "/api/v1/ca/"+file, nil, nil, "192.0.2.1:1"); rec.Code != http.StatusNotFound {
			t.Fatalf("CA file %q: %d", file, rec.Code)
		}
	}

	// During the distribution the next CA is offered too; after the switch
	// the retired CA is not.
	now := time.Now()
	if err := e.exec(`UPDATE certificates SET not_after = ? WHERE kind = 'ca' AND status = ?`,
		now.Add(modules.CARotateBefore-time.Hour).Unix(), models.CertActive); err != nil {
		t.Fatal(err)
	}
	if err := e.c.manageCertificates(); err != nil {
		t.Fatal(err)
	}
	list = caList(t, e)
	if len(list) != 2 {
		t.Fatalf("CA list while distributing: %+v", list)
	}
	var next webCAJSON
	for _, ca := range list {
		if ca.Status == models.CertNext {
			next = ca
		}
	}
	if next.File == "" {
		t.Fatalf("no next CA in %+v", list)
	}
	if err := e.exec(`UPDATE certificates SET created_at = ? WHERE kind = 'ca' AND status = ?`,
		now.Add(-modules.CASwitchAfter-time.Hour).Unix(), models.CertNext); err != nil {
		t.Fatal(err)
	}
	if err := e.c.manageCertificates(); err != nil {
		t.Fatal(err)
	}
	if list = caList(t, e); len(list) != 1 || list[0].Fingerprint != next.Fingerprint || list[0].Status != models.CertActive {
		t.Fatalf("CA list after the switch: %+v", list)
	}
}

// The session cookie is only sent over HTTPS.
func TestSessionCookieSecure(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(e.web, "POST", "/api/v1/auth/login", map[string]string{"email": testAdminEmail, "password": "Admin-pass-123"}, map[string]string{csrfHeader: "1"}, "x:1")
	found := false
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == sessionCookie {
			found = true
			if !ck.Secure || !ck.HttpOnly {
				t.Fatalf("session cookie: %+v", ck)
			}
		}
	}
	if !found {
		t.Fatalf("login: %d, no session cookie", rec.Code)
	}
}
