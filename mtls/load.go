package mtls

import (
	"bytes"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/hekmon/rat/tmux/names"
)

// The checks of Load, in their order. An invalid name wraps names.ErrInvalid.
var (
	// ErrFiles is returned for a directory not holding the files of one side of a bundle: ca.crt,
	// then server.crt and server.key, or client.crt and client.key, each a single PEM block. Parse
	// returns it for a content that is not a single PEM block of its kind.
	ErrFiles = errors.New("not the directory of a side of a bundle")
	// ErrKeyPermissions is returned for a private key readable by others than its owner: it is a
	// credential (a client key grants a shell). Not checked on Windows, where modes mean nothing.
	ErrKeyPermissions = errors.New("private key readable by others")
	// ErrKeyMismatch is returned for a private key not matching the certificate.
	ErrKeyMismatch = errors.New("private key not matching the certificate")
	// ErrNotSignedByCA is returned for a certificate not signed by the CA of its directory, or a
	// CA certificate that is not one.
	ErrNotSignedByCA = errors.New("certificate not signed by the CA")
	// ErrRole is returned for a certificate not authenticating the side its file names, or not
	// only it.
	ErrRole = errors.New("certificate of another role")
	// ErrValidity is returned for a certificate, or its CA, expired or not valid yet.
	ErrValidity = errors.New("certificate not valid now")
)

// Check is a check of Load, for a caller reporting each (rat-tool). Parse runs them too, but for
// the permissions of the key.
type Check struct {
	// Name tells what the check requires, for humans.
	Name string
	// Err is the sentinel the error of Load wraps when the check fails.
	Err error
}

// Checks returns the checks of Load, in the order it runs them: when Load fails, the checks
// before the one whose Err its error wraps have passed.
func Checks() []Check {
	return []Check{
		{"files of a side", ErrFiles},
		{"key readable by its owner only", ErrKeyPermissions},
		{"key matching the certificate", ErrKeyMismatch},
		{"certificate signed by the CA", ErrNotSignedByCA},
		{"certificate of its role only", ErrRole},
		{"certificates valid now", ErrValidity},
		{"plain names", names.ErrInvalid},
	}
}

// Side is a side of a bundle, loaded from its directory or parsed from its contents, and checked:
// what ratd, or one of its clients, holds.
type Side struct {
	// Role is the side the directory is for, named by its certificate file and carried by its
	// certificate.
	Role Role
	// Name is the name the certificate carries: the tenant for the server, the client for a
	// client, which is its session.
	Name string
	// Tenant is the tenant of the bundle, named by its CA.
	Tenant string
	// CA is the certificate of the bundle CA.
	CA *x509.Certificate
	// Certificate is the certificate of the side, with its private key and parsed leaf.
	Certificate tls.Certificate
}

// NotAfter returns when the side stops working: the expiry of its certificate or of the CA,
// whichever comes first.
func (s *Side) NotAfter() time.Time {
	if s.CA.NotAfter.Before(s.Certificate.Leaf.NotAfter) {
		return s.CA.NotAfter
	}
	return s.Certificate.Leaf.NotAfter
}

// Load reads the directory of a side of a bundle, and checks it, to fail when starting rather than
// at the first connection, where a misconfiguration only shows as an opaque handshake error.
// The side is the one whose certificate file the directory holds. The checks run in this order,
// and the error wraps the sentinel of the first one failing, the checks before it having passed:
//
//  1. files (ErrFiles): the files of one side are present, each a single PEM block: several
//     certificates in ca.crt would all be trusted;
//  2. key permissions (ErrKeyPermissions): the key is readable by its owner only (not on Windows);
//  3. key (ErrKeyMismatch): the key matches the certificate;
//  4. CA (ErrNotSignedByCA): ca.crt is a CA, which signed the certificate;
//  5. role (ErrRole): the certificate authenticates the side its file names, and only it;
//  6. validity (ErrValidity): the certificate and the CA are valid now;
//  7. names (names.ErrInvalid): the certificate and the CA carry plain names.
func Load(dir string) (*Side, error) {
	return load(dir, time.Now())
}

// Parse returns the side role of a bundle from the contents of its PEM files: the CA certificate,
// the certificate of the side and its key. It is Load for credentials that are not in a directory
// (a secret store, an embedded asset): the same checks, in the same order, apart from what only
// files have. The first, ErrFiles, checks each content is a single PEM block of its kind; the
// permissions of a key (ErrKeyPermissions) are its holder's concern. The side is the one role
// names, which a directory tells by the names of its files.
func Parse(role Role, ca, certificate, key []byte) (*Side, error) {
	return parse(role, ca, certificate, key, time.Now())
}

// parse is Parse at the time now, which tests move.
func parse(role Role, caPEM, certPEM, keyPEM []byte, now time.Time) (*Side, error) {
	if role != Server && role != Client {
		return nil, fmt.Errorf("%w: unknown role %d", ErrRole, role)
	}
	// 1. files, as contents
	labels := sideLabels{ca: "the CA certificate", cert: "the certificate", key: "the key"}
	ca, err := parseCertificate(caPEM, labels.ca)
	if err != nil {
		return nil, err
	}
	cert, err := parseCertificate(certPEM, labels.cert)
	if err != nil {
		return nil, err
	}
	key, err := parseKey(keyPEM, labels.key)
	if err != nil {
		return nil, err
	}
	return checkSide(role, ca, cert, key, labels, now)
}

// load is Load at the time now, which tests move.
func load(dir string, now time.Time) (*Side, error) {
	// 1. files
	role, err := sideRole(dir)
	if err != nil {
		return nil, err
	}
	ca, err := readCertificate(filepath.Join(dir, caFile))
	if err != nil {
		return nil, err
	}
	cert, err := readCertificate(filepath.Join(dir, certFile(role)))
	if err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, keyFile(role))
	key, err := readKey(keyPath)
	if err != nil {
		return nil, err
	}
	// 2. key permissions
	if err = checkKeyPermissions(keyPath); err != nil {
		return nil, err
	}
	return checkSide(role, ca, cert, key, sideLabels{ca: caFile, cert: certFile(role), key: keyPath}, now)
}

// sideLabels name the parts of a side in errors: file names for Load, their kinds for Parse.
type sideLabels struct {
	ca, cert, key string
}

// checkSide runs the checks of Load after the files (3 to 7) on the parts of a side of role, read
// and parsed, and returns the side.
func checkSide(role Role, ca, cert *x509.Certificate, key crypto.Signer, labels sideLabels, now time.Time) (*Side, error) {
	// 3. key
	if public, ok := key.Public().(interface{ Equal(crypto.PublicKey) bool }); !ok || !public.Equal(cert.PublicKey) {
		return nil, fmt.Errorf("%w: %s does not match %s", ErrKeyMismatch, labels.key, labels.cert)
	}
	// 4. CA. CheckSignatureFrom also refuses a signer that is not a CA (basic constraints) or not
	// allowed to sign certificates (key usage): ca.crt needs no check of its own.
	if err := cert.CheckSignatureFrom(ca); err != nil {
		return nil, fmt.Errorf("%w: %s is not signed by %s: %w", ErrNotSignedByCA, labels.cert, labels.ca, err)
	}
	// 5. role: exactly the one of the side, so that a client can not run a server with its
	// certificate, nor the other way round. Stricter than the verification of Go (TLS handshakes),
	// which accepts a certificate carrying both roles.
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != role.usage() {
		return nil, fmt.Errorf("%w: %s is not a %s certificate only", ErrRole, labels.cert, role)
	}
	// 6. validity
	for _, c := range []struct {
		file string
		cert *x509.Certificate
	}{{labels.cert, cert}, {labels.ca, ca}} {
		if now.Before(c.cert.NotBefore) {
			return nil, fmt.Errorf("%w: %s is not valid before %s", ErrValidity, c.file, c.cert.NotBefore.Format(time.RFC3339))
		}
		if now.After(c.cert.NotAfter) {
			return nil, fmt.Errorf("%w: %s expired on %s", ErrValidity, c.file, c.cert.NotAfter.Format(time.RFC3339))
		}
	}
	// 7. names
	if err := names.Check(cert.Subject.CommonName); err != nil {
		return nil, fmt.Errorf("%s: %w", labels.cert, err)
	}
	if err := names.Check(ca.Subject.CommonName); err != nil {
		return nil, fmt.Errorf("%s: %w", labels.ca, err)
	}
	return &Side{
		Role:   role,
		Name:   cert.Subject.CommonName,
		Tenant: ca.Subject.CommonName,
		CA:     ca,
		// The chain sent is the certificate alone: the peer holds the CA.
		Certificate: tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key, Leaf: cert},
	}, nil
}

// sideRole returns the role whose certificate file dir holds.
func sideRole(dir string) (Role, error) {
	// a missing directory would read as holding no certificate
	if _, err := os.Stat(dir); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrFiles, err)
	}
	server := fileExists(filepath.Join(dir, certFile(Server)))
	client := fileExists(filepath.Join(dir, certFile(Client)))
	switch {
	case server && client:
		return 0, fmt.Errorf("%w: %s holds both %s and %s", ErrFiles, dir, certFile(Server), certFile(Client))
	case server:
		return Server, nil
	case client:
		return Client, nil
	default:
		return 0, fmt.Errorf("%w: %s holds neither %s nor %s", ErrFiles, dir, certFile(Server), certFile(Client))
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// decodePEM returns the content of the single PEM block of pemType data holds, label naming data
// in errors.
func decodePEM(data []byte, pemType, label string) ([]byte, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != pemType {
		return nil, fmt.Errorf("%w: %s does not hold a PEM %s", ErrFiles, label, pemType)
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, fmt.Errorf("%w: %s holds more than a PEM %s", ErrFiles, label, pemType)
	}
	return block.Bytes, nil
}

// parseCertificate returns the certificate of the PEM data, label naming it in errors.
func parseCertificate(data []byte, label string) (*x509.Certificate, error) {
	der, err := decodePEM(data, "CERTIFICATE", label)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrFiles, label, err)
	}
	return cert, nil
}

// parseKey returns the signing key of the PEM data, label naming it in errors.
func parseKey(data []byte, label string) (crypto.Signer, error) {
	der, err := decodePEM(data, "PRIVATE KEY", label)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrFiles, label, err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("%w: %s does not hold a signing key", ErrFiles, label)
	}
	return signer, nil
}

// readFile returns the content of the file at path.
func readFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFiles, err)
	}
	return data, nil
}

func readCertificate(path string) (*x509.Certificate, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return parseCertificate(data, path)
}

func readKey(path string) (crypto.Signer, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return parseKey(data, path)
}

// checkKeyPermissions refuses a key readable by others than its owner, as ssh does: a key copied
// without its mode (0600) gets the one of the umask, usually readable by every user.
func checkKeyPermissions(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrFiles, err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %04o, run: chmod 600 %s", ErrKeyPermissions, path, mode, path)
	}
	return nil
}
