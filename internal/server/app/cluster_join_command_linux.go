//go:build linux && dqlite

package app

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// JoinCluster is the authorized database join. It checks the persisted
// identity, starts the existing Raft join, and observes live membership.
// WaitReady is not the catch-up proof. MaterialsReady is not application
// readiness. The returned session keeps the node running and does not open
// the application; Close releases it.
func JoinCluster(ctx context.Context, cfg Config, target ClusterJoinTarget) (_ *ClusterRaftSession, result ClusterCatchUp, resultErr error) {
	session, err := StartClusterRaft(ctx, cfg, target)
	if err != nil {
		return nil, ClusterCatchUp{}, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, session.Close())
		}
	}()
	if err := session.WaitReady(ctx); err != nil {
		return nil, ClusterCatchUp{}, err
	}
	refresh, err := store.JoinedTransportAuthorizationRefresher(ctx, session.Database())
	if err != nil {
		return nil, ClusterCatchUp{}, err
	}
	if err := refresh(ctx); err != nil {
		return nil, ClusterCatchUp{}, err
	}
	session.startTransportAuthorization(refresh)
	if err := session.installJoinedEnrollmentIdentity(ctx, cfg); err != nil {
		return nil, ClusterCatchUp{}, err
	}
	if err := session.installJoinedCatalog(ctx, cfg); err != nil {
		return nil, ClusterCatchUp{}, err
	}
	if err := session.startMaterialListener(ctx, cfg); err != nil {
		return nil, ClusterCatchUp{}, err
	}
	if _, err := committedJoinMaterialDigest(cfg); err != nil {
		return nil, ClusterCatchUp{}, err
	}
	observed, err := observeClusterCatchUp(ctx, session.Database())
	if err != nil {
		return nil, ClusterCatchUp{}, err
	}
	observed.MaterialsReady, err = checkJoinedMaterialReadiness(ctx, session, cfg)
	if err != nil {
		return nil, ClusterCatchUp{}, err
	}
	return session, observed, nil
}

// observeClusterCatchUp reads live membership. With fewer than three members
// it returns the current role and does not claim fault tolerance. At three or
// more members it waits until this node is a voter in a three-voter set.
// Membership is not proved with a SQL ping and roles are not assigned.
// The material-dependent step is checked separately against the current
// manifest and authenticated bytes after membership has been observed.
func observeClusterCatchUp(ctx context.Context, db *cluster.Database) (ClusterCatchUp, error) {
	if db == nil || db.ID == 0 {
		return ClusterCatchUp{}, errors.New("catch-up requires the joined database")
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		members, err := db.Members(ctx)
		if err == nil {
			observed := summarizeCatchUp(db.ID, members)
			if observed.Address != "" && (len(members) < 3 || (observed.Role == "voter" && observed.Voters >= 3)) {
				return observed, nil
			}
		} else if ctx.Err() != nil {
			return ClusterCatchUp{}, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ClusterCatchUp{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func committedJoinMaterialDigest(cfg Config) (string, error) {
	if cfg.DataDir == "" {
		return "", errors.New("material join readiness requires the data directory")
	}
	root, err := filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return "", err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", err
	}
	raw, err := readJoinMaterialFile(filepath.Join(root, "gpuworks.db.join", "materials.commit"), 256)
	if err != nil {
		return "", err
	}
	fields := strings.Split(string(raw), "\n")
	if len(fields) != 3 || fields[2] != "" || !strings.HasPrefix(fields[0], ".materials-") || filepath.Base(fields[0]) != fields[0] || len(fields[1]) != 64 {
		return "", errors.New("invalid join material commit identity")
	}
	if _, err := hex.DecodeString(fields[1]); err != nil {
		return "", errors.New("invalid material digest")
	}
	return fields[1], nil
}

func summarizeCatchUp(id uint64, members []cluster.Member) ClusterCatchUp {
	observed := ClusterCatchUp{ID: id}
	for _, member := range members {
		if member.Role == "voter" {
			observed.Voters++
		}
		if member.ID == id {
			observed.Address = member.Address
			observed.Role = member.Role
		}
	}
	observed.FaultTolerant = observed.Voters >= 3 && observed.Role == "voter"
	return observed
}

// ConfirmClusterCatchUp moves leadership to nodes[0], commits a row there,
// stops that leader, and requires nodes[1] to read the row and commit another.
// The voter set and member identities must be unchanged. Fewer than three
// voters is refused.
func ConfirmClusterCatchUp(ctx context.Context, nodes []*cluster.Database) (prior, next string, stopped uint64, err error) {
	if len(nodes) < 2 {
		return "", "", 0, errors.New("catch-up requires a surviving peer")
	}
	before, err := nodes[0].Members(ctx)
	if err != nil {
		return "", "", 0, err
	}
	if summarizeCatchUp(nodes[0].ID, before).Voters < 3 {
		return "", "", 0, errors.New("catch-up requires three voters")
	}
	ids := map[uint64]bool{}
	for _, member := range before {
		ids[member.ID] = true
	}
	for _, node := range nodes {
		if node == nil || !ids[node.ID] {
			return "", "", 0, errors.New("catch-up node is not in the live membership")
		}
	}
	if err := transferLeader(ctx, nodes[0]); err != nil {
		return "", "", 0, err
	}
	var n uint64
	if err := binary.Read(rand.Reader, binary.BigEndian, &n); err != nil {
		return "", "", 0, err
	}
	n = n&0x7fffffffffffffff | 1
	prior = fmt.Sprintf("catchup-%x", n)
	if _, err := nodes[0].SQL.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS gpuworks_catchup (id INTEGER PRIMARY KEY, note TEXT)`); err != nil {
		return "", "", 0, err
	}
	if _, err := nodes[0].SQL.ExecContext(ctx, `INSERT INTO gpuworks_catchup (id, note) VALUES (?, ?)`, int64(n), prior); err != nil {
		return "", "", 0, err
	}
	leaderID := nodes[0].ID
	if err := nodes[0].Close(); err != nil {
		return "", "", leaderID, err
	}
	survivor := nodes[1]
	note, err := awaitNote(ctx, survivor, int64(n))
	if err != nil {
		return "", "", leaderID, err
	}
	if note != prior {
		return "", "", leaderID, errors.New("survivor read a different committed row")
	}
	next = prior + "-next"
	if _, err := survivor.SQL.ExecContext(ctx, `INSERT INTO gpuworks_catchup (id, note) VALUES (?, ?)`, int64(n)+1, next); err != nil {
		return "", "", leaderID, err
	}
	after, err := survivor.Members(ctx)
	if err != nil {
		return "", "", leaderID, err
	}
	if len(after) != len(before) || summarizeCatchUp(survivor.ID, after).Voters < 3 {
		return "", "", leaderID, errors.New("catch-up changed the voter set")
	}
	seen := map[uint64]bool{}
	for _, member := range after {
		if !ids[member.ID] || seen[member.ID] {
			return "", "", leaderID, errors.New("catch-up changed member identity")
		}
		seen[member.ID] = true
	}
	return prior, next, leaderID, nil
}

func transferLeader(ctx context.Context, db *cluster.Database) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		leader, err := db.Leader(ctx)
		if err == nil && leader.ID == db.ID {
			return nil
		}
		if err := db.Transfer(ctx, db.ID); err != nil {
			last = err
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

func awaitNote(ctx context.Context, db *cluster.Database, id int64) (string, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		var note string
		err := db.SQL.QueryRowContext(ctx, `SELECT note FROM gpuworks_catchup WHERE id = ?`, id).Scan(&note)
		if err == nil {
			return note, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return "", errors.Join(ctx.Err(), last)
		case <-ticker.C:
		}
	}
}
