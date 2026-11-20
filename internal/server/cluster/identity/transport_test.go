package identity

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// exchangeTLS requires application bytes in both directions: a TLS 1.3 client
// can finish its handshake before learning that its certificate was rejected.
func exchangeTLS(t *testing.T, serverConfig, clientConfig *tls.Config) (clientErr, serverErr error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer raw.Close()
		deadline, _ := ctx.Deadline()
		_ = raw.SetDeadline(deadline)
		connection := tls.Server(raw, serverConfig)
		if err := connection.HandshakeContext(ctx); err != nil {
			done <- err
			return
		}
		var request [1]byte
		if _, err := io.ReadFull(connection, request[:]); err != nil {
			done <- err
			return
		}
		if request[0] != 41 {
			done <- errors.New("unexpected authenticated payload")
			return
		}
		_, err = connection.Write([]byte{42})
		done <- err
	}()
	dialer := tls.Dialer{Config: clientConfig}
	connection, clientErr := dialer.DialContext(ctx, "tcp", listener.Addr().String())
	if clientErr == nil {
		deadline, _ := ctx.Deadline()
		_ = connection.SetDeadline(deadline)
		_, clientErr = connection.Write([]byte{41})
		if clientErr == nil {
			var reply [1]byte
			_, clientErr = io.ReadFull(connection, reply[:])
			if clientErr == nil && reply[0] != 42 {
				clientErr = errors.New("incorrect authenticated response")
			}
		}
		_ = connection.Close()
	}
	return clientErr, <-done
}

func TestMutualTLSIndependentMemberKeysAndAuthorization(t *testing.T) {
	f := newIdentityFixture(t)
	serverCert, serverKey, serverFP := f.member(t, "127.0.0.1:9010")
	clientCert, clientKey, clientFP := f.member(t, "127.0.0.1:9011")
	permitOnly := func(want string) PeerAuthorizer {
		return func(got string) error {
			if got != want {
				return errors.New("peer is not locally admitted")
			}
			return nil
		}
	}
	server, _, err := f.trust.TLSConfigs(serverCert, serverKey, f.now, permitOnly(clientFP))
	if err != nil {
		t.Fatal(err)
	}
	_, client, err := f.trust.TLSConfigs(clientCert, clientKey, f.now, permitOnly(serverFP))
	if err != nil {
		t.Fatal(err)
	}
	if client.ServerName != "" || client.InsecureSkipVerify || server.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatal("transport bypasses endpoint or mutual authentication")
	}
	if c, s := exchangeTLS(t, server, client); c != nil || s != nil {
		t.Fatalf("mutual exchange failed: client=%v server=%v", c, s)
	}
	// Reverse roles using the same independent identities and admission policy.
	clientListen, _, err := f.trust.TLSConfigs(clientCert, clientKey, f.now, permitOnly(serverFP))
	if err != nil {
		t.Fatal(err)
	}
	_, serverDial, err := f.trust.TLSConfigs(serverCert, serverKey, f.now, permitOnly(clientFP))
	if err != nil {
		t.Fatal(err)
	}
	if c, s := exchangeTLS(t, clientListen, serverDial); c != nil || s != nil {
		t.Fatalf("reverse mutual exchange failed: client=%v server=%v", c, s)
	}
	reject := func(string) error { return errors.New("member revoked in local authorization snapshot") }
	t.Run("server rejects authenticated member", func(t *testing.T) {
		refusing, _, err := f.trust.TLSConfigs(serverCert, serverKey, f.now, reject)
		if err != nil {
			t.Fatal(err)
		}
		if c, s := exchangeTLS(t, refusing, client); c == nil || s == nil {
			t.Fatalf("revoked client exchanged data: client=%v server=%v", c, s)
		}
	})
	t.Run("client rejects authenticated server", func(t *testing.T) {
		_, refusing, err := f.trust.TLSConfigs(clientCert, clientKey, f.now, reject)
		if err != nil {
			t.Fatal(err)
		}
		if c, _ := exchangeTLS(t, server, refusing); c == nil {
			t.Fatal("revoked server accepted")
		}
	})
}

func TestMutualTLSRejectsInvalidPeerMaterial(t *testing.T) {
	f := newIdentityFixture(t)
	serverCert, serverKey, _ := f.member(t, "127.0.0.1:9010")
	clientCert, clientKey, _ := f.member(t, "127.0.0.1:9011")
	// Here all correctly authenticated members of the pinned CA are admitted;
	// failure cases isolate certificate validation from authorization policy.
	permit := func(string) error { return nil }
	server, _, err := f.trust.TLSConfigs(serverCert, serverKey, f.now, permit)
	if err != nil {
		t.Fatal(err)
	}
	_, client, err := f.trust.TLSConfigs(clientCert, clientKey, f.now, permit)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("no client certificate", func(t *testing.T) {
		missing := client.Clone()
		missing.Certificates = nil
		if c, s := exchangeTLS(t, server, missing); c == nil || s == nil {
			t.Fatalf("anonymous exchange accepted: client=%v server=%v", c, s)
		}
	})
	t.Run("wrong CA", func(t *testing.T) {
		other := newIdentityFixture(t)
		cert, key, _ := other.member(t, "127.0.0.1:9012")
		_, foreign, err := other.trust.TLSConfigs(cert, key, other.now, permit)
		if err != nil {
			t.Fatal(err)
		}
		if c, _ := exchangeTLS(t, server, foreign); c == nil {
			t.Fatal("foreign CA server accepted")
		}
		// Trust the server CA, but force presentation of the foreign client cert.
		foreign.RootCAs = client.RootCAs
		foreign.VerifyConnection = client.VerifyConnection
		foreign.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &foreign.Certificates[0], nil }
		if c, s := exchangeTLS(t, server, foreign); c == nil || s == nil {
			t.Fatalf("foreign client accepted: client=%v server=%v", c, s)
		}
	})
	t.Run("wrong endpoint SAN", func(t *testing.T) {
		cert, key, _ := f.member(t, "different.example:9010")
		wrong, _, err := f.trust.TLSConfigs(cert, key, f.now, permit)
		if err != nil {
			t.Fatal(err)
		}
		if c, _ := exchangeTLS(t, wrong, client); c == nil {
			t.Fatal("server with wrong address accepted")
		}
	})
	t.Run("certificate expires after config creation", func(t *testing.T) {
		future := client.Clone()
		future.Time = func() time.Time { return f.now.Add(2 * time.Hour) }
		if c, _ := exchangeTLS(t, server, future); c == nil {
			t.Fatal("expired server accepted during handshake")
		}
		futureServer := server.Clone()
		futureServer.Time = func() time.Time { return f.now.Add(2 * time.Hour) }
		if c, s := exchangeTLS(t, futureServer, client); c == nil || s == nil {
			t.Fatalf("expired client accepted during handshake: client=%v server=%v", c, s)
		}
	})
	for _, tc := range []struct {
		name      string
		cert, key []byte
		policy    PeerAuthorizer
		now       time.Time
	}{
		{"wrong private key", serverCert, clientKey, permit, f.now},
		{"missing private key", serverCert, nil, permit, f.now},
		{"missing authorization", serverCert, serverKey, nil, f.now},
		{"expired local identity", serverCert, serverKey, permit, f.now.Add(2 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := f.trust.TLSConfigs(tc.cert, tc.key, tc.now, tc.policy); err == nil {
				t.Fatal("invalid transport configuration accepted")
			}
		})
	}
}
