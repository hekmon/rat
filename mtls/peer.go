package mtls

import (
	"context"
	"crypto/x509"
	"fmt"
	"time"
)

// CheckPeer checks cert, the certificate of a peer checked during the handshake of its
// connection, again for a request on that connection: it and the CA of the side valid now, then
// not revoked (see CheckRevoked). A connection kept alive outlives its handshake by hours, for a
// client calling more often than its idle timeout: a certificate expired since would go on working
// on it, while a PKI issuing short-lived certificates relies on their expiry alone. The error wraps
// ErrValidity, ErrRevoked or ErrCRL.
func (s *Side) CheckPeer(ctx context.Context, cert *x509.Certificate) error {
	now := time.Now()
	for _, c := range []struct {
		what string
		cert *x509.Certificate
	}{{"the certificate of " + cert.Subject.CommonName, cert}, {"the CA", s.CA}} {
		if now.Before(c.cert.NotBefore) {
			return fmt.Errorf("%w: %s is not valid before %s", ErrValidity, c.what, c.cert.NotBefore.Format(time.RFC3339))
		}
		if now.After(c.cert.NotAfter) {
			return fmt.Errorf("%w: %s expired on %s", ErrValidity, c.what, c.cert.NotAfter.Format(time.RFC3339))
		}
	}
	return s.CheckRevoked(ctx, cert)
}
