//go:build linux && dqlite

package cluster

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	"github.com/canonical/go-dqlite/v3/driver"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

func TestNativeQuorumSnapshotJoinsActualSQLSocketClose(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	trust, policy, fixtures := authorizedDatabaseFixtures(t, 3)
	dbs := make([]*Database, len(fixtures))
	for i := range fixtures {
		if i > 0 {
			fixtures[i].config.Seeds = []string{fixtures[0].config.Address}
		}
		if err := identity.PublishTransportPolicy(fixtures[i].policyPath, 0, policy); err != nil {
			t.Fatal(err)
		}
		dbs[i] = openAuthorizedFixture(t, ctx, trust, fixtures[i])
	}
	conversionWait(t, ctx, "socket ACK actual three voters", func() bool {
		members, err := dbs[0].Members(ctx)
		return err == nil && len(members) == 3 && members[0].Role == "voter" && members[1].Role == "voter" && members[2].Role == "voter"
	})
	proof, err := dbs[0].QuorumProof(ctx)
	if err != nil {
		t.Fatal(err)
	}
	leaderIndex := -1
	for i, db := range dbs {
		if db.ID == proof.LeaderID {
			leaderIndex = i
		}
	}
	if leaderIndex < 0 {
		t.Fatal("actual proof leader not present")
	}
	leader := dbs[leaderIndex]
	materialQuorumSeedSQL(t, ctx, leader.SQL, fixtures)
	originalSQL := materialQuorumSQLSnapshot(t, ctx, leader.SQL)
	actualDial := materialQuorumTLS(t, trust, policy, fixtures[leaderIndex])
	var holdClose atomic.Bool
	releaseACK := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseACK) })
	closeEntered := make(chan *nativeQuorumSQLCloseACK, 1)
	var connectionsMu sync.Mutex
	var connections []*nativeQuorumSQLCloseACK
	seeds := client.NewInmemNodeStore()
	if err := seeds.Set(ctx, []client.NodeInfo{{ID: leader.ID, Address: leader.Address}}); err != nil {
		t.Fatal(err)
	}
	localDriver, err := driver.New(seeds, driver.WithDialFunc(func(ctx context.Context, address string) (net.Conn, error) {
		if address != leader.Address {
			return nil, fmt.Errorf("snapshot SQL refuses remote endpoint %q", address)
		}
		return dialQuorumSQLSocket(ctx, address, func(ctx context.Context, address string) (net.Conn, error) {
			conn, err := actualDial(ctx, address)
			if err != nil {
				return nil, err
			}
			actualTLS, ok := conn.(*tls.Conn)
			if !ok {
				return nil, errors.Join(errors.New("snapshot dial did not return actual TLS"), conn.Close())
			}
			observed := &nativeQuorumSQLCloseACK{Conn: actualTLS, hold: &holdClose, release: releaseACK, entered: closeEntered}
			connectionsMu.Lock()
			connections = append(connections, observed)
			connectionsMu.Unlock()
			return observed, nil
		})
	}))
	if err != nil {
		t.Fatal(err)
	}
	// This helper owns only the same production proof/snapshot lifetime. The
	// actual three native nodes remain separately owned by their real Database.
	owner := &Database{ID: leader.ID, Address: leader.Address, leader: leader.Leader}
	bindDatabaseQuorumReader(owner, actualDial, func(ctx context.Context) (*sql.DB, func() error, error) {
		connector, err := localDriver.OpenConnector(fixtures[leaderIndex].config.Name)
		if err != nil {
			return nil, nil, err
		}
		return openQuorumSQL(ctx, connector)
	})
	t.Cleanup(owner.closeQuorum)
	readComplete := make(chan error, 1)
	returned := make(chan error, 1)
	returnedProof := make(chan QuorumProof, 1)
	ownerClosed := make(chan struct{})
	go func() {
		proof, err := owner.WithLocalQuorumReadSnapshot(ctx, func(readCtx context.Context, tx *sql.Tx, _ QuorumProof) error {
			err := nativeSnapshotReadRows(readCtx, tx)
			if err == nil {
				holdClose.Store(true)
			}
			readComplete <- err
			if err != nil {
				return err
			}
			<-readCtx.Done()
			return readCtx.Err()
		})
		returnedProof <- proof
		returned <- err
	}()
	select {
	case err := <-readComplete:
		if err != nil {
			t.Fatal("actual driver SQL SELECT before socket hold", err)
		}
	case <-ctx.Done():
		t.Fatal("actual SDK connector never completed the local read", ctx.Err())
	}
	go func() { owner.closeQuorum(); close(ownerClosed) }()
	var held *nativeQuorumSQLCloseACK
	select {
	case held = <-closeEntered:
	case <-ctx.Done():
		t.Fatal("cancel never reached original SQL TLS Close", ctx.Err())
	}
	if _, err := held.Conn.NetConn().Write([]byte{0}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("original SQL TCP socket not actually closed before held ACK: %v", err)
	}
	select {
	case err := <-returned:
		t.Fatalf("snapshot returned before original socket Close ACK: %v", err)
	default:
	}
	select {
	case <-ownerClosed:
		t.Fatal("proof owner retired before original socket Close ACK")
	default:
	}
	releaseOnce.Do(func() { close(releaseACK) })
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("actual SQL cancellation cause lost after ACK: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("snapshot failed to join original Close ACK", ctx.Err())
	}
	if proof := <-returnedProof; !reflect.DeepEqual(proof, QuorumProof{}) {
		t.Fatalf("closed SQL owner exposed proof: %+v", proof)
	}
	select {
	case <-ownerClosed:
	case <-ctx.Done():
		t.Fatal("proof owner failed to join snapshot/socket ACK", ctx.Err())
	}
	connectionsMu.Lock()
	captured := append([]*nativeQuorumSQLCloseACK(nil), connections...)
	connectionsMu.Unlock()
	for _, conn := range captured {
		conn.mu.Lock()
		closeErr := conn.closeErr
		conn.mu.Unlock()
		if conn.closes.Load() != 1 || closeErr != nil {
			t.Fatalf("original SQL TLS Close count=%d error=%v, want one confirmed successful Close", conn.closes.Load(), closeErr)
		}
	}
	if got := materialQuorumSQLSnapshot(t, ctx, leader.SQL); !reflect.DeepEqual(got, originalSQL) {
		t.Fatal("original SQL socket cancellation changed representative SQL state")
	}
	t.Logf("ACTUAL_SDK_SQL_CLOSE_ACK original_connections=%d real_tcp_closed=true snapshot_and_helper_owner_waited=true each_close_once=true actual_close_error=nil zero_proof=true", len(captured))
}

// These facts retain the actual SDK TLS socket and its Close return boundary;
// database/sql's public transaction state cannot reveal the original socket ACK.
type nativeQuorumSQLCloseACK struct {
	*tls.Conn
	hold     *atomic.Bool
	release  <-chan struct{}
	entered  chan<- *nativeQuorumSQLCloseACK
	closes   atomic.Int64
	mu       sync.Mutex
	closeErr error
}

func (c *nativeQuorumSQLCloseACK) Close() error {
	c.closes.Add(1)
	err := c.Conn.Close()
	c.mu.Lock()
	c.closeErr = err
	c.mu.Unlock()
	if c.hold.Load() {
		select {
		case c.entered <- c:
		default:
		}
		<-c.release
	}
	return err
}
