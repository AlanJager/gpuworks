//go:build linux && dqlite

package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// ReadQuorumDiscoveryForCluster reads only public dial hints, including on a
// follower. The original session mutex remains held through actual borrowed
// SQL cleanup, so Close cannot release its runtime lease before this returns.
// It neither issues grants nor proves current quorum, closure or a spare role.
func (s *LegacyJoinedRaftSession) ReadQuorumDiscoveryForCluster(parent context.Context, expected ClusterIdentity, requesterKey string) (requester ClusterAdmission, members []ClusterTransportMember, result error) {
	if err := parent.Err(); err != nil {
		return ClusterAdmission{}, nil, err
	}
	if s == nil {
		return ClusterAdmission{}, nil, ErrClusterIdentityConflict
	}
	if !s.mu.TryLock() {
		return ClusterAdmission{}, nil, errors.New("native session is classifying or closing")
	}
	defer s.mu.Unlock()
	db := s.db
	if db == nil || s.release == nil || db.ID != s.id || db.Address != s.address || s.id == 0 {
		return ClusterAdmission{}, nil, errors.New("native session is unavailable")
	}
	return readQuorumDiscovery(parent, db, expected, requesterKey)
}

// readQuorumDiscovery is the discovery read over one database handle: the
// live membership and, in one read-only snapshot, the admission of the
// requester and the members' transport records. An established member's
// session and a member running as a single member both answer from it.
func readQuorumDiscovery(parent context.Context, db *cluster.Database, expected ClusterIdentity, requesterKey string) (requester ClusterAdmission, members []ClusterTransportMember, result error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	defer func() {
		result = errors.Join(result, ctx.Err())
		if result != nil {
			requester, members = ClusterAdmission{}, nil
		}
	}()
	if err := observedClusterIdentity(db, expected); err != nil {
		return ClusterAdmission{}, nil, err
	}
	native, err := db.Members(ctx)
	if err != nil {
		return ClusterAdmission{}, nil, err
	}
	// A borrowed sql.Conn/Tx cannot retry ErrBadConn as sql.DB queries do.
	// The normal pool has at most four connections that may still target the
	// former leader. Each failed read fully cleans up before trying another;
	// only this fixed SELECT snapshot is retried, never authorization writes.
	for attempt := 0; attempt < 5; attempt++ {
		requester, members, result = readClosedDiscoverySQL(ctx, db.SQL, expected, cluster.Member{ID: db.ID, Address: db.Address}, db.TransportMemberKey(), requesterKey, native)
		if !errors.Is(result, driver.ErrBadConn) || ctx.Err() != nil {
			return requester, members, result
		}
	}
	return requester, members, result
}

func readClosedDiscoverySQL(ctx context.Context, pool *sql.DB, expected ClusterIdentity, provider cluster.Member, providerKey, requesterKey string, native []cluster.Member) (requester ClusterAdmission, members []ClusterTransportMember, result error) {
	conn, err := pool.Conn(ctx)
	if err != nil {
		return ClusterAdmission{}, nil, err
	}
	defer func() { result = errors.Join(result, conn.Close()) }()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ClusterAdmission{}, nil, err
	}
	defer func() {
		err := tx.Rollback()
		// Cancellation can begin database/sql's own rollback. Conn.Close above
		// joins the borrowed connection; ErrTxDone alone is not that completion.
		if errors.Is(err, sql.ErrTxDone) && ctx.Err() != nil {
			err = nil
		}
		result = errors.Join(result, err)
	}()
	return readClosedClusterDiscovery(ctx, tx, expected, provider, providerKey, requesterKey, native)
}
