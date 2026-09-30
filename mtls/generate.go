package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/hekmon/rat/tmux/names"
)

// validityYears is how long the certificates of a bundle are valid: rotating is generating a new
// bundle when needed, not a schedule an expiry would impose.
const validityYears = 10

// clockSkew is how long before its generation a bundle is valid: a peer whose clock runs a few
// minutes late would otherwise reject a bundle it just received, with an error saying little.
const clockSkew = 24 * time.Hour

// caOrganization is the organization of the CA subject, which also names the tenant: it keeps
// that subject distinct from the server certificate, which names the tenant too, so that the
// server certificate is not self-issued (subject equal to issuer, RFC 5280).
const caOrganization = "rat CA"

// Bundle describes a generated bundle.
type Bundle struct {
	// Tenant is the tenant the bundle is for, named by its CA and its server certificate.
	Tenant string
	// Clients are the clients of the tenant, each named by its certificate, which is its session.
	Clients []string
	// CAFingerprint identifies the bundle: every directory of the bundle holds its CA.
	CAFingerprint string
	// NotAfter is when every certificate of the bundle expires.
	NotAfter time.Time
}

// Generate writes the bundle of tenant into dir: a CA, a server certificate naming the tenant,
// and a certificate per client, naming it. Names are plain names (package names), at least one
// client is required, and client names are unique. The CA private key is never written: no
// certificate can be added to the bundle afterwards.
//
// dir must not exist, even empty: replacing a bundle silently breaks every client deployed with
// it, and the error then wraps fs.ErrExist. Missing parent directories are created. The bundle is
// written as below, keys and the directories holding them readable by their owner only:
//
//	dir/server/          ca.crt  server.crt  server.key
//	dir/clients/<name>/  ca.crt  client.crt  client.key
//
// On failure, dir is removed if it was created (parents created stay).
func Generate(dir, tenant string, clients []string) (Bundle, error) {
	return generate(dir, tenant, clients, time.Now())
}

// generate is Generate at the time now, which tests move.
func generate(dir, tenant string, clients []string, now time.Time) (bundle Bundle, err error) {
	if err = names.Check(tenant); err != nil {
		return bundle, fmt.Errorf("tenant: %w", err)
	}
	if len(clients) == 0 {
		return bundle, errors.New("at least one client is required")
	}
	seen := make(map[string]bool, len(clients))
	for _, client := range clients {
		if err = names.Check(client); err != nil {
			return bundle, fmt.Errorf("client: %w", err)
		}
		if seen[client] {
			return bundle, fmt.Errorf("client %q given twice", client)
		}
		seen[client] = true
	}
	// Issue everything in memory first: writing is then the only step that can fail halfway.
	notBefore := now.Add(-clockSkew)
	notAfter := notBefore.AddDate(validityYears, 0, 0)
	ca, err := newCA(tenant, notBefore, notAfter)
	if err != nil {
		return bundle, err
	}
	server, err := issue(ca, tenant, Server, notBefore, notAfter)
	if err != nil {
		return bundle, err
	}
	clientCredentials := make([]credential, len(clients))
	for i, client := range clients {
		if clientCredentials[i], err = issue(ca, client, Client, notBefore, notAfter); err != nil {
			return bundle, err
		}
	}
	// Parents as mkdir -p would create them: they hold no key. The bundle directory itself is
	// created by Mkdir, which fails if it exists: checking first would leave a window between the
	// check and the creation.
	if err = os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return bundle, fmt.Errorf("failed to create the parent of the bundle directory: %w", err)
	}
	if err = os.Mkdir(dir, 0o700); err != nil {
		return bundle, fmt.Errorf("failed to create the bundle directory: %w", err)
	}
	defer func() {
		if err != nil {
			// dir did not exist before: nothing but this bundle is removed
			_ = os.RemoveAll(dir)
		}
	}()
	if err = writeSide(filepath.Join(dir, Server.String()), ca.cert, server, Server); err != nil {
		return bundle, err
	}
	// Clients have a directory of their own, so that a client named "server" does not collide
	// with the server.
	clientsDir := filepath.Join(dir, "clients")
	if err = os.Mkdir(clientsDir, 0o700); err != nil {
		return bundle, fmt.Errorf("failed to create the clients directory: %w", err)
	}
	for i, client := range clients {
		if err = writeSide(filepath.Join(clientsDir, client), ca.cert, clientCredentials[i], Client); err != nil {
			return bundle, err
		}
	}
	return Bundle{
		Tenant:        tenant,
		Clients:       clients,
		CAFingerprint: Fingerprint(ca.cert),
		// as encoded in the certificates, to the second: what loading a side tells
		NotAfter: ca.cert.NotAfter,
	}, nil
}

// credential is a certificate and its private key.
type credential struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// newCA returns the CA of the bundle of tenant, which only signs the certificates of the bundle.
func newCA(tenant string, notBefore, notAfter time.Time) (ca credential, err error) {
	template := &x509.Certificate{
		// The CA names the tenant, so that a client directory tells which tenant it reaches.
		Subject:               pkix.Name{Organization: []string{caOrganization}, CommonName: tenant},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
		// No intermediate CA can be signed. MaxPathLenZero is required: Go reads MaxPathLen 0
		// alone as unset.
		MaxPathLen:     0,
		MaxPathLenZero: true,
	}
	return create(template, nil)
}

// issue returns a certificate for role, carrying name, signed by ca.
func issue(ca credential, name string, role Role, notBefore, notAfter time.Time) (credential, error) {
	template := &x509.Certificate{
		// No address (subject alternative name): clients do not check the host name they reach.
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		BasicConstraintsValid: true,
		// TLS 1.3 key exchange is always ephemeral Diffie-Hellman: the key only signs.
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{role.usage()},
	}
	return create(template, &ca)
}

// create generates an ECDSA P-256 key and its certificate from template, signed by parent, or
// self-signed if parent is nil.
func create(template *x509.Certificate, parent *credential) (c credential, err error) {
	if c.key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
		return c, fmt.Errorf("failed to generate a key: %w", err)
	}
	// 128 random bits: never negative, and unique without a registry of issued serials
	serial := make([]byte, 16)
	_, _ = rand.Read(serial) // never fails, as documented
	template.SerialNumber = new(big.Int).SetBytes(serial)
	signer, signerCert := c.key, template
	if parent != nil {
		signer, signerCert = parent.key, parent.cert
	}
	der, err := x509.CreateCertificate(rand.Reader, template, signerCert, &c.key.PublicKey, signer)
	if err != nil {
		return c, fmt.Errorf("failed to create the certificate of %q: %w", template.Subject.CommonName, err)
	}
	if c.cert, err = x509.ParseCertificate(der); err != nil {
		return c, fmt.Errorf("failed to read the certificate of %q: %w", template.Subject.CommonName, err)
	}
	return c, nil
}

// writeSide writes the directory of a side: the CA certificate, and the certificate and key of the
// side. dir must not exist.
func writeSide(dir string, ca *x509.Certificate, c credential, role Role) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create the %s directory: %w", role, err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(c.key)
	if err != nil {
		return fmt.Errorf("failed to encode the %s key: %w", role, err)
	}
	for _, file := range []struct {
		name, pemType string
		content       []byte
		mode          os.FileMode
	}{
		{caFile, "CERTIFICATE", ca.Raw, 0o644},
		{certFile(role), "CERTIFICATE", c.cert.Raw, 0o644},
		{keyFile(role), "PRIVATE KEY", key, 0o600},
	} {
		if err = writePEM(filepath.Join(dir, file.name), file.pemType, file.content, file.mode); err != nil {
			return err
		}
	}
	return nil
}

// writePEM writes content as a single PEM block into a new file at path.
func writePEM(path, pemType string, content []byte, mode os.FileMode) (err error) {
	// O_EXCL: never over an existing file, whatever the directory holds
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", path, err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("failed to write %s: %w", path, closeErr)
		}
	}()
	if err = pem.Encode(f, &pem.Block{Type: pemType, Bytes: content}); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return nil
}
