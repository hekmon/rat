package mtls

import (
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"strings"
	"time"
)

// Role is what a certificate authenticates, carried by its extended key usage: ratd, or one of its
// clients. Neither can stand for the other.
type Role int

const (
	// Server is ratd, serving the tenant its certificate names (serverAuth).
	Server Role = iota + 1
	// Client is a client of ratd, working in the session its certificate names (clientAuth).
	Client
)

// String returns the name of the role, which is also the base name of its files (server.crt).
func (r Role) String() string {
	switch r {
	case Server:
		return "server"
	case Client:
		return "client"
	default:
		return fmt.Sprintf("Role(%d)", int(r))
	}
}

// usage returns the extended key usage carrying the role.
func (r Role) usage() x509.ExtKeyUsage {
	if r == Server {
		return x509.ExtKeyUsageServerAuth
	}
	return x509.ExtKeyUsageClientAuth
}

// caFile is the name of the CA certificate in the directory of each side.
const caFile = "ca.crt"

// certFile and keyFile return the names of the certificate and key of role in its directory.
func certFile(r Role) string { return r.String() + ".crt" }
func keyFile(r Role) string  { return r.String() + ".key" }

// ExpiryWarning is how long before a bundle expires its sides warn about it: an expired bundle
// stops every client at once, years after anyone remembers how it was made.
const ExpiryWarning = 365 * 24 * time.Hour

// Fingerprint returns the SHA-256 fingerprint of cert, as openssl prints it (colon separated
// uppercase hex). The fingerprint of its CA identifies a bundle.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	hex := make([]string, len(sum))
	for i, b := range sum {
		hex[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(hex, ":")
}
