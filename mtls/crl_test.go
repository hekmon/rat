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
	"sync"
	"sync/atomic"
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

	// critical extensions Go reads are accepted, as real PKIs mark them
	t.Run("critical extensions Go reads", func(t *testing.T) {
		number, err := asn1.Marshal(big.NewInt(2))
		if err != nil {
			t.Fatal(err)
		}
		crl := crlOf(t, ca, day, nil, []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 20}, Critical: true, Value: number}}, nil)
		if _, err = parseCRL(crl, ca.cert, time.Now()); err != nil {
			t.Errorf("expected a critical CRL number to be accepted, got %v", err)
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

// TestCRLTimeout guards that a fetch ends with the context of the check: a PKI that does not
// answer fails the request rather than hanging it.
func TestCRLTimeout(t *testing.T) {
	ca := crlCA(t, "t")
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	ratd := crlSide(t, ca, Server, "t", server.URL+"/t.crl")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := ratd.CheckRevoked(ctx, crlSide(t, ca, Client, "alice").Certificate.Leaf); !errors.Is(err, ErrCRL) {
		t.Errorf("expected ErrCRL, got %v", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("expected the check to end with its context, waited %s", waited)
	}
}

// TestCRLEventsReentrant guards that an event may check the side again: they are told outside the
// locks of its CRL, so that a holder reacting to a failure, by retrying, does not deadlock.
func TestCRLEventsReentrant(t *testing.T) {
	ca := crlCA(t, "t")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	ratd := crlSide(t, ca, Server, "t", server.URL+"/t.crl")
	deadline := time.After(5 * time.Second)
	done := make(chan error, 1)
	reentered := false
	ratd.OnCRL(CRLEvents{Failed: func(error, *CRL) {
		if reentered {
			return
		}
		reentered = true
		_, err := ratd.CRL(context.Background())
		done <- err
	}})
	first := make(chan error, 1)
	go func() {
		_, err := ratd.CRL(context.Background())
		first <- err
	}()
	select {
	case err := <-first:
		if !errors.Is(err, ErrCRL) {
			t.Fatalf("expected ErrCRL, got %v", err)
		}
	case <-deadline:
		t.Fatal("an event checking the side again deadlocked")
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrCRL) {
			t.Errorf("reentrant check: expected ErrCRL, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reentrant check never returned")
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

	ratd.revocation.mu.Lock()
	ratd.revocation.crl.NextUpdate = time.Now().Add(-time.Second)
	ratd.revocation.mu.Unlock()
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

// crlAt returns the CRL of ca in DER, issued at thisUpdate, stale at nextUpdate, revoking the
// certificates of serials.
func crlAt(t *testing.T, ca credential, thisUpdate, nextUpdate time.Time, serials ...*big.Int) []byte {
	t.Helper()
	entries := make([]x509.RevocationListEntry, 0, len(serials))
	for _, serial := range serials {
		entries = append(entries, x509.RevocationListEntry{SerialNumber: serial, RevocationTime: thisUpdate})
	}
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{Number: big.NewInt(1),
		ThisUpdate: thisUpdate, NextUpdate: nextUpdate, RevokedCertificateEntries: entries}, ca.cert, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// waitRefreshed waits for the fetch in the background of r, if any, to end.
func waitRefreshed(t *testing.T, r *revocation) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		r.mu.Lock()
		refreshing := r.refreshing
		r.mu.Unlock()
		if !refreshing {
			return
		}
	}
	t.Fatal("the fetch in the background did not end")
}

// TestCRLRefresh guards when a side fetches its CRL again, told by dates rather than durations: a
// machine asleep for days counts them, and a clock set back fetches. While the CRL held is valid,
// a check returns at once, fetching in the background crlRefresh after the last fetch, crlRetry
// after a failure, keeping the CRL held; a CRL older than the one held is refused. Without a
// valid CRL, every check fetches, and fails while it can not. Each fetch succeeding, newer or
// not, and each failure, is told to the events of the side.
func TestCRLRefresh(t *testing.T) {
	ca := crlCA(t, "t")
	var mu sync.Mutex
	var body []byte
	status, fetches := http.StatusOK, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fetches++
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	serve := func(s int, b []byte) {
		mu.Lock()
		defer mu.Unlock()
		status, body = s, b
	}
	fetched := func() int {
		mu.Lock()
		defer mu.Unlock()
		return fetches
	}
	start := time.Now().Round(0)
	clock := start
	defer func(now func() time.Time) { crlNow = now }(crlNow)
	crlNow = func() time.Time { return clock }

	ratd := crlSide(t, ca, Server, "t", server.URL)
	bob := crlSide(t, ca, Client, "bob")
	var news []*CRL
	var succeeded []*CRL
	var failures []error
	ratd.OnCRL(CRLEvents{
		Fetched:   func(crl *CRL) { mu.Lock(); news = append(news, crl); mu.Unlock() },
		Succeeded: func(crl *CRL) { mu.Lock(); succeeded = append(succeeded, crl); mu.Unlock() },
		Failed:    func(err error, _ *CRL) { mu.Lock(); failures = append(failures, err); mu.Unlock() },
	})
	check := func(step string, expectedFetches int, expectedErr error) {
		t.Helper()
		err := ratd.CheckRevoked(context.Background(), bob.Certificate.Leaf)
		waitRefreshed(t, ratd.revocation)
		if !errors.Is(err, expectedErr) || (expectedErr == nil) != (err == nil) || fetched() != expectedFetches {
			t.Errorf("%s: expected %v after %d fetches, got %v after %d", step, expectedErr, expectedFetches, err, fetched())
		}
	}
	week := 7 * 24 * time.Hour
	first := crlAt(t, ca, start.Add(-time.Hour), start.Add(week))
	revokingBob := crlAt(t, ca, start.Add(-time.Minute), start.Add(week), bob.Certificate.Leaf.SerialNumber)

	serve(http.StatusOK, first)
	check("first check", 1, nil)
	clock = start.Add(10 * time.Minute)
	check("within the delay", 1, nil)
	serve(http.StatusOK, revokingBob)
	clock = start.Add(16 * time.Minute)
	// the CRL held answers, the new one being fetched meanwhile
	check("delay passed", 2, nil)
	check("new CRL", 2, ErrRevoked)
	clock = clock.Add(3 * 24 * time.Hour)
	check("asleep for 3 days", 3, ErrRevoked)
	clock = clock.Add(-time.Hour)
	check("clock set back", 4, ErrRevoked)

	serve(http.StatusOK, first)
	clock = clock.Add(16 * time.Minute)
	check("older CRL fetched", 5, ErrRevoked)
	check("older CRL refused, the one held kept", 5, ErrRevoked)
	clock = clock.Add(30 * time.Second)
	check("failure retried after a minute, not before", 5, ErrRevoked)
	clock = clock.Add(31 * time.Second)
	check("failure retried", 6, ErrRevoked)

	serve(http.StatusInternalServerError, nil)
	clock = start.Add(week + time.Minute)
	check("stale, the PKI down", 7, ErrCRL)
	check("stale, fetched at each check", 8, ErrCRL)

	mu.Lock()
	defer mu.Unlock()
	if len(news) != 2 || news[1].Len() != 1 {
		t.Errorf("expected 2 new CRLs told, the second revoking bob, got %d", len(news))
	}
	if len(succeeded) != 4 {
		t.Errorf("expected 4 successful fetches told, newer or not, got %d", len(succeeded))
	}
	if len(failures) != 4 || !strings.Contains(failures[0].Error(), "before the CRL held") {
		t.Errorf("expected 4 failures told, the first refusing the older CRL, got %v", failures)
	}
}

// TestCRLConcurrentFirstFetch guards that concurrent checks before any CRL is held fetch it once,
// the others using what the first fetched: concurrent callers must not become concurrent fetches
// against the PKI.
func TestCRLConcurrentFirstFetch(t *testing.T) {
	ca := crlCA(t, "t")
	body := crlOf(t, ca, 24*time.Hour, nil, nil, nil)
	var fetches atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	ratd := crlSide(t, ca, Server, "t", server.URL+"/t.crl")
	alice := crlSide(t, ca, Client, "alice")
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- ratd.CheckRevoked(context.Background(), alice.Certificate.Leaf)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent check: expected no error, got %v", err)
		}
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("expected the CRL fetched once, got %d fetches", n)
	}
}

// TestCRLWaitersShareFailure guards that checks waiting on a fetch failing take its error rather
// than fetching each in turn: during an outage of the PKI, the last of N checks would otherwise
// wait for N fetches, up to crlTimeout each. A check coming after the failure fetches again.
func TestCRLWaitersShareFailure(t *testing.T) {
	ca := crlCA(t, "t")
	var fetches atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fetches.Add(1) == 1 {
			close(started)
			<-release
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	ratd := crlSide(t, ca, Server, "t", server.URL+"/t.crl")
	const checks = 5
	errs := make(chan error, checks)
	check := func() {
		_, err := ratd.CRL(context.Background())
		errs <- err
	}
	go check()
	<-started
	for range checks - 1 {
		go check()
	}
	// the others waiting on the fetch in flight
	time.Sleep(100 * time.Millisecond)
	close(release)
	for range checks {
		if err := <-errs; !errors.Is(err, ErrCRL) {
			t.Errorf("expected ErrCRL, got %v", err)
		}
	}
	// one fetch, two if a check came late, after it ended: not one per check
	if n := fetches.Load(); n > 2 {
		t.Errorf("expected the checks waiting to share the failure, got %d fetches for %d checks", n, checks)
	}
	if _, err := ratd.CRL(context.Background()); !errors.Is(err, ErrCRL) || fetches.Load() < 2 {
		t.Errorf("a check after the failure: expected a fetch of its own, got %v after %d fetches", err, fetches.Load())
	}
}
