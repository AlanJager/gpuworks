//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ErrClusterRestoreRefused reports a restore that did not start: the target
// is not empty, a member of the cluster still answers, or the archive is not
// one this installation can be rebuilt from. Nothing was changed.
var ErrClusterRestoreRefused = errors.New("cluster restore refused")

// ClusterRestoreOptions bounds a restore and names where the archived
// server.yaml goes when the installation has none.
type ClusterRestoreOptions struct {
	ConfigFile string
	Timeout    time.Duration
}

// ClusterRestoreResult says what a restore built. Rejoin lists the database
// addresses of the archive's other members: their admissions are revoked, and
// each joins the restored cluster again from an empty data directory.
type ClusterRestoreResult struct {
	Manifest ClusterBackupManifest
	Member   cluster.Member
	Rejoin   []string
}

// clusterRestoreAnswering returns the first address a process still listens
// on. A refused or unanswered connection is the expected state of a stopped
// cluster; only an accepted connection counts.
func clusterRestoreAnswering(ctx context.Context, addresses []string) string {
	seen := make(map[string]bool)
	for _, address := range addresses {
		if address == "" || seen[address] {
			continue
		}
		seen[address] = true
		conn, err := (&net.Dialer{Timeout: 1500 * time.Millisecond}).DialContext(ctx, "tcp", address)
		if err == nil {
			_ = conn.Close()
			return address
		}
	}
	return ""
}

// RestoreClusterMember rebuilds this installation's data directory from a
// cluster backup as the only member of the restored cluster. It is run with
// every member stopped, on the host that holds the addresses of the member
// that took the backup, into a data directory that does not exist or is
// empty. The database keeps the image's schema: no migration runs. The other
// members' admissions are revoked; they join again like new members.
//
// The data directory appears complete or not at all. A failure leaves the
// target as it was and removes the private staging directory.
func RestoreClusterMember(ctx context.Context, cfg Config, archive string, options ClusterRestoreOptions) (result ClusterRestoreResult, resultErr error) {
	if options.Timeout <= 0 {
		return result, errors.New("a cluster restore requires a positive timeout")
	}
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrClusterRestoreRefused, fmt.Sprintf(format, args...))
	}
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil || cfg.DataDir == "" {
		return result, refuse("a data directory is required")
	}
	name := filepath.Base(clusterBackupDatabaseName)
	if cfg.DBPath != "" && filepath.Clean(cfg.DBPath) != filepath.Join(dataDir, name) {
		return result, refuse("the database path must be %s inside the data directory", name)
	}
	if entries, err := os.ReadDir(dataDir); err == nil && len(entries) != 0 {
		return result, refuse("%s is not empty; move the old data directory aside first", dataDir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	backup, err := InspectClusterBackup(archive)
	if err != nil {
		return result, err
	}
	manifest := backup.Manifest
	result.Manifest = manifest
	ctx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	addresses := []string{manifest.MemberAddress, manifest.EnrollmentListen}
	for _, member := range manifest.Members {
		addresses = append(addresses, member.Address)
	}
	if address := clusterRestoreAnswering(ctx, addresses); address != "" {
		return result, refuse("a process answers at %s; stop every member of the cluster before restoring", address)
	}
	stage, err := os.MkdirTemp(filepath.Dir(dataDir), ".cluster-restore-")
	if err != nil {
		return result, err
	}
	// The stage holds the cluster authority's key and the master key.
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(stage)) }()
	unpacked := filepath.Join(stage, "unpacked")
	data := filepath.Join(stage, "data")
	converted := filepath.Join(stage, "converted")
	for _, dir := range []string{unpacked, data, converted, filepath.Join(stage, "single"), filepath.Join(stage, "verification")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			return result, err
		}
	}
	if _, err := readClusterBackup(archive, unpacked); err != nil {
		return result, err
	}
	for from, to := range map[string]string{"master.key": "master.key", enrollmentDirectory: enrollmentDirectory} {
		if err := os.Rename(filepath.Join(unpacked, from), filepath.Join(data, to)); err != nil {
			return result, err
		}
	}
	for _, directory := range clusterBackupMaterialDirectories {
		err := os.Rename(filepath.Join(unpacked, "data", directory), filepath.Join(data, directory))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
	}
	bundle, err := readEnrollmentBundle(data, false)
	if err != nil {
		return result, err
	}
	if bundle == nil || bundle.info.ClusterID != manifest.ClusterID || bundle.info.CAFingerprint != manifest.CAFingerprint {
		return result, fmt.Errorf("%w: the enrollment directory is not the manifest's cluster", ErrClusterBackupInvalid)
	}
	now := time.Now().UTC()
	memberKey, err := bundle.trust.VerifyMember(bundle.tls.Certificates[0].Leaf, now)
	if err != nil {
		return result, err
	}
	host, _, err := net.SplitHostPort(manifest.MemberAddress)
	if err != nil || memberKey != manifest.MemberKey || bundle.tls.Certificates[0].Leaf.VerifyHostname(host) != nil {
		return result, fmt.Errorf("%w: the member key is not the one the manifest names for %s", ErrClusterBackupInvalid, manifest.MemberAddress)
	}
	tier, err := storagetier.Parse(bundle.info.StorageTier)
	if err != nil {
		return result, err
	}
	cluster.UseStorageTier(tier)
	expected := store.ClusterIdentity{ClusterID: manifest.ClusterID, CAFingerprint: manifest.CAFingerprint, Protocol: manifest.Protocol}

	// A private single member receives the image; the same conversion an
	// installation uses then gives it this member's address and key.
	single := filepath.Join(stage, "single", name)
	rejoin, revoked, err := restoreClusterImage(ctx, single+".dqlite", name, filepath.Join(unpacked, filepath.FromSlash(clusterBackupDatabaseName)), expected, memberKey)
	if err != nil {
		return result, err
	}
	result.Rejoin = rejoin
	cert, err := os.ReadFile(filepath.Join(data, enrollmentDirectory, "server.pem"))
	if err != nil {
		return result, err
	}
	key, err := os.ReadFile(filepath.Join(data, enrollmentDirectory, "server.key"))
	if err != nil {
		return result, err
	}
	// The first policy names the revoked keys: a running member refuses a
	// refresh that adds a revocation, so one written without them would stop
	// following the database, and no member could join.
	policy := identity.TransportPolicy{ClusterID: manifest.ClusterID, CAFingerprint: manifest.CAFingerprint, Revision: 1, AllowedKeys: []string{memberKey}, RevokedKeys: revoked}
	authorize, err := policy.Authorizer(manifest.ClusterID, manifest.CAFingerprint)
	if err != nil {
		return result, err
	}
	listen, dial, err := bundle.trust.TLSConfigs(cert, key, now, authorize)
	if err != nil {
		return result, err
	}
	target := cluster.DatabaseConfig{
		Dir: filepath.Join(converted, name+".dqlite"), Address: manifest.MemberAddress, Name: name,
		ListenTLS: listen, DialTLS: dial, TLSCertPEM: cert, TLSKeyPEM: key, TLSCACertPEM: bundle.ca,
		TransportPolicyPath: filepath.Join(stage, "verification", "policy.json"),
	}
	if err := identity.PublishTransportPolicy(target.TransportPolicyPath, 0, policy); err != nil {
		return result, err
	}
	member, err := cluster.PrepareSingleMemberAddress(ctx, single, filepath.Join(stage, "conversion"), target)
	if err != nil {
		return result, err
	}
	result.Member = member
	if err := cluster.PrepareDatabaseTransport(target.Dir, manifest.MemberAddress, manifest.ClusterID, manifest.CAFingerprint, bundle.ca, cert, key, policy); err != nil {
		return result, err
	}
	db, err := cluster.OpenConfiguredDatabase(ctx, target.Dir, name)
	if err != nil {
		return result, err
	}
	verifyErr := restoredClusterIdentity(ctx, db, expected)
	if err := errors.Join(verifyErr, db.Close()); err != nil {
		return result, err
	}
	// Only the database directory moves into the data directory; what the
	// conversion and the verification start left beside it stays in the stage.
	if err := os.Rename(target.Dir, filepath.Join(data, name+".dqlite")); err != nil {
		return result, err
	}
	if err := syncAddressActivationTree(ctx, data); err != nil {
		return result, err
	}
	if options.ConfigFile != "" {
		archived := filepath.Join(unpacked, "server.yaml")
		if _, err := os.Lstat(options.ConfigFile); errors.Is(err, os.ErrNotExist) {
			if raw, err := os.ReadFile(archived); err == nil {
				if err := os.WriteFile(options.ConfigFile, raw, 0600); err != nil {
					return result, err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return result, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	// An empty target directory is removed so the complete one can take its
	// name; the rename never replaces anything.
	if err := os.Remove(dataDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, data, unix.AT_FDCWD, dataDir, unix.RENAME_NOREPLACE); err != nil {
		return result, fmt.Errorf("publish the restored data directory: %w", err)
	}
	return result, syncEnrollmentDirectory(filepath.Dir(dataDir))
}

// restoreClusterImage loads the image into a new private single-member
// database and revokes every admission but the restoring member's.
func restoreClusterImage(ctx context.Context, dir, name, image string, expected store.ClusterIdentity, memberKey string) (rejoin, revoked []string, resultErr error) {
	db, err := cluster.OpenSingleDatabase(ctx, dir, name)
	if err != nil {
		return nil, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, db.Close()) }()
	st, err := store.OpenReplicated(ctx, db.SQL, db.Scheduling, bus.New(), store.ReplicatedBackend())
	if err != nil {
		return nil, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, st.Close()) }()
	if err := st.RestoreDatabaseImage(ctx, image); err != nil {
		return nil, nil, err
	}
	actual, err := st.ClusterIdentity(ctx)
	if err != nil {
		return nil, nil, err
	}
	if actual.ClusterID != expected.ClusterID || actual.CAFingerprint != expected.CAFingerprint || actual.Protocol != expected.Protocol {
		return nil, nil, fmt.Errorf("%w: the database image belongs to another cluster", ErrClusterBackupInvalid)
	}
	return st.RevokeOtherClusterAdmissions(ctx, expected.ClusterID, memberKey)
}

// restoredClusterIdentity reads the cluster identity through the restored
// member's own address and key. It does not compare schemas: the image keeps
// the schema of the release that wrote it.
func restoredClusterIdentity(ctx context.Context, db *cluster.Database, expected store.ClusterIdentity) (resultErr error) {
	if id, pin := db.TransportIdentity(); id != expected.ClusterID || pin != expected.CAFingerprint {
		return store.ErrClusterIdentityConflict
	}
	st, err := store.OpenReplicated(ctx, db.SQL, db.Scheduling, bus.New(), store.ReplicatedBackend())
	if err != nil {
		return err
	}
	actual, err := st.ClusterIdentity(ctx)
	if err != nil {
		return err
	}
	if actual.ClusterID != expected.ClusterID || actual.CAFingerprint != expected.CAFingerprint || actual.Protocol != expected.Protocol {
		return store.ErrClusterIdentityConflict
	}
	return nil
}
