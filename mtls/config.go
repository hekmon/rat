package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

// ServerConfig returns the TLS configuration of ratd, from the server side of its bundle: it
// presents the server certificate, and requires a client certificate of the bundle (signed by
// its CA, valid, clientAuth), which Go checks by itself. TLS 1.3 only. It sets nothing of the
// protocol above TLS (ALPN), which is ratd's to choose.
// The error wraps ErrRole for the side of a client: a client can not run a server.
func ServerConfig(side *Side) (*tls.Config, error) {
	if side.Role != Server {
		return nil, fmt.Errorf("%w: the directory of a %s can not serve", ErrRole, side.Role)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{side.Certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool(side.CA),
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientConfig returns the TLS configuration of a client of ratd, from the side of the client in
// the bundle: it presents the client certificate, and accepts a server certificate of the bundle
// (signed by its CA, valid, serverAuth) without checking the host name it reaches, which the
// server certificate does not carry. TLS 1.3 only. A server refused fails the handshake with an
// error wrapping ErrNotSignedByCA, ErrValidity or ErrRole.
// The error wraps ErrRole for the side of the server: the server can not stand for a client.
func ClientConfig(side *Side) (*tls.Config, error) {
	if side.Role != Client {
		return nil, fmt.Errorf("%w: the directory of the %s can not be a client", ErrRole, side.Role)
	}
	roots := pool(side.CA)
	return &tls.Config{
		Certificates: []tls.Certificate{side.Certificate},
		MinVersion:   tls.VersionTLS13,
		// Go has no option to skip the host name check alone: InsecureSkipVerify turns off the
		// whole verification of the server certificate, which VerifyConnection does again, host
		// name aside. The server still proves it holds the key of its certificate: Go checks the
		// signature of the handshake whatever InsecureSkipVerify says.
		InsecureSkipVerify: true,
		// VerifyConnection rather than VerifyPeerCertificate: it also runs on resumed sessions,
		// and VerifyPeerCertificate gets no verified chains once InsecureSkipVerify is set.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("%w: the server presented no certificate", ErrNotSignedByCA)
			}
			// The server sends its certificate alone: no intermediate, the CA signs no CA. The
			// role is checked as Go does, the closed bundle holding no certificate with both roles.
			_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:     roots,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			return verificationError(err)
		},
	}, nil
}

// PeerName returns the name carried by the certificate of the peer of a connection established
// with these configurations: for ratd, the client, which is its session; for a client, the
// tenant. It is empty when the peer presented no certificate, which a handshake with these
// configurations does not allow.
func PeerName(state tls.ConnectionState) string {
	if len(state.PeerCertificates) == 0 {
		return ""
	}
	return state.PeerCertificates[0].Subject.CommonName
}

// pool returns a pool trusting ca alone.
func pool(ca *x509.Certificate) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca)
	return p
}

// verificationError wraps the error of the verification of a server certificate with the
// sentinel of the check of Load it matches, for the client to tell why the server was refused.
func verificationError(err error) error {
	if err == nil {
		return nil
	}
	var unknownAuthority x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &unknownAuthority):
		return fmt.Errorf("%w: the server certificate is not from this bundle: %w", ErrNotSignedByCA, err)
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return fmt.Errorf("%w: the server certificate: %w", ErrValidity, err)
	case errors.As(err, &invalid) && invalid.Reason == x509.IncompatibleUsage:
		return fmt.Errorf("%w: the certificate of the server is not a server certificate: %w", ErrRole, err)
	default:
		return fmt.Errorf("the server certificate is refused: %w", err)
	}
}
