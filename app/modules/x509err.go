package modules

import "crypto/x509"

// x509UnknownAuthority aliases the x509 error for errors.As in IsCertificateError.
type x509UnknownAuthority = x509.UnknownAuthorityError
