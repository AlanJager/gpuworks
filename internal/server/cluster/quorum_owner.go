package cluster

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"sync"
	"time"
)

// QuorumProof uses the live leader only to locate a dedicated fresh barrier
// connection. Cached membership and successful leader discovery are not proof.
func (d *Database) QuorumProof(ctx context.Context) (QuorumProof, error) {
	if err := ctx.Err(); err != nil {
		return QuorumProof{}, err
	}
	if d == nil || d.quorum == nil {
		return QuorumProof{}, ErrUnavailable
	}
	if d.closed.Load() {
		return QuorumProof{}, errors.New("database is closed")
	}
	proof, err := d.quorum(ctx)
	if d.closed.Load() {
		err = errors.Join(err, errors.New("database is closed"))
	}
	err = errors.Join(err, ctx.Err())
	if err != nil {
		return QuorumProof{}, err
	}
	return proof, nil
}

// The native owner outlives the construction context. Its separate proof
// lifetime closes admissions and drains actual Go calls before pools/node stop.
// Closing a remote connection does not assert that its native barrier drained.
func bindDatabaseQuorumReader(d *Database, dial func(context.Context, string) (net.Conn, error), openSQL ...func(context.Context) (*sql.DB, func() error, error)) {
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	var mu sync.Mutex
	var active sync.WaitGroup
	closed := false
	d.closeQuorum = func() {
		mu.Lock()
		closed = true
		cancelLifetime()
		mu.Unlock()
		active.Wait()
	}
	run := func(parent context.Context, operation func(context.Context) (QuorumProof, error)) (proof QuorumProof, result error) {
		mu.Lock()
		if closed || d.closed.Load() {
			mu.Unlock()
			return QuorumProof{}, errors.New("database quorum reader is closed")
		}
		active.Add(1)
		mu.Unlock()
		defer active.Done()
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		stopped := make(chan struct{})
		stop := context.AfterFunc(lifetime, func() { cancel(); close(stopped) })
		defer func() {
			if !stop() {
				<-stopped
			}
			mu.Lock()
			retired := closed || d.closed.Load()
			mu.Unlock()
			result = errors.Join(result, ctx.Err())
			if retired {
				result = errors.Join(result, errors.New("database quorum reader is closed"))
			}
			if result != nil {
				proof = QuorumProof{}
			}
			cancel()
		}()
		return operation(ctx)
	}
	readProof := func(ctx context.Context) (QuorumProof, error) {
		leader, err := d.leader(ctx)
		if err != nil {
			return QuorumProof{}, err
		}
		if err := ctx.Err(); err != nil {
			return QuorumProof{}, err
		}
		proof, err := ReadQuorumProof(ctx, leader.Address, dial)
		if err != nil {
			return QuorumProof{}, err
		}
		if proof.LeaderID != leader.ID {
			return QuorumProof{}, errors.New("native quorum proof leader changed during discovery")
		}
		for _, member := range proof.Members {
			if member.ID == proof.LeaderID && member.Address == leader.Address {
				return proof, nil
			}
		}
		return QuorumProof{}, errors.New("native quorum proof leader address changed during discovery")
	}
	d.quorum = func(ctx context.Context) (QuorumProof, error) { return run(ctx, readProof) }
	d.quorumSnapshot = func(ctx context.Context, read func(context.Context, *sql.Tx, QuorumProof) error) (QuorumProof, error) {
		if len(openSQL) != 1 || openSQL[0] == nil {
			return QuorumProof{}, ErrUnavailable
		}
		return run(ctx, func(ctx context.Context) (QuorumProof, error) {
			return readLocalQuorumSnapshot(ctx, d, readProof, openSQL[0], read)
		})
	}
}
