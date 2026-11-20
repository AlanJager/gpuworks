//go:build linux && dqlite

package cluster

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRemoteExistingReadSQLRejectsInvalidSetupBeforeDial(t *testing.T) {
	requireDatabaseHelper(t)
	valid := Member{ID: 1, Address: "127.0.0.1:9001", Role: "voter"}
	type invalidSetup struct {
		name    string
		member  Member
		db      string
		nilDial bool
	}
	cases := []invalidSetup{
		{name: "zero ID", member: Member{Address: valid.Address, Role: valid.Role}, db: "cluster.db"},
		{name: "empty endpoint", member: Member{ID: 1, Role: valid.Role}, db: "cluster.db"},
		{name: "missing port", member: Member{ID: 1, Address: "127.0.0.1", Role: valid.Role}, db: "cluster.db"},
		{name: "noncanonical endpoint", member: Member{ID: 1, Address: "127.0.0.1:09001", Role: valid.Role}, db: "cluster.db"},
		{name: "standby", member: Member{ID: 1, Address: valid.Address, Role: "standby"}, db: "cluster.db"},
		{name: "spare", member: Member{ID: 1, Address: valid.Address, Role: "spare"}, db: "cluster.db"},
		{name: "missing role", member: Member{ID: 1, Address: valid.Address}, db: "cluster.db"},
		{name: "nil authenticated dialer", member: valid, db: "cluster.db", nilDial: true},
	}
	for _, name := range []string{"", ".", "..", "../cluster.db", `dir\cluster.db`, "file:cluster.db", "cluster.db?mode=ro", "zero\x00.db", "line\n.db", "delete\x7f.db", string([]byte{0xff}), strings.Repeat("a", 512)} {
		cases = append(cases, invalidSetup{name: fmt.Sprintf("invalid filename %q", name), member: valid, db: name})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			dial := func(context.Context, string) (net.Conn, error) {
				calls.Add(1)
				return nil, errors.New("invalid setup reached network")
			}
			if tc.nilDial {
				dial = nil
			}
			pool, cleanup, err := OpenRemoteExistingReadSQL(t.Context(), tc.member, tc.db, dial)
			if err == nil || pool != nil || cleanup != nil || calls.Load() != 0 {
				if cleanup != nil {
					_ = cleanup()
				}
				t.Fatalf("invalid setup escaped: pool=%v cleanup=%t err=%v dials=%d", pool, cleanup != nil, err, calls.Load())
			}
		})
	}
}

func TestRemoteExistingReadSQLCanceledSetupReturnsNoResources(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var calls atomic.Int32
	pool, cleanup, err := OpenRemoteExistingReadSQL(ctx, Member{ID: 1, Address: "127.0.0.1:9001", Role: "voter"}, "cluster.db", func(context.Context, string) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New("canceled setup reached network")
	})
	if !errors.Is(err, context.Canceled) || pool != nil || cleanup != nil || calls.Load() != 0 {
		t.Fatalf("canceled setup escaped: pool=%v cleanup=%t err=%v dials=%d", pool, cleanup != nil, err, calls.Load())
	}
}

func TestRemoteExistingReadSQLLazyPoolCleanupPreventsFutureDial(t *testing.T) {
	requireDatabaseHelper(t)
	var calls atomic.Int32
	pool, cleanup, err := OpenRemoteExistingReadSQL(t.Context(), Member{ID: 1, Address: "127.0.0.1:9001", Role: "voter"}, "material public.db", func(context.Context, string) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New("closed pool reached network")
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	if calls.Load() != 0 || pool.Stats().OpenConnections != 0 {
		t.Fatal("creating a lazy pool touched the network")
	}
	for range 2 {
		if err := cleanup(); err != nil {
			t.Fatal("lazy pool cleanup failed", err)
		}
	}
	if err := pool.PingContext(t.Context()); err == nil || calls.Load() != 0 || pool.Stats().OpenConnections != 0 {
		t.Fatalf("closed pool admitted work: err=%v dials=%d stats=%+v", err, calls.Load(), pool.Stats())
	}
}

func TestRemoteExistingReadSQLRefusesLearnedLeaderEndpoint(t *testing.T) {
	requireDatabaseHelper(t)
	const authorized = "127.0.0.1:9001"
	const redirected = "127.0.0.1:9002"
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	fixtureDone := make(chan error, 1)
	go func() {
		fixtureDone <- remoteExistingSQLRedirectFixture(peer, redirected)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	var original, foreign atomic.Int32
	pool, cleanup, err := OpenRemoteExistingReadSQL(ctx, Member{ID: 1, Address: authorized, Role: "voter"}, "cluster.db", func(_ context.Context, address string) (net.Conn, error) {
		if address != authorized {
			foreign.Add(1)
			return nil, errors.New("learned endpoint reached pinned dialer")
		}
		if original.Add(1) == 1 {
			return client, nil
		}
		return nil, errors.New("fixture has one authorized connection")
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	if err := pool.PingContext(ctx); err == nil {
		t.Fatal("redirected endpoint produced a usable SQL connection")
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-fixtureDone:
		if err != nil {
			t.Fatal("precise LEADER fixture failed", err)
		}
	case <-t.Context().Done():
		t.Fatal("authorized physical socket did not close")
	}
	if original.Load() < 1 || foreign.Load() != 0 || pool.Stats().OpenConnections != 0 {
		t.Fatalf("endpoint confinement failed: original=%d foreign=%d stats=%+v", original.Load(), foreign.Load(), pool.Stats())
	}
}

// The fixture accepts exactly protocol-v1's handshake and LEADER request. It
// advertises another voter, then requires actual EOF without CLIENT or OPEN.
func remoteExistingSQLRedirectFixture(peer net.Conn, redirected string) error {
	var request [24]byte
	if _, err := io.ReadFull(peer, request[:]); err != nil {
		return err
	}
	var expected [24]byte
	binary.LittleEndian.PutUint64(expected[:8], 1)
	binary.LittleEndian.PutUint32(expected[8:12], 1)
	if request != expected {
		return fmt.Errorf("unexpected handshake/LEADER bytes %x", request)
	}
	body := make([]byte, 8+((len(redirected)+8)&^7))
	binary.LittleEndian.PutUint64(body[:8], 2)
	copy(body[8:], redirected)
	response := make([]byte, 8+len(body))
	binary.LittleEndian.PutUint32(response[:4], uint32(len(body)/8))
	response[4] = 1 // Protocol-v1 NODE: voter ID and padded address.
	copy(response[8:], body)
	if n, err := peer.Write(response); err != nil {
		return err
	} else if n != len(response) {
		return io.ErrShortWrite
	}
	var extra [1]byte
	if n, err := peer.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		return fmt.Errorf("original endpoint received further bytes or stayed open: n=%d err=%v", n, err)
	}
	return nil
}
