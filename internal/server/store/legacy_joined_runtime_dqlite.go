//go:build linux && dqlite

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// LegacyJoinedRaftSession owns a resumed v114 member and its runtime lease.
// Raft remains running if OpenStore cannot yet reach quorum; callers close the
// session only when abandoning startup. After OpenStore succeeds, Store.Close
// owns the session. Neither step calls the remote material/member HTTP API.
type LegacyJoinedRaftSession struct {
	db       *cluster.Database
	id       uint64
	address  string
	raftDir  string
	release  func() error
	mu       sync.Mutex
	once     sync.Once
	closeErr error
	owned    bool
	// live is db for the cluster upgrade steps, which must not wait for a
	// classification that holds mu behind a running migration.
	live atomic.Pointer[cluster.Database]
	// upgradeSets are the executable's extension migration sets, and
	// migration the one schema migration this member runs at a time.
	upgradeMu   sync.Mutex
	upgradeSets []MigrationSet
	migration   clusterMigrationRun
}

// newJoinedRaftSession is the session of a member whose database runs.
func newJoinedRaftSession(db *cluster.Database, id uint64, address, raftDir string, release func() error) *LegacyJoinedRaftSession {
	s := &LegacyJoinedRaftSession{db: db, id: id, address: address, raftDir: raftDir, release: release}
	s.live.Store(db)
	return s
}

// StartLegacyJoinedRaft uses the persisted discovery cache only as dial seeds.
// It requires a complete existing join identity, maintenance guard, native
// Raft files, and transport policy before starting the local database. The
// cache is not a membership or quorum proof; OpenStore checks live membership.
func StartLegacyJoinedRaft(ctx context.Context, path, address, clusterID, pin string, ca, certificate, key []byte) (_ *LegacyJoinedRaftSession, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, release, err := LockRuntimeDatabase(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, release())
		}
	}()
	if err := CheckRestoreCompleted(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("existing SQLite database %q requires explicit import", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	raftDir, joinDir := path+".dqlite", path+".join"
	if err := checkExistingNativeFiles(raftDir); err != nil {
		return nil, err
	}
	if info, err := os.Stat(raftDir); err != nil {
		return nil, err
	} else if owner, ok := info.Sys().(*syscall.Stat_t); !ok || owner.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("run joined Raft as its persisted directory owner")
	}
	keyFingerprint, err := persistedLegacyJoinIdentity(path, address, clusterID, pin, key)
	if err != nil {
		return nil, err
	}
	if err := requireLegacyJoinGuard(path+".maintenance", keyFingerprint); err != nil {
		return nil, err
	}
	id, seeds, err := cluster.PublishedJoinSeeds(ctx, raftDir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(raftDir, "join")); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("persisted Raft join marker requires explicit quorum-backed reconciliation")
	}
	trust, err := identity.NewTrust(ca, clusterID, pin, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	db, err := cluster.StartAuthorizedDatabase(ctx, cluster.DatabaseConfig{
		Dir: raftDir, Address: address, Seeds: seeds, Name: filepath.Base(path),
	}, filepath.Join(joinDir, "transport-policy.json"), trust, certificate, key)
	if err != nil {
		return nil, err
	}
	if db.ID != id || db.Address != address || db.TransportMemberKey() != keyFingerprint {
		return nil, errors.Join(ErrClusterIdentityConflict, db.Close())
	}
	return newJoinedRaftSession(db, id, address, raftDir, release), nil
}

// OpenStore waits for SQL quorum, then verifies live member identity and exact
// v114 markers before transferring ownership. Failure leaves Raft running so
// the caller may retry without publishing a new identity or clearing a marker.
func (s *LegacyJoinedRaftSession) OpenStore(ctx context.Context, b *bus.Bus) (*Store, error) {
	st, _, err := s.openClassifiedStore(ctx, b, true)
	return st, err
}

// OpenClassifiedStore observes one quorum-backed schema and identity snapshot.
// It transfers the same Raft process and lease to Store.Close on success.
// For current schema, the caller must recheck durable phase before any startup
// write; classification is not authority for subsequent application writes.
func (s *LegacyJoinedRaftSession) OpenClassifiedStore(ctx context.Context, b *bus.Bus) (*Store, bool, error) {
	return s.openClassifiedStore(ctx, b, false)
}

func (s *LegacyJoinedRaftSession) openClassifiedStore(ctx context.Context, b *bus.Bus, legacyOnly bool) (*Store, bool, error) {
	if s == nil {
		return nil, false, errors.New("legacy joined Raft session is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil || s.owned {
		return nil, false, errors.New("legacy joined Raft session is unavailable or already owned")
	}
	if err := s.db.WaitReady(ctx); err != nil {
		return nil, false, err
	}
	live, err := s.db.Members(ctx)
	if err != nil {
		return nil, false, err
	}
	live, err = normalizeLiveClusterMembers(live)
	if err != nil || len(live) < 2 {
		return nil, false, errors.Join(ErrClusterIdentityConflict, err)
	}
	local := 0
	for _, member := range live {
		if member.ID == s.id && member.Address == s.address {
			local++
		}
	}
	if local != 1 || s.db.ID != s.id || s.db.Address != s.address {
		return nil, false, ErrClusterIdentityConflict
	}
	if _, err := os.Lstat(filepath.Join(s.raftDir, "join")); !errors.Is(err, os.ErrNotExist) {
		return nil, false, errors.New("persisted Raft join marker reappeared")
	}
	clusterID, pin := s.db.TransportIdentity()
	key := s.db.TransportMemberKey()
	if !validClusterDigest(clusterID, 16) || !validClusterDigest(pin, 32) || !validClusterDigest(key, 32) {
		return nil, false, ErrClusterIdentityConflict
	}
	st, err := OpenReplicated(ctx, s.db.SQL, s.db.Scheduling, b, ReplicatedBackend())
	if err != nil {
		return nil, false, err
	}
	expected := ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: ClusterProtocolVersion}
	legacy, err := observeEstablishedRuntimeSnapshot(ctx, st.db, expected, key, s.address, s.raftDir)
	if err != nil {
		return nil, false, err
	}
	if legacyOnly && !legacy {
		return nil, false, ErrSchemaIncompatible
	}
	bindOwnedMemberStore(ctx, st, s.db, expected, s.Close)
	s.owned = true
	return st, legacy, nil
}

// Only failed reads inside this known read-only transaction may be observed
// again. A completion error after successful reads remains fatal, even if the
// driver calls it ErrBadConn: an unknown COMMIT must never be replayed.
func observeEstablishedRuntimeSnapshot(ctx context.Context, db *gorm.DB, expected ClusterIdentity, key, address, raftDir string) (bool, error) {
	for {
		var legacy bool
		var readErr error
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			legacy, readErr = classifyEstablishedRuntimeSnapshot(ctx, tx, expected, key, address, raftDir)
			return readErr
		})
		if !retryableClassificationRead(readErr) {
			return legacy, err
		}
		// The transaction has rolled back before waiting. Retain the caller's
		// original attempt budget and the native cause on cancellation/expiry.
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

func (s *LegacyJoinedRaftSession) Close() error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		s.mu.Lock()
		db, release := s.db, s.release
		s.db = nil
		s.live.Store(nil)
		s.release = nil
		s.mu.Unlock()
		s.closeErr = db.Close()
		if release != nil {
			s.closeErr = errors.Join(s.closeErr, release())
		}
	})
	return s.closeErr
}

func persistedLegacyJoinIdentity(path, address, clusterID, pin string, key []byte) (string, error) {
	dir := path + ".join"
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("legacy join identity requires a private plain directory")
	}
	persistedKey, err := identity.LoadMemberKey(filepath.Join(dir, "member.key"))
	if err != nil || !bytes.Equal(persistedKey, key) {
		return "", errors.New("legacy joined key differs from its persisted identity")
	}
	csr, err := identity.NewMemberCSR(key)
	if err != nil {
		return "", err
	}
	fingerprint, err := identity.CSRKeyFingerprint(csr)
	if err != nil {
		return "", err
	}
	configPath := filepath.Join(dir, "config.json")
	info, err = os.Lstat(configPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() <= 0 || info.Size() > 128<<10 {
		return "", errors.New("legacy join configuration requires a private bounded file")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "", err
	}
	var config struct {
		ClusterID        string `json:"cluster_id"`
		CAFingerprint    string `json:"ca_fingerprint"`
		Protocol         uint   `json:"protocol"`
		EnrollmentURL    string `json:"enrollment_url"`
		EnrollmentOrigin string `json:"enrollment_origin"`
		EnrollmentListen string `json:"enrollment_listen"`
		Address          string `json:"address"`
		KeyFingerprint   string `json:"key_fingerprint"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil || decoder.Decode(new(any)) != io.EOF {
		return "", errors.New("invalid persisted legacy join configuration")
	}
	if config.ClusterID != clusterID || config.CAFingerprint != pin || config.Protocol != ClusterProtocolVersion || config.Address != address || config.KeyFingerprint != fingerprint {
		return "", ErrClusterIdentityConflict
	}
	return fingerprint, nil
}

func requireLegacyJoinGuard(path, fingerprint string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("legacy joined runtime requires its exact maintenance guard")
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, []byte("cluster-join "+fingerprint+"\n")) {
		return errors.New("legacy joined maintenance guard differs from persisted identity")
	}
	return nil
}
