//go:build linux && dqlite

package cluster

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

func TestProbeMemberAuthenticatesPendingMemberWithoutChangingRaft(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	trust, policy, members := authorizedDatabaseFixtures(t, 2)
	server, pending := members[0], members[1]
	if err := identity.PublishTransportPolicy(server.policyPath, 0, policy); err != nil {
		t.Fatal(err)
	}
	db := openAuthorizedFixture(t, ctx, trust, server)
	before, err := db.Members(ctx)
	if err != nil || len(before) != 1 {
		t.Fatalf("initial membership %#v %v", before, err)
	}
	if err := ProbeMember(ctx, server.config.Address, server.fingerprint, trust, pending.certificate, pending.key, policy); err != nil {
		t.Fatal("pending authorized client could not probe", err)
	}
	after, err := db.Members(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("probe changed membership %#v %v", after, err)
	}
	if err := ProbeMember(ctx, server.config.Address, pending.fingerprint, trust, pending.certificate, pending.key, policy); err == nil {
		t.Fatal("wrong expected endpoint key accepted")
	}
	denied := policy
	denied.AllowedKeys = []string{server.fingerprint}
	if err := ProbeMember(ctx, server.config.Address, server.fingerprint, trust, pending.certificate, pending.key, denied); err == nil {
		t.Fatal("locally unauthorized probe key accepted")
	}
	wrongPolicy := policy
	wrongPolicy.CAFingerprint = strings.Repeat("f", 64)
	if err := ProbeMember(ctx, server.config.Address, server.fingerprint, trust, pending.certificate, pending.key, wrongPolicy); err == nil {
		t.Fatal("policy trust authority mismatch accepted")
	}
	otherTrust, otherPolicy, otherMembers := authorizedDatabaseFixtures(t, 1)
	otherPolicy.AllowedKeys = append(otherPolicy.AllowedKeys, server.fingerprint)
	slices.Sort(otherPolicy.AllowedKeys)
	if err := ProbeMember(ctx, server.config.Address, server.fingerprint, otherTrust, otherMembers[0].certificate, otherMembers[0].key, otherPolicy); err == nil {
		t.Fatal("wrong CA accepted actual server")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	denied.Revision = 2
	denied.RevokedKeys = []string{pending.fingerprint}
	if err := identity.PublishTransportPolicy(server.policyPath, 1, denied); err != nil {
		t.Fatal(err)
	}
	db = openAuthorizedFixture(t, ctx, trust, server)
	// Client trusts its stale allowlist, but the server rejects its identity. In
	// TLS 1.3, a client-side successful handshake alone cannot establish this.
	if err := ProbeMember(ctx, server.config.Address, server.fingerprint, trust, pending.certificate, pending.key, policy); err == nil {
		t.Fatal("server-revoked probe client was accepted")
	}
	after, err = db.Members(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed probe changed membership %#v %v", after, err)
	}
}

func TestProbeMemberRequiresDqliteProtocolAndHonorsCancellation(t *testing.T) {
	requireDatabaseHelper(t)
	trust, policy, members := authorizedDatabaseFixtures(t, 2)
	server, pending := members[0], members[1]
	authorize, err := trust.PolicyAuthorizer(policy)
	if err != nil {
		t.Fatal(err)
	}
	listenTLS, _, err := trust.TLSConfigs(server.certificate, server.key, time.Now(), authorize)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"not dqlite", "cancel blocked protocol", "short deadline"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := tls.Listen("tcp", "127.0.0.1:0", listenTLS.Clone())
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			readProtocol := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				// Reading the protocol preface proves a real mutually-authenticated TLS
				// exchange before injecting non-dqlite data or awaiting cancellation.
				header := make([]byte, 8)
				_, err = io.ReadFull(conn, header)
				if err != nil {
					done <- err
					return
				}
				close(readProtocol)
				if mode == "not dqlite" {
					_, err = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
					done <- err
					return
				}
				_, err = io.Copy(io.Discard, conn)
				done <- err
			}()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var deadlineCancel context.CancelFunc
			if mode == "short deadline" {
				ctx, deadlineCancel = context.WithTimeout(ctx, 150*time.Millisecond)
				defer deadlineCancel()
			}
			result := make(chan error, 1)
			go func() {
				result <- ProbeMember(ctx, listener.Addr().String(), server.fingerprint, trust, pending.certificate, pending.key, policy)
			}()
			select {
			case <-readProtocol:
			case <-time.After(3 * time.Second):
				t.Fatal("probe did not reach authenticated protocol exchange")
			}
			if mode == "cancel blocked protocol" {
				cancel()
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("non-dqlite or canceled endpoint reported success")
				}
				if mode == "cancel blocked protocol" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
				if mode == "short deadline" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("short deadline lost: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("probe ignored bounded caller lifetime")
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("probe left endpoint connection open")
			}
		})
	}
}

func TestProbeMemberRejectsNoncanonicalConfiguration(t *testing.T) {
	requireDatabaseHelper(t)
	trust, policy, members := authorizedDatabaseFixtures(t, 1)
	member := members[0]
	for _, address := range []string{"", "0.0.0.0:9000", "[::]:9000", "224.0.0.1:9000", "127.0.0.1:09000", "HOST.example:9000", "host.example.:9000", "bad_host:9000", "[0:0:0:0:0:0:0:1]:9000"} {
		if err := ProbeMember(t.Context(), address, member.fingerprint, trust, member.certificate, member.key, policy); err == nil {
			t.Fatalf("invalid address accepted: %q", address)
		}
	}
	if err := ProbeMember(t.Context(), member.config.Address, "invalid", trust, member.certificate, member.key, policy); err == nil {
		t.Fatal("invalid expected key accepted")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ProbeMember(canceled, member.config.Address, member.fingerprint, trust, member.certificate, member.key, policy); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled probe %v", err)
	}
	if err := ProbeMember(t.Context(), member.config.Address, member.fingerprint, nil, member.certificate, member.key, policy); err == nil {
		t.Fatal("nil trust accepted")
	}
	// Host normalization is tested without dialing or binding an endpoint.
	for _, address := range []string{"127.0.0.1:9000", "[::1]:9000", "node.example:9000"} {
		if err := probeMemberAddress(address); err != nil {
			t.Fatalf("canonical address %s rejected: %v", address, err)
		}
	}
}

func TestProbeReplyConnRefusesAnOversizedReplyBeforeItsBody(t *testing.T) {
	for name, test := range map[string]struct {
		stream  []byte
		refused bool
	}{
		"http reply":       {[]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"), true},
		"over the limit":   {append([]byte{0x01, 0x20, 0, 0, 1, 0, 0, 0}, make([]byte, 16)...), true},
		"two small frames": {append(append([]byte{2, 0, 0, 0, 1, 0, 0, 0}, make([]byte, 16)...), append([]byte{1, 0, 0, 0, 1, 0, 0, 0}, make([]byte, 8)...)...), false},
		"empty body":       {[]byte{0, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 9, 9, 9, 9, 9, 9, 9, 9}, false},
	} {
		t.Run(name, func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			go func() {
				// One byte at a time: a header may arrive in any pieces.
				for _, b := range test.stream {
					if _, err := right.Write([]byte{b}); err != nil {
						return
					}
				}
				right.Close()
			}()
			conn := &probeReplyConn{Conn: left}
			got, err := io.ReadAll(conn)
			if test.refused {
				if err == nil || !strings.Contains(err.Error(), "does not speak the database protocol") {
					t.Fatalf("err=%v", err)
				}
				if len(got) > 8 {
					t.Fatalf("%d bytes of an oversized reply were delivered", len(got))
				}
				return
			}
			if err != nil || !bytes.Equal(got, test.stream) {
				t.Fatalf("stream changed: %d of %d bytes, err=%v", len(got), len(test.stream), err)
			}
		})
	}
}
