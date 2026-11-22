//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

// ClusterBackupOptions says where a backup goes and how long it may take.
type ClusterBackupOptions struct {
	// Dir receives the archive; empty means ClusterBackupDirectory.
	Dir string
	// ConfigFile, when set, is the installation's server.yaml to archive.
	ConfigFile string
	// Timeout bounds the reads from the database processes.
	Timeout time.Duration
}

// ClusterBackupDirectory is where a member keeps its backups by default.
func ClusterBackupDirectory(cfg Config) string {
	return filepath.Join(cfg.DataDir, "cluster-backups")
}

// ErrClusterBackupUnavailable reports that no consistent image could be read:
// no leader answered, or leadership moved on every attempt.
var ErrClusterBackupUnavailable = errors.New("cluster backup could not read a consistent database image")

// clusterBackupLeader finds the member that answers a quorum proof naming
// itself the leader, asking this member first.
func clusterBackupLeader(ctx context.Context, addresses []string, dial func(context.Context, string) (net.Conn, error)) (cluster.QuorumProof, string, error) {
	var failures []error
	for _, address := range addresses {
		if err := ctx.Err(); err != nil {
			return cluster.QuorumProof{}, "", err
		}
		proof, err := cluster.ReadQuorumProof(ctx, address, dial)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", address, err))
			continue
		}
		for _, member := range proof.Members {
			if member.ID == proof.LeaderID && member.Address == address {
				return proof, address, nil
			}
		}
	}
	return cluster.QuorumProof{}, "", fmt.Errorf("%w: no member answered as the leader: %w", ErrClusterBackupUnavailable, errors.Join(failures...))
}

// BackupCluster writes one archive of the cluster this installation is a
// member of, without stopping anything: the database as the leader holds it
// between two of its barriers, this member's master key and enrollment
// directory (the cluster authority and this member's key), and the files the
// database refers to by digest. It reads the database processes directly with
// this member's key, so it needs no session and works while business is
// closed. The archive holds secrets and is written as a private file.
func BackupCluster(ctx context.Context, cfg Config, options ClusterBackupOptions) (backup ClusterBackup, resultErr error) {
	if options.Timeout <= 0 {
		return backup, errors.New("a cluster backup requires a positive timeout")
	}
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.DataDir, "gpuworks.db")
	}
	report, err := readClusterStatus(cfg)
	if err != nil {
		return backup, err
	}
	if report.clusterID == "not recorded" || report.pin == "" || len(report.cert) == 0 || len(report.key) == 0 {
		return backup, errors.New("this installation holds no cluster member identity")
	}
	ca, err := readStatusFile(filepath.Join(cfg.DataDir, enrollmentDirectory, "ca.pem"))
	if err != nil {
		return backup, err
	}
	now := time.Now().UTC()
	trust, err := identity.NewTrust(ca, report.clusterID, report.pin, now)
	if err != nil {
		return backup, err
	}
	pair, err := tls.X509KeyPair(report.cert, report.key)
	if err != nil {
		return backup, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return backup, err
	}
	memberKey, err := trust.VerifyMember(leaf, now)
	if err != nil {
		return backup, err
	}
	policy, _, err := ClusterTransportPolicyPath(cfg)
	if err != nil {
		return backup, err
	}
	_, config, err := trust.TLSConfigsFromPolicy(policy, report.cert, report.key, now)
	if err != nil {
		return backup, err
	}
	dial := func(ctx context.Context, target string) (net.Conn, error) {
		return (&tls.Dialer{Config: config}).DialContext(ctx, "tcp", target)
	}
	callCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	addresses := []string{report.address}
	for _, member := range report.members {
		if member.address != "" && member.address != report.address {
			addresses = append(addresses, member.address)
		}
	}
	name := filepath.Base(cfg.DBPath)
	var image []byte
	var before, after cluster.QuorumProof
	for attempt := 0; image == nil; attempt++ {
		if attempt == 3 {
			return backup, fmt.Errorf("%w: leadership moved during three attempts", ErrClusterBackupUnavailable)
		}
		var leader string
		before, leader, err = clusterBackupLeader(callCtx, addresses, dial)
		if err != nil {
			return backup, err
		}
		candidate, err := cluster.ReadDatabaseImage(callCtx, leader, name, dial)
		if err != nil {
			return backup, fmt.Errorf("%w: %s: %w", ErrClusterBackupUnavailable, leader, err)
		}
		// The second barrier on the same member in the same term shows it was
		// the leader throughout, so the image holds only committed writes: all
		// of them up to the first barrier and none after the second.
		after, err = cluster.ReadQuorumProof(callCtx, leader, dial)
		if err == nil && after.Term == before.Term && after.LeaderID == before.LeaderID && after.Index >= before.Index {
			image = candidate
		}
	}
	dir := options.Dir
	if dir == "" {
		dir = ClusterBackupDirectory(cfg)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return backup, err
	}
	scratch, err := os.MkdirTemp(dir, ".cluster-backup-image-")
	if err != nil {
		return backup, err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(scratch)) }()
	imagePath := filepath.Join(scratch, name)
	if err := os.WriteFile(imagePath, image, 0600); err != nil {
		return backup, err
	}
	migrations, err := store.ImageMigrationVersions(callCtx, imagePath)
	if err != nil {
		return backup, err
	}
	extensions, err := store.ImageExtensionMigrationVersions(callCtx, imagePath)
	if err != nil {
		return backup, err
	}
	manifest := ClusterBackupManifest{
		Format: ClusterBackupFormat, CreatedAt: now.Truncate(time.Second), Release: version.Version,
		ClusterID: report.clusterID, CAFingerprint: report.pin, Protocol: report.protocol, StorageTier: string(report.storageTier),
		MemberAddress: report.address, MemberKey: memberKey, EnrollmentListen: report.enrollmentListen, EnrollmentURL: report.publicURL,
		Term: before.Term, IndexFrom: before.Index, IndexTo: after.Index,
		Migrations: migrations, ExtensionMigration: extensions, Excluded: clusterBackupExcluded,
	}
	for _, member := range after.Members {
		manifest.Members = append(manifest.Members, ClusterBackupMember{ID: member.ID, Address: member.Address, Role: member.Role})
	}
	entries := []clusterBackupEntry{
		{name: clusterBackupDatabaseName, path: imagePath},
		{name: "master.key", path: filepath.Join(cfg.DataDir, "master.key")},
	}
	for _, file := range []string{"ca.key", "ca.pem", "config.json", "master.binding", "server.key", "server.pem"} {
		entries = append(entries, clusterBackupEntry{name: enrollmentDirectory + "/" + file, path: filepath.Join(cfg.DataDir, enrollmentDirectory, file)})
	}
	if options.ConfigFile != "" {
		entries = append(entries, clusterBackupEntry{name: "server.yaml", path: options.ConfigFile})
	}
	material, err := clusterBackupMaterial(cfg.DataDir)
	if err != nil {
		return backup, err
	}
	entries = append(entries, material...)
	dest := filepath.Join(dir, clusterBackupArchiveName(manifest.CreatedAt))
	if _, err := writeClusterBackupArchive(dest, manifest, entries); err != nil {
		return backup, err
	}
	// What is returned is what a later reader will find, checked in full.
	return InspectClusterBackup(dest)
}
