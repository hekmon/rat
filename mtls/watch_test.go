package mtls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// renewal returns a certificate of role named name, signed by ca, for key, or a new key if nil,
// naming points as its CRL distribution points.
func renewal(t *testing.T, ca credential, role Role, name string, key *ecdsa.PrivateKey, points ...string) credential {
	t.Helper()
	if key == nil {
		var err error
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			t.Fatal(err)
		}
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{role.usage()}, CRLDistributionPoints: points,
	}, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return credential{cert: cert, key: key}
}

// rewrite replaces the files of the side of role in dir, stamped modified at modified, as a
// renewal copies them.
func rewrite(t *testing.T, dir string, ca *x509.Certificate, c credential, role Role, modified time.Time) {
	t.Helper()
	for _, file := range []string{caFile, certFile(role), keyFile(role)} {
		_ = os.Remove(filepath.Join(dir, file))
	}
	if err := writeSide(dir, ca, c, role); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{caFile, certFile(role), keyFile(role)} {
		if err := os.Chtimes(filepath.Join(dir, file), modified, modified); err != nil {
			t.Fatal(err)
		}
	}
}

// TestWatcher guards what a Watcher takes of its files changing: a certificate renewed, with the
// same key or a new one (told), used from then on; a new CA (which takes a restart), another
// name, or a file missing refused, told once per change, the side in use kept; the files restored
// after a refusal told, copied again unchanged not; changes looked at once per watchInterval at
// most. The events and the CRL held carry over
// to the side renewed.
func TestWatcher(t *testing.T) {
	ca := crlCA(t, "t")
	crlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(crlOf(t, ca, 24*time.Hour, nil, nil, nil))
	}))
	defer crlServer.Close()
	dir := filepath.Join(t.TempDir(), "alice")
	modified := time.Now().Add(-time.Hour)
	first := renewal(t, ca, Client, "alice", nil, crlServer.URL)
	rewrite(t, dir, ca.cert, first, Client, modified)
	w, err := Watch(dir)
	if err != nil {
		t.Fatal(err)
	}
	type told struct {
		reloaded *Side
		newKey   bool
		refused  error
		restored bool
	}
	var events []told
	w.OnWatch(WatchEvents{
		Reloaded: func(side *Side, newKey bool) { events = append(events, told{reloaded: side, newKey: newKey}) },
		Refused:  func(err error) { events = append(events, told{refused: err, restored: err == nil}) },
	})
	crlFetched := 0
	w.OnCRL(CRLEvents{Fetched: func(*CRL) { crlFetched++ }})
	held, err := w.Side().CRL(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// look now, past the interval
	look := func() *Side {
		t.Helper()
		w.mu.Lock()
		w.checked = time.Time{}
		w.mu.Unlock()
		return w.Side()
	}
	change := func(ca *x509.Certificate, c credential) {
		t.Helper()
		modified = modified.Add(time.Minute)
		rewrite(t, dir, ca, c, Client, modified)
	}
	expect := func(step string, side *Side, expected ...told) {
		t.Helper()
		if len(events) != len(expected) {
			t.Fatalf("%s: expected %d events, got %+v", step, len(expected), events)
		}
		for i, e := range expected {
			got := events[i]
			if (e.reloaded == nil) != (got.reloaded == nil) || e.newKey != got.newKey || !errors.Is(got.refused, e.refused) ||
				e.restored != got.restored {
				t.Errorf("%s: expected %+v, got %+v", step, e, got)
			}
		}
		events = nil
		if !side.Certificate.Leaf.Equal(w.Side().Certificate.Leaf) {
			t.Errorf("%s: expected the side in use to be %s", step, side.Certificate.Leaf.SerialNumber)
		}
	}

	change(ca.cert, first)
	look()
	expect("files copied again, unchanged", first.side(t, w))

	sameKey := renewal(t, ca, Client, "alice", first.key, crlServer.URL)
	change(ca.cert, sameKey)
	if side := w.Side(); !side.Certificate.Leaf.Equal(first.cert) {
		t.Errorf("within the interval: expected the files not looked at, got %s", side.Certificate.Leaf.SerialNumber)
	}
	renewed := look()
	expect("renewed, same key", renewed, told{reloaded: renewed})
	if !renewed.Certificate.Leaf.Equal(sameKey.cert) {
		t.Errorf("renewed: expected the new certificate in use")
	}
	if crl, _ := renewed.CRL(context.Background()); crl != held || crlFetched != 1 {
		t.Errorf("renewed: expected the CRL held carried over, not fetched again, got %d fetches", crlFetched)
	}
	if len(renewed.revocation.events) != 1 {
		t.Errorf("renewed: expected the CRL events carried over, got %d", len(renewed.revocation.events))
	}

	newKey := renewal(t, ca, Client, "alice", nil, crlServer.URL)
	change(ca.cert, newKey)
	rekeyed := look()
	expect("renewed, new key", rekeyed, told{reloaded: rekeyed, newKey: true})

	other := crlCA(t, "t")
	change(other.cert, renewal(t, other, Client, "alice", nil))
	look()
	expect("new CA", rekeyed, told{refused: ErrCAChanged})
	look()
	expect("new CA, looked at again", rekeyed)
	change(ca.cert, newKey)
	look()
	expect("files restored", rekeyed, told{restored: true})

	change(ca.cert, renewal(t, ca, Client, "bob", nil))
	look()
	expect("another client", rekeyed, told{refused: ErrIdentityChanged})

	change(ca.cert, newKey)
	look()
	expect("files restored after another client", rekeyed, told{restored: true})

	_ = os.Remove(filepath.Join(dir, "client.key"))
	look()
	expect("key missing", rekeyed, told{refused: ErrFiles})
	look()
	expect("key missing, looked at again", rekeyed)
	change(ca.cert, newKey)
	look()
	expect("files restored after a missing key", rekeyed, told{restored: true})
}

// TestWatcherHandshake guards that ratd presents a certificate renewed at the next handshake,
// with the configuration it built when starting.
func TestWatcherHandshake(t *testing.T) {
	ca := crlCA(t, "t")
	dir := filepath.Join(t.TempDir(), "server")
	modified := time.Now().Add(-time.Hour)
	rewrite(t, dir, ca.cert, renewal(t, ca, Server, "t", nil), Server, modified)
	w, err := Watch(dir)
	if err != nil {
		t.Fatal(err)
	}
	config, err := ServerConfig(w)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = tls.Server(conn, config).Handshake()
			}()
		}
	}()
	alice := renewal(t, ca, Client, "alice", nil)
	presented := func() *x509.Certificate {
		t.Helper()
		conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{InsecureSkipVerify: true,
			Certificates: []tls.Certificate{{Certificate: [][]byte{alice.cert.Raw}, PrivateKey: alice.key}}})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0]
	}
	presented()
	renewed := renewal(t, ca, Server, "t", nil)
	rewrite(t, dir, ca.cert, renewed, Server, modified.Add(time.Minute))
	w.mu.Lock()
	w.checked = time.Time{}
	w.mu.Unlock()
	if cert := presented(); !cert.Equal(renewed.cert) {
		t.Errorf("expected the renewed certificate presented, got serial %s", cert.SerialNumber)
	}
}

// side returns the side in use of w, for c to be compared with it.
func (c credential) side(t *testing.T, w *Watcher) *Side {
	t.Helper()
	side := w.Side()
	if !side.Certificate.Leaf.Equal(c.cert) {
		t.Errorf("expected the side in use to hold serial %s, got %s", c.cert.SerialNumber, side.Certificate.Leaf.SerialNumber)
	}
	return side
}
