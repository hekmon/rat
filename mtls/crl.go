package mtls

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"
)

var (
	// ErrCRL is returned when no valid CRL of the CA of a side can be had: its certificate names
	// distribution points none of which is over HTTP, or the CRL could not be fetched, is refused,
	// or is stale.
	ErrCRL = errors.New("no valid CRL of the CA")
	// ErrRevoked is returned for a certificate the CRL of its CA revokes.
	ErrRevoked = errors.New("certificate revoked")
)

// maxCRLSize bounds a CRL fetched: a CA dedicated to rat revokes a handful of certificates, and a
// server answering endlessly must not exhaust the memory of ratd.
const maxCRLSize = 8 << 20

// crlTimeout bounds a fetch, from the connection to the last byte.
const crlTimeout = 10 * time.Second

// crlClient fetches CRLs, through the proxy of the environment, as http.DefaultTransport does.
var crlClient = &http.Client{Timeout: crlTimeout}

// The extensions of a CRL, and of its entries, that a CRL checked may carry as critical: those Go
// reads. Go does not refuse the others, which a critical mark means the CRL can not be understood
// without: a partial CRL (issuing distribution point), a delta CRL, an indirect CRL (certificate
// issuer of an entry). Read as complete, they would leave revoked certificates out.
var (
	criticalCRLExtensions = []asn1.ObjectIdentifier{
		{2, 5, 29, 20}, // CRL number
		{2, 5, 29, 35}, // authority key identifier
	}
	criticalEntryExtensions = []asn1.ObjectIdentifier{
		{2, 5, 29, 21}, // reason code
	}
)

// crlAddress returns the address the CRL of the CA of cert is fetched from: the first
// distribution point of cert over http or https, every certificate of the CA naming the same CRL.
// It returns "" when cert names none: the CA revokes nothing (a bundle, a PKI issuing short-lived
// certificates). The error wraps ErrCRL for distribution points none of which is over HTTP (LDAP,
// which AD CS lists first): the CA revokes, and rat could not tell what.
func crlAddress(cert *x509.Certificate) (string, error) {
	points := cert.CRLDistributionPoints
	for _, point := range points {
		if u, err := url.Parse(point); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			return point, nil
		}
	}
	if len(points) > 0 {
		return "", fmt.Errorf("%w: the certificate names no distribution point over http or https: %q", ErrCRL, points)
	}
	return "", nil
}

// crlRefresh is how long a CRL held is used before fetching it again, in the background: a
// certificate revoked is refused within crlRefresh of the PKI publishing it, and an idle side
// fetches nothing.
const crlRefresh = 15 * time.Minute

// crlRetry is how long after a fetch failing in the background it is tried again, the CRL held
// staying valid meanwhile. Without a valid CRL, every check fetches: a side waking from sleep
// recovers as soon as its network does.
const crlRetry = time.Minute

// crlNow returns the current time without its monotonic reading, for tests to move: delays
// between fetches are told apart by dates. The monotonic clock of Go stops on some systems while
// the machine sleeps, and a laptop asleep for days would count only the minutes it was awake.
var crlNow = func() time.Time { return time.Now().Round(0) }

// CRLEvents are told what happens to the CRL of a side, for its holder to log and act on. They are
// called outside any lock of the side, from the goroutine of the fetch.
type CRLEvents struct {
	// Fetched is told a CRL newer than the one held, or the first: a client closes its idle
	// connections, for the next request to check the server against it.
	Fetched func(crl *CRL)
	// Failed is told a fetch failing or a CRL refused, with the CRL kept, nil when none valid is
	// held: every peer is then refused.
	Failed func(err error, held *CRL)
}

// revocation is the CRL of the CA of a side, fetched from the distribution point of the
// certificate of the side when first needed, again in the background crlRefresh after, and at
// once when the one held is stale.
type revocation struct {
	// address is where the CRL is fetched from: the distribution point of own, never of a peer,
	// which would let a client choose an address for ratd to fetch.
	address string
	// ca is the CA of the side, own its certificate.
	ca, own *x509.Certificate
	// fetch is held while fetching: one fetch at a time, the others waiting for it.
	fetch sync.Mutex
	// mu guards the fields below.
	mu sync.Mutex
	// crl is the last CRL accepted, nil before the first.
	crl *CRL
	// attempted is when the last fetch ended, failed when it failed: the next comes crlRefresh
	// after a success, crlRetry after a failure.
	attempted time.Time
	failed    bool
	// refreshing tells a fetch in the background is running.
	refreshing bool
	// events are told what happens to the CRL.
	events []CRLEvents
}

// OnCRL adds events to be told what happens to the CRL of the side (see CRLEvents), to be set
// before the first check. A side whose certificate names no CRL tells nothing.
func (s *Side) OnCRL(events CRLEvents) {
	if s.revocation == nil {
		return
	}
	s.revocation.mu.Lock()
	defer s.revocation.mu.Unlock()
	s.revocation.events = append(s.revocation.events, events)
}

// CRLURL returns the address the CRL of the CA of the side is fetched from, the first distribution
// point of its certificate over http or https, or "" when it names none: the CA revokes nothing.
func (s *Side) CRLURL() string {
	if s.revocation == nil {
		return ""
	}
	return s.revocation.address
}

// CRL returns the CRL of the CA of the side, valid now: the one held, or fetched from the
// distribution point of the certificate of the side (see CRLURL) when none is held yet or the
// one held is stale, so that a side contacts nothing before needing it. It returns nil when the
// certificate names no distribution point.
//
// The error wraps ErrCRL when no valid CRL can be had, and ErrRevoked when the CRL revokes the
// certificate of the side itself: a side revoked tells it, rather than its peers refusing it. A
// Side built otherwise than by Load or Parse fetches none.
func (s *Side) CRL(ctx context.Context) (*CRL, error) {
	if s.revocation == nil {
		return nil, nil
	}
	return s.revocation.current(ctx)
}

// CheckRevoked returns an error wrapping ErrRevoked when the CRL of the CA of the side (see CRL)
// revokes cert, a certificate of a peer, and the error of CRL when no valid CRL can be had: a side
// whose certificate names a CRL accepts no certificate without it.
//
// A side whose certificate names no CRL refuses a peer whose certificate names one, the error
// wrapping ErrCRL: the CA revokes, which the side can not check, its certificate issued from
// another template than the peer's. The distribution point of the peer is never fetched: a client
// would choose an address for ratd to fetch.
func (s *Side) CheckRevoked(ctx context.Context, cert *x509.Certificate) error {
	if s.revocation == nil && len(cert.CRLDistributionPoints) > 0 {
		own := s.Certificate.Leaf.Subject.CommonName
		return fmt.Errorf("%w: the certificate of %s names a CRL distribution point, the certificate of %s none: "+
			"the CA revokes, which %s can not check", ErrCRL, cert.Subject.CommonName, own, own)
	}
	crl, err := s.CRL(ctx)
	if err != nil || crl == nil {
		return err
	}
	if crl.Revokes(cert) {
		return revokedError(crl, cert)
	}
	return nil
}

// current returns the CRL held, valid now, starting a fetch in the background once due (see
// crlRefresh, crlRetry), or fetching it first when none valid is held, each call fetching then.
func (r *revocation) current(ctx context.Context) (*CRL, error) {
	now := crlNow()
	r.mu.Lock()
	crl := r.crl
	if crl != nil && now.Before(crl.NextUpdate) {
		if !r.refreshing && r.due(now) {
			r.refreshing = true
			// not the context of the call, which ends with it
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), crlTimeout)
				defer cancel()
				_, _ = r.refresh(ctx, false)
			}()
		}
		r.mu.Unlock()
	} else {
		r.mu.Unlock()
		var err error
		if crl, err = r.refresh(ctx, true); err != nil {
			return nil, err
		}
	}
	if crl.Revokes(r.own) {
		return nil, revokedError(crl, r.own)
	}
	return crl, nil
}

// due tells whether a fetch is due at now, the CRL held being valid. A date before the last fetch,
// the clock set back, makes it due: the delay can not be told. Its caller holds mu.
func (r *revocation) due(now time.Time) bool {
	delay := crlRefresh
	if r.failed {
		delay = crlRetry
	}
	return now.Before(r.attempted) || !now.Before(r.attempted.Add(delay))
}

// refresh fetches the CRL, once the fetch in flight, if any, is done, and returns the CRL held,
// valid now. Waiting, it fetches only when the CRL held is still not valid: another call may have
// fetched it meanwhile. A CRL older than the one held is refused: a CRL replayed would un-revoke
// the certificates revoked since.
func (r *revocation) refresh(ctx context.Context, waiting bool) (*CRL, error) {
	r.fetch.Lock()
	defer r.fetch.Unlock()
	r.mu.Lock()
	held := r.crl
	r.mu.Unlock()
	if waiting && held != nil && crlNow().Before(held.NextUpdate) {
		return held, nil
	}
	crl, err := FetchCRL(ctx, r.ca, r.address)
	if err == nil && held != nil && crl.ThisUpdate.Before(held.ThisUpdate) {
		err = fmt.Errorf("%w: fetched from %s: issued on %s, before the CRL held, issued on %s", ErrCRL, r.address,
			crl.ThisUpdate.Format(time.RFC3339), held.ThisUpdate.Format(time.RFC3339))
	}
	now := crlNow()
	r.mu.Lock()
	r.attempted, r.failed, r.refreshing = now, err != nil, false
	newer := err == nil && (held == nil || crl.ThisUpdate.After(held.ThisUpdate))
	if err == nil {
		r.crl = crl
	}
	events := slices.Clone(r.events)
	r.mu.Unlock()
	if err != nil {
		if held != nil && !now.Before(held.NextUpdate) {
			held = nil
		}
		for _, e := range events {
			if e.Failed != nil {
				e.Failed(err, held)
			}
		}
		if held == nil {
			return nil, err
		}
		return held, nil
	}
	if newer {
		for _, e := range events {
			if e.Fetched != nil {
				e.Fetched(crl)
			}
		}
	}
	return crl, nil
}

// revokedError returns the error telling crl revokes cert.
func revokedError(crl *CRL, cert *x509.Certificate) error {
	return fmt.Errorf("%w: the CRL of the CA, fetched from %s, revokes the certificate of %s (serial %s)",
		ErrRevoked, crl.Address, cert.Subject.CommonName, cert.SerialNumber)
}

// CRL is the revocation list of a CA, checked.
type CRL struct {
	// Address is where it was fetched from.
	Address string
	// ThisUpdate is when the CA issued it, NextUpdate when it goes stale.
	ThisUpdate, NextUpdate time.Time
	// revoked holds the serial numbers revoked, in decimal.
	revoked map[string]bool
}

// Revokes reports whether the CRL revokes cert, a certificate of its CA.
func (c *CRL) Revokes(cert *x509.Certificate) bool {
	return c.revoked[cert.SerialNumber.String()]
}

// Len returns the number of certificates the CRL revokes.
func (c *CRL) Len() int {
	return len(c.revoked)
}

// FetchCRL fetches the CRL of ca from address, over http or https, and checks it at the current
// time: DER or PEM, at most 8 MiB, signed by ca, carrying no critical extension Go does not read
// (a partial, delta or indirect CRL), with a next update that has not passed. Plain http is
// enough: the CRL is signed. The error wraps ErrCRL.
func FetchCRL(ctx context.Context, ca *x509.Certificate, address string) (*CRL, error) {
	data, err := fetch(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("%w: fetching %s: %w", ErrCRL, address, err)
	}
	crl, err := parseCRL(data, ca, crlNow())
	if err != nil {
		return nil, fmt.Errorf("%w: fetched from %s: %w", ErrCRL, address, err)
	}
	crl.Address = address
	return crl, nil
}

// fetch returns the body of a GET of address, refused beyond maxCRLSize.
func fetch(ctx context.Context, address string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	resp, err := crlClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCRLSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCRLSize {
		return nil, fmt.Errorf("over %d MiB", maxCRLSize>>20)
	}
	return data, nil
}

// parseCRL returns the CRL of ca in data, DER or PEM, checked at now.
func parseCRL(data []byte, ca *x509.Certificate, now time.Time) (*CRL, error) {
	der := data
	if block, _ := pem.Decode(data); block != nil {
		if block.Type != "X509 CRL" {
			return nil, fmt.Errorf("a PEM %s, not a PEM X509 CRL", block.Type)
		}
		der = block.Bytes
	}
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		return nil, err
	}
	return checkCRL(list, ca, now)
}

// checkCRL checks list, the CRL of ca, at now, and returns it.
func checkCRL(list *x509.RevocationList, ca *x509.Certificate, now time.Time) (*CRL, error) {
	// CheckSignatureFrom checks the signature, not the issuer the CRL names: a CA of another name
	// sharing the key of this one is not this CA.
	if !bytes.Equal(list.RawIssuer, ca.RawSubject) {
		return nil, errors.New("not issued by the CA of ca.crt")
	}
	if err := list.CheckSignatureFrom(ca); err != nil {
		return nil, fmt.Errorf("not signed by the CA of ca.crt: %w", err)
	}
	for _, ext := range list.Extensions {
		if ext.Critical && !containsOID(criticalCRLExtensions, ext.Id) {
			return nil, fmt.Errorf("critical extension %s not supported: a partial, delta or indirect CRL", ext.Id)
		}
	}
	revoked := make(map[string]bool, len(list.RevokedCertificateEntries))
	for _, entry := range list.RevokedCertificateEntries {
		for _, ext := range entry.Extensions {
			if ext.Critical && !containsOID(criticalEntryExtensions, ext.Id) {
				return nil, fmt.Errorf("critical extension %s of an entry not supported: an indirect CRL", ext.Id)
			}
		}
		revoked[entry.SerialNumber.String()] = true
	}
	// Without a next update, a CRL never goes stale: one replayed years later would look current.
	if list.NextUpdate.IsZero() {
		return nil, errors.New("no next update")
	}
	if now.After(list.NextUpdate) {
		return nil, fmt.Errorf("stale since %s", list.NextUpdate.Format(time.RFC3339))
	}
	return &CRL{ThisUpdate: list.ThisUpdate, NextUpdate: list.NextUpdate, revoked: revoked}, nil
}

func containsOID(oids []asn1.ObjectIdentifier, oid asn1.ObjectIdentifier) bool {
	for _, o := range oids {
		if o.Equal(oid) {
			return true
		}
	}
	return false
}
