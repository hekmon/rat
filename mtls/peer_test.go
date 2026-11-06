package mtls

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestCheckPeer guards what a side checks of the certificate of a peer on each request, which the
// handshake of its connection checked long before: it and the CA valid now (ErrValidity), as a
// certificate expiring while its connection is alive must stop working, then not revoked.
func TestCheckPeer(t *testing.T) {
	ca := crlCA(t, "t")
	ratd := crlSide(t, ca, Server, "t")
	now := time.Now()
	peer := func(notBefore, notAfter time.Time) *x509.Certificate {
		c, err := create(&x509.Certificate{
			Subject: pkix.Name{CommonName: "alice"}, NotBefore: notBefore, NotAfter: notAfter,
			BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}, &ca)
		if err != nil {
			t.Fatal(err)
		}
		return c.cert
	}
	expiredCA := *ratd
	expiredCA.CA = &x509.Certificate{NotBefore: now.Add(-2 * time.Hour), NotAfter: now.Add(-time.Hour)}
	for _, tc := range []struct {
		name    string
		side    *Side
		peer    *x509.Certificate
		message string
	}{
		{"valid", ratd, peer(now.Add(-time.Hour), now.Add(time.Hour)), ""},
		{"expired", ratd, peer(now.Add(-2*time.Hour), now.Add(-time.Hour)), "the certificate of alice expired on"},
		{"not valid yet", ratd, peer(now.Add(time.Hour), now.Add(2*time.Hour)), "the certificate of alice is not valid before"},
		{"CA expired", &expiredCA, peer(now.Add(-time.Hour), now.Add(time.Hour)), "the CA expired on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.side.CheckPeer(context.Background(), tc.peer)
			if tc.message == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				return
			}
			if !errors.Is(err, ErrValidity) || !strings.Contains(err.Error(), tc.message) {
				t.Errorf("expected ErrValidity telling %q, got %v", tc.message, err)
			}
		})
	}
}
