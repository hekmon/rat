package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

// ServerConfig returns the TLS configuration of ratd, from the server side of its bundle: it
// presents the server certificate, and requires a client certificate of the bundle (signed by
// its CA, valid, clientAuth), which Go checks by itself. TLS 1.3 only. It sets nothing of the
// protocol above TLS (ALPN), which is ratd's to choose.
//
// It does not check revocation: ratd checks it, and validity again, on every request
// (Side.CheckPeer), which a connection kept alive outlives a handshake for. Refused during the
// handshake, a client would only learn "bad certificate", the alert Go sends whatever the reason.
//
// The certificate is the one of the side of source at each handshake: a Watcher renews it while
// ratd runs, its CA unchanged (see Watcher).
// The error wraps ErrRole for the side of a client: a client can not run a server.
func ServerConfig(source Source) (*tls.Config, error) {
	side := source.Side()
	if side.Role != Server {
		return nil, fmt.Errorf("%w: the directory of a %s can not serve", ErrRole, side.Role)
	}
	config := &tls.Config{
		Certificates: []tls.Certificate{side.Certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		// the CA of a Watcher never changes: a new one takes a restart
		ClientCAs:  pool(side.CA),
		MinVersion: tls.VersionTLS13,
		// Go accepts a client certificate carrying the server role too, or none (see onlyRole):
		// refused here with "bad certificate", as a certificate of the wrong role is.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) > 0 && !onlyRole(state.PeerCertificates[0], Client) {
				return fmt.Errorf("%w: the certificate of %s is not a client certificate only", ErrRole,
					state.PeerCertificates[0].Subject.CommonName)
			}
			return nil
		},
	}
	// The configuration of each handshake, with the certificate of the side in use. Not
	// GetCertificate, which Go skips for Certificates when the client names no host (an address),
	// as httptest sets them. A copy made at the handshake keeps what the caller set after
	// ServerConfig (ALPN).
	config.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		handshake := config.Clone()
		handshake.GetConfigForClient = nil
		handshake.Certificates = []tls.Certificate{source.Side().Certificate}
		return handshake, nil
	}
	return config, nil
}

// ClientConfig returns the TLS configuration of a client of ratd, from the side of the client in
// the bundle: it presents the client certificate, and accepts a server certificate of the bundle
// (signed by its CA, valid, serverAuth, not revoked) without checking the host name it reaches,
// which the server certificate does not carry. TLS 1.3 only. A server refused fails the handshake
// with an error wrapping ErrNotSignedByCA, ErrValidity, ErrRole, ErrRevoked or ErrCRL (see
// Side.CheckRevoked: the CRL is fetched at the first handshake needing it).
//
// The certificate presented, and the CRL checked, are those of the side of source at each
// handshake: a Watcher renews them while the client runs, its CA unchanged (see Watcher).
// The error wraps ErrRole for the side of the server: the server can not stand for a client.
func ClientConfig(source Source) (*tls.Config, error) {
	side := source.Side()
	if side.Role != Client {
		return nil, fmt.Errorf("%w: the directory of the %s can not be a client", ErrRole, side.Role)
	}
	// the CA of a Watcher never changes: a new one takes a restart
	roots := pool(side.CA)
	return &tls.Config{
		GetClientCertificate: func(request *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			certificate := source.Side().Certificate
			// No certificate for a server asking for another CA, as Go does with Certificates: a
			// ratd of another tenant then tells "certificate required" (see TestConfigs).
			if request.SupportsCertificate(&certificate) != nil {
				return &tls.Certificate{}, nil
			}
			return &certificate, nil
		},
		MinVersion: tls.VersionTLS13,
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
			// The server sends its certificate alone: no intermediate, the CA signs no CA.
			_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:     roots,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			if err != nil {
				return verificationError(err)
			}
			// Go accepts a server certificate carrying the client role too, or none (see onlyRole)
			if !onlyRole(state.PeerCertificates[0], Server) {
				return fmt.Errorf("%w: the certificate of the server is not a server certificate only", ErrRole)
			}
			// Bounded by the timeout of a fetch: VerifyConnection is given no context.
			return source.Side().CheckRevoked(context.Background(), state.PeerCertificates[0])
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
