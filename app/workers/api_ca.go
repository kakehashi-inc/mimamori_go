package workers

import (
	"net/http"
	"strings"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// caFileSuffix ends the path segment of a CA certificate download: the
// fingerprint (lower-case hex) followed by this suffix.
const caFileSuffix = ".crt"

// webCAJSON is one CA certificate a browser can install, with the path
// segment that downloads it.
type webCAJSON struct {
	certInfoJSON
	File string `json:"file"`
}

// installableCAs returns the CAs a browser should trust: the active CA and
// the next one being distributed.
func (c *core) installableCAs() []*models.Certificate {
	var out []*models.Certificate
	for _, ca := range c.trustedCAs() {
		if ca.Status == models.CertActive || ca.Status == models.CertNext {
			out = append(out, ca)
		}
	}
	return out
}

// handleWebCAList lists the CA certificates for the CA distribution screen.
// It needs no login: CA certificates are public.
func (c *core) handleWebCAList(w http.ResponseWriter, r *http.Request) {
	list := []webCAJSON{}
	for _, ca := range c.installableCAs() {
		list = append(list, webCAJSON{certInfoJSON: certOut(ca), File: ca.Fingerprint + caFileSuffix})
	}
	writeJSON(w, http.StatusOK, map[string]any{"cas": list})
}

// handleWebCAFile returns one CA certificate (PEM) as
// application/x-x509-ca-cert, the type browsers and mobile systems offer
// to install. It needs no login.
func (c *core) handleWebCAFile(w http.ResponseWriter, r *http.Request) {
	fp, ok := strings.CutSuffix(r.PathValue("file"), caFileSuffix)
	if ok {
		for _, ca := range c.installableCAs() {
			if ca.Fingerprint != fp {
				continue
			}
			h := w.Header()
			h.Set("Content-Type", "application/x-x509-ca-cert")
			h.Set("Content-Disposition", `inline; filename="`+modules.AppName+"-ca-"+fp[:16]+caFileSuffix+`"`)
			h.Set("Cache-Control", "no-cache")
			_, _ = w.Write([]byte(ca.CertPEM))
			return
		}
	}
	writeError(w, http.StatusNotFound, modules.M("common.ca_not_found"))
}
