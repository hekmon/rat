package mtls

import (
	"context"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/hekmon/rat/tmux/names"
)

// CheckPeer checks cert, the certificate of a peer checked during the handshake of its
// connection, again for a request on that connection: its name a plain name, it and the CA of the
// side valid now, then not revoked (see CheckRevoked). The error wraps names.ErrInvalid,
// ErrValidity, ErrRevoked or ErrCRL.
//
// The name first, which Load checks of the certificate of a side, not of its peers: ratd takes
// the session from it and shows it in its status line, where a new line would start other fields
// for systemd, and the errors after quote it. A template of another PKI copying a display name
// ("Doe, John") does not make a plain one. The validity again: a connection kept alive outlives
// its handshake by hours, for a client calling more often than its idle timeout, a certificate
// expired since going on working on it, while a PKI issuing short-lived certificates relies on
// their expiry alone.
func (s *Side) CheckPeer(ctx context.Context, cert *x509.Certificate) error {
	if err := names.Check(cert.Subject.CommonName); err != nil {
		return fmt.Errorf("the certificate of the peer: %w", err)
	}
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
