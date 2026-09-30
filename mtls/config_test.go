package mtls

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// testCA returns a new CA for tenant, valid for the test.
func testCA(t *testing.T, tenant string) credential {
	t.Helper()
	ca, err := newCA(tenant, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// testSide returns the side of role carrying name, issued by ca, valid for the test or expired,
// built in memory as Load builds it.
func testSide(t *testing.T, ca credential, name string, role Role, valid bool) *Side {
	t.Helper()
	notBefore, notAfter := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	if !valid {
		notBefore, notAfter = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
	}
	c, err := issue(ca, name, role, notBefore, notAfter)
	if err != nil {
		t.Fatal(err)
	}
	return &Side{Role: role, Name: name, Tenant: ca.cert.Subject.CommonName, CA: ca.cert,
		Certificate: tls.Certificate{Certificate: [][]byte{c.cert.Raw}, PrivateKey: c.key, Leaf: c.cert}}
}

// connection is what each end of a TLS connection got.
type connection struct {
	client     error  // the handshake of the client
	clientRead error  // the first read of the client, after its handshake succeeded
	clientSaw  string // the name of the server, for the client
	server     error  // the handshake of the server
	serverSaw  string // the name of the client, for the server
}

// connect runs a TLS connection over loopback TCP between a server, which writes "ok" once its
// handshake succeeds, and a client, which reads it once its own handshake succeeds.
func connect(t *testing.T, server, client *tls.Config) (c connection) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	served := make(chan struct{})
	go func() {
		defer close(served)
		conn, err := listener.Accept()
		if err != nil {
			c.server = err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		tlsConn := tls.Server(conn, server)
		if c.server = tlsConn.Handshake(); c.server == nil {
			c.serverSaw = PeerName(tlsConn.ConnectionState())
			_, _ = tlsConn.Write([]byte("ok"))
		}
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	tlsConn := tls.Client(conn, client)
	if c.client = tlsConn.Handshake(); c.client == nil {
		c.clientSaw = PeerName(tlsConn.ConnectionState())
		_, c.clientRead = io.ReadFull(tlsConn, make([]byte, 2))
	}
	_ = conn.Close()
	<-served
	return c
}

// TestConfigs guards the TLS contract: the server and a client of a bundle connect, the client not
// checking the host name, and each end refuses a peer from another bundle, of the wrong role, or
// expired. A client refused by the server completes its own handshake and learns it on its first
// read: in TLS 1.3, the server checks the client certificate after the client finished.
func TestConfigs(t *testing.T) {
	ca := testCA(t, "t")
	server := testSide(t, ca, "t", Server, true)
	alice := testSide(t, ca, "alice", Client, true)
	serverConfig := func(side *Side) *tls.Config {
		t.Helper()
		config, err := ServerConfig(side)
		if err != nil {
			t.Fatal(err)
		}
		return config
	}
	clientConfig := func(side *Side) *tls.Config {
		t.Helper()
		config, err := ClientConfig(side)
		if err != nil {
			t.Fatal(err)
		}
		return config
	}
	// a side holding the certificate of another, for files mixed by hand
	holding := func(side *Side, role Role, other *Side) *Side {
		mixed := *side
		mixed.Role, mixed.Certificate = role, other.Certificate
		return &mixed
	}

	t.Run("same bundle", func(t *testing.T) {
		client := clientConfig(alice)
		// as http.Transport sets it from the address dialed: not checked
		client.ServerName = "elsewhere.example"
		c := connect(t, serverConfig(server), client)
		if c.client != nil || c.clientRead != nil || c.server != nil {
			t.Fatalf("expected a connection, got %+v", c)
		}
		if c.serverSaw != "alice" || c.clientSaw != "t" {
			t.Errorf("expected the server to see alice and the client t, got %q and %q", c.serverSaw, c.clientSaw)
		}
	})

	t.Run("TLS 1.2", func(t *testing.T) {
		client := clientConfig(alice)
		client.MinVersion, client.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
		if c := connect(t, serverConfig(server), client); c.client == nil || c.server == nil {
			t.Errorf("expected TLS 1.2 to be refused, got %+v", c)
		}
	})

	// the client trusts the server, which refuses its certificate with an alert telling why
	for _, tc := range []struct {
		name   string
		client *Side
		alert  string
	}{
		// same tenant, hence same CA subject: the client presents its certificate
		{"client of another bundle", holding(alice, Client, testSide(t, testCA(t, "t"), "alice", Client, true)),
			"unknown certificate authority"},
		// another CA subject: the client presents no certificate, as the server asks for the CA
		// of its bundle
		{"client of another tenant", holding(alice, Client, testSide(t, testCA(t, "u"), "alice", Client, true)),
			"certificate required"},
		{"server certificate as a client", holding(alice, Client, server), "bad certificate"},
		{"expired client", testSide(t, ca, "alice", Client, false), "expired certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := connect(t, serverConfig(server), clientConfig(tc.client))
			if c.server == nil {
				t.Fatalf("expected the server to refuse the client, got %+v", c)
			}
			if c.client != nil || c.clientRead == nil || !strings.Contains(c.clientRead.Error(), tc.alert) {
				t.Errorf("expected the client to complete its handshake and read the alert %q, got %+v", tc.alert, c)
			}
		})
	}

	// the client refuses the server, telling why
	for _, tc := range []struct {
		name     string
		server   *Side
		expected error
	}{
		{"server of another bundle", testSide(t, testCA(t, "t"), "t", Server, true), ErrNotSignedByCA},
		{"client certificate as a server", holding(server, Server, alice), ErrRole},
		{"expired server", testSide(t, ca, "t", Server, false), ErrValidity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c := connect(t, serverConfig(tc.server), clientConfig(alice)); !errors.Is(c.client, tc.expected) {
				t.Errorf("expected the client handshake to fail with %v, got %+v", tc.expected, c)
			}
		})
	}

	t.Run("wrong side", func(t *testing.T) {
		if _, err := ServerConfig(alice); !errors.Is(err, ErrRole) {
			t.Errorf("server configuration of a client: expected ErrRole, got %v", err)
		}
		if _, err := ClientConfig(server); !errors.Is(err, ErrRole) {
			t.Errorf("client configuration of the server: expected ErrRole, got %v", err)
		}
	})
}
