package mtls

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// crlCA returns a CA named name allowed to sign CRLs, as the sub CA of another PKI is: the CA of
// a bundle is not, nobody holding its key to sign one anyway.
func crlCA(t *testing.T, name string) credential {
	t.Helper()
	ca, err := create(&x509.Certificate{
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// crlSide returns the side of role named name, issued by ca, its certificate naming points as its
// CRL distribution points, checked as Load checks a side.
func crlSide(t *testing.T, ca credential, role Role, name string, points ...string) *Side {
	t.Helper()
	c, err := create(&x509.Certificate{
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{role.usage()},
		CRLDistributionPoints: points,
	}, &ca)
	if err != nil {
		t.Fatal(err)
	}
	side, err := checkSide(role, ca.cert, c.cert, c.key, sideLabels{ca: "ca.crt", cert: "cert", key: "key"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return side
}

// crlOf returns the CRL of ca in DER, valid from an hour ago for validity, revoking the
// certificates of serials, with extra extensions, and extra on each entry.
func crlOf(t *testing.T, ca credential, validity time.Duration, serials []*big.Int, extra, entryExtra []pkix.Extension) []byte {
	t.Helper()
	thisUpdate := time.Now().Add(-time.Hour)
	entries := make([]x509.RevocationListEntry, 0, len(serials))
	for _, serial := range serials {
		entries = append(entries, x509.RevocationListEntry{SerialNumber: serial, RevocationTime: thisUpdate,
			ExtraExtensions: entryExtra})
	}
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number:                    big.NewInt(1),
		ThisUpdate:                thisUpdate,
		NextUpdate:                thisUpdate.Add(validity),
		RevokedCertificateEntries: entries,
		ExtraExtensions:           extra,
	}, ca.cert, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// TestParseCRL guards the checks of a CRL: DER or PEM, issued and signed by the CA, refused when
// partial, delta or indirect (a critical extension Go does not read, which would leave revoked
// certificates out), stale, or without a next update; and what it revokes.
func TestParseCRL(t *testing.T) {
	ca := crlCA(t, "t")
	alice, bob := crlSide(t, ca, Client, "alice"), crlSide(t, ca, Client, "bob")
	day := 24 * time.Hour
	valid := crlOf(t, ca, day, []*big.Int{alice.Certificate.Leaf.SerialNumber}, nil, nil)
	pemCRL := pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: valid})
	critical := func(oid asn1.ObjectIdentifier) []pkix.Extension {
		return []pkix.Extension{{Id: oid, Critical: true, Value: []byte{0x30, 0x00}}}
	}

	for _, data := range [][]byte{valid, pemCRL} {
		crl, err := parseCRL(data, ca.cert, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if !crl.Revokes(alice.Certificate.Leaf) || crl.Revokes(bob.Certificate.Leaf) || crl.Len() != 1 {
			t.Errorf("expected alice revoked, bob not, got alice %v, bob %v, %d revoked",
				crl.Revokes(alice.Certificate.Leaf), crl.Revokes(bob.Certificate.Leaf), crl.Len())
		}
	}

	for _, tc := range []struct {
		name    string
		data    []byte
		at      time.Time
		message string
	}{
		{"not a CRL", []byte("not a CRL"), time.Now(), "x509"},
		{"PEM certificate", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}), time.Now(),
			"not a PEM X509 CRL"},
		// same name, another key
		{"CA of another bundle", crlOf(t, crlCA(t, "t"), day, nil, nil, nil), time.Now(), "not signed by the CA"},
		{"CA of another name", crlOf(t, crlCA(t, "u"), day, nil, nil, nil), time.Now(), "not issued by the CA"},
		{"partial", crlOf(t, ca, day, nil, critical(asn1.ObjectIdentifier{2, 5, 29, 28}), nil), time.Now(),
			"critical extension 2.5.29.28"},
		{"delta", crlOf(t, ca, day, nil, critical(asn1.ObjectIdentifier{2, 5, 29, 27}), nil), time.Now(),
			"critical extension 2.5.29.27"},
		{"indirect", crlOf(t, ca, day, []*big.Int{big.NewInt(1)}, nil, critical(asn1.ObjectIdentifier{2, 5, 29, 29})),
			time.Now(), "critical extension 2.5.29.29 of an entry"},
		{"stale", valid, time.Now().Add(day), "stale since"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseCRL(tc.data, ca.cert, tc.at); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Errorf("expected an error telling %q, got %v", tc.message, err)
			}
		})
	}

	// Go writes no CRL without a next update, but reads one
	t.Run("no next update", func(t *testing.T) {
		list, err := x509.ParseRevocationList(valid)
		if err != nil {
			t.Fatal(err)
		}
		list.NextUpdate = time.Time{}
		if _, err = checkCRL(list, ca.cert, time.Now()); err == nil || !strings.Contains(err.Error(), "no next update") {
			t.Errorf("expected an error telling no next update, got %v", err)
		}
	})
}

// TestCRLURL guards where the CRL of a side is fetched from: the first distribution point of its
// certificate over http or https, none when it names none, and a load refused (ErrCRL, the last
// check) when none is over HTTP: the CA revokes, rat could not tell what.
func TestCRLURL(t *testing.T) {
	ca := crlCA(t, "t")
	for _, tc := range []struct {
		name     string
		points   []string
		expected string
	}{
		{"none", nil, ""},
		{"http", []string{"http://pki.example/t.crl"}, "http://pki.example/t.crl"},
		{"https", []string{"https://pki.example/t.crl"}, "https://pki.example/t.crl"},
		{"LDAP first", []string{"ldap:///CN=t,CN=CDP?certificateRevocationList", "http://pki.example/t.crl"},
			"http://pki.example/t.crl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if address := crlSide(t, ca, Server, "t", tc.points...).CRLURL(); address != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, address)
			}
		})
	}

	c, err := create(&x509.Certificate{
		Subject: pkix.Name{CommonName: "alice"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		CRLDistributionPoints: []string{"ldap:///CN=t,CN=CDP?certificateRevocationList"},
	}, &ca)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "side")
	if err = writeSide(dir, ca.cert, c, Client); err != nil {
		t.Fatal(err)
	}
	if _, err = load(dir, time.Now()); !errors.Is(err, ErrCRL) || checkOf(err) != len(Checks())-1 {
		t.Errorf("LDAP only: expected ErrCRL, the last check, got %v", err)
	}
}

// TestSideCRL guards the CRL a side holds: none without a distribution point; fetched when first
// needed, kept while valid, fetched again once stale; ErrRevoked when it revokes the side itself
// or a peer (CheckRevoked); ErrCRL when it can not be had (an HTTP error, a body over the bound, a
// server down), no peer being accepted then.
func TestSideCRL(t *testing.T) {
	ca := crlCA(t, "t")
	var body []byte
	status, fetches := http.StatusOK, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	ctx := context.Background()
	ratd := crlSide(t, ca, Server, "t", server.URL+"/t.crl")
	alice, bob := crlSide(t, ca, Client, "alice"), crlSide(t, ca, Client, "bob")

	if crl, err := crlSide(t, ca, Server, "t").CRL(ctx); crl != nil || err != nil {
		t.Errorf("no distribution point: expected no CRL and no error, got %v and %v", crl, err)
	}

	body = crlOf(t, ca, 24*time.Hour, []*big.Int{bob.Certificate.Leaf.SerialNumber}, nil, nil)
	for range 2 {
		crl, err := ratd.CRL(ctx)
		if err != nil || crl.Address != server.URL+"/t.crl" || crl.Len() != 1 {
			t.Errorf("expected the CRL of %s, revoking one certificate, got %+v and %v", server.URL, crl, err)
		}
	}
	if fetches != 1 {
		t.Errorf("expected the CRL fetched once, while valid, got %d fetches", fetches)
	}
	if err := ratd.CheckRevoked(ctx, alice.Certificate.Leaf); err != nil {
		t.Errorf("alice: expected no error, got %v", err)
	}
	if err := ratd.CheckRevoked(ctx, bob.Certificate.Leaf); !errors.Is(err, ErrRevoked) {
		t.Errorf("bob: expected ErrRevoked, got %v", err)
	}

	ratd.revocation.crl.NextUpdate = time.Now().Add(-time.Second)
	body = crlOf(t, ca, 24*time.Hour, []*big.Int{ratd.Certificate.Leaf.SerialNumber}, nil, nil)
	if _, err := ratd.CRL(ctx); !errors.Is(err, ErrRevoked) || fetches != 2 {
		t.Errorf("stale, then revoking the side: expected ErrRevoked after a second fetch, got %v after %d", err, fetches)
	}
	if err := ratd.CheckRevoked(ctx, alice.Certificate.Leaf); !errors.Is(err, ErrRevoked) {
		t.Errorf("side revoked: expected ErrRevoked for alice too, got %v", err)
	}

	for _, tc := range []struct {
		name    string
		status  int
		body    []byte
		message string
	}{
		{"HTTP error", http.StatusNotFound, nil, "HTTP 404"},
		{"over the bound", http.StatusOK, make([]byte, maxCRLSize+1), "over 8 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body = tc.status, tc.body
			side := crlSide(t, ca, Server, "t", server.URL+"/t.crl")
			if err := side.CheckRevoked(ctx, alice.Certificate.Leaf); !errors.Is(err, ErrCRL) ||
				!strings.Contains(err.Error(), tc.message) {
				t.Errorf("expected ErrCRL telling %q, got %v", tc.message, err)
			}
		})
	}

	server.Close()
	if err := crlSide(t, ca, Server, "t", server.URL+"/t.crl").CheckRevoked(ctx, alice.Certificate.Leaf); !errors.Is(err, ErrCRL) {
		t.Errorf("server down: expected ErrCRL, got %v", err)
	}
}

// TestCheckRevokedTemplates guards a side whose certificate names no CRL refusing a peer whose
// certificate names one (ErrCRL): the CA revokes, the side can not check it, its certificate
// issued from another template. Nothing is fetched from the address of the peer. Without one on
// either side, as in a bundle, the peer is accepted.
func TestCheckRevokedTemplates(t *testing.T) {
	ca := crlCA(t, "t")
	fetched := false
	crlServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { fetched = true }))
	defer crlServer.Close()
	ratd := crlSide(t, ca, Server, "t")
	if err := ratd.CheckRevoked(context.Background(), crlSide(t, ca, Client, "alice").Certificate.Leaf); err != nil {
		t.Errorf("no CRL named on either side: expected no error, got %v", err)
	}
	alice := crlSide(t, ca, Client, "alice", crlServer.URL)
	if err := ratd.CheckRevoked(context.Background(), alice.Certificate.Leaf); !errors.Is(err, ErrCRL) ||
		!strings.Contains(err.Error(), "the certificate of alice names a CRL distribution point, the certificate of t none") {
		t.Errorf("CRL named by the peer only: expected ErrCRL telling it, got %v", err)
	}
	if fetched {
		t.Error("expected nothing fetched from the distribution point of the peer")
	}

	// a client whose certificate names no CRL refuses a server whose certificate names one
	serverConfig, err := ServerConfig(crlSide(t, ca, Server, "t", crlServer.URL))
	if err != nil {
		t.Fatal(err)
	}
	clientConfig, err := ClientConfig(crlSide(t, ca, Client, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	if c := connect(t, serverConfig, clientConfig); !errors.Is(c.client, ErrCRL) {
		t.Errorf("expected the client to refuse the server with ErrCRL, got %v", c.client)
	}
}
