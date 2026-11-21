//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// These records describe filesystem publication, not a business task queue.
// The maintenance record remains the startup barrier until publication is durable.
type addressActivationIntent struct {
	Version        int    `json:"version"`
	Source         string `json:"source"`
	Address        string `json:"address"`
	ClusterID      string `json:"cluster_id"`
	CAFingerprint  string `json:"ca_fingerprint"`
	KeyFingerprint string `json:"key_fingerprint"`
	SourceDigest   string `json:"source_digest"`
	Operation      string `json:"operation"`
}
type addressActivationReady struct {
	Candidate       string         `json:"candidate"`
	SourceDigest    string         `json:"source_digest"`
	CandidateDigest string         `json:"candidate_digest"`
	Member          cluster.Member `json:"member"`
}

// ActivateDatabaseAddress converts a stopped, initialized single-member server
// to its explicit mTLS database address. It preserves cold backups and resumes
// interrupted publication. It does not grant multi-member application readiness.
func ActivateDatabaseAddress(ctx context.Context, cfg Config, address string) (cluster.Member, error) {
	return activateDatabaseAddress(ctx, cfg, address, nil)
}

func activateDatabaseAddress(ctx context.Context, cfg Config, address string, checkpoint func(string) error) (member cluster.Member, resultErr error) {
	if err := ctx.Err(); err != nil {
		return member, err
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return member, err
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 || host == "" {
		return member, errors.New("database address requires an explicit host and port")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsUnspecified() || ip.IsMulticast()) {
		return member, errors.New("database address must identify a member")
	}
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.DataDir, "gpuworks.db")
	}
	path, release, err := store.LockRuntimeDatabase(cfg.DBPath)
	if err != nil {
		return member, err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	if err := store.CheckRestoreCompleted(cfg.DataDir); err != nil {
		return member, err
	}
	if err := store.CheckRestoreCompleted(filepath.Dir(path)); err != nil {
		return member, err
	}
	for _, blocked := range []string{path, path + ".join"} {
		if _, err := os.Lstat(blocked); !errors.Is(err, os.ErrNotExist) {
			return member, fmt.Errorf("address activation refuses existing or unreadable %s", blocked)
		}
	}
	info, err := os.Lstat(path + ".dqlite")
	if err != nil {
		return member, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || owner.Uid != uint32(os.Geteuid()) {
		return member, errors.New("run address activation as the existing native database directory owner")
	}
	bundle, err := readEnrollmentBundle(cfg.DataDir, false)
	if err != nil {
		return member, err
	}
	if bundle == nil {
		return member, errors.New("initialize the existing server's enrollment identity before activating its database address")
	}
	for _, listener := range []string{bundle.info.ListenAddress, cfg.Addr} {
		_, listenerPort, splitErr := net.SplitHostPort(listener)
		if splitErr == nil {
			listenerNumber, parseErr := strconv.Atoi(listenerPort)
			if parseErr == nil && listenerNumber == number {
				return member, errors.New("database port must differ from the enrollment and HTTP listener ports")
			}
		}
	}
	if err := bundle.tls.Certificates[0].Leaf.VerifyHostname(host); err != nil {
		return member, fmt.Errorf("database address must match the initialized member certificate: %w", err)
	}
	keyFingerprint, err := bundle.trust.VerifyMember(bundle.tls.Certificates[0].Leaf, time.Now())
	if err != nil {
		return member, err
	}
	key, err := os.ReadFile(filepath.Join(cfg.DataDir, enrollmentDirectory, "server.key"))
	if err != nil {
		return member, err
	}
	cert, err := os.ReadFile(filepath.Join(cfg.DataDir, enrollmentDirectory, "server.pem"))
	if err != nil {
		return member, err
	}
	policy := identity.TransportPolicy{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Revision: 1, AllowedKeys: []string{keyFingerprint}}
	authorize, err := policy.Authorizer(bundle.info.ClusterID, bundle.info.CAFingerprint)
	if err != nil {
		return member, err
	}
	listen, dial, err := bundle.trust.TLSConfigs(cert, key, time.Now(), authorize)
	if err != nil {
		return member, err
	}
	target := cluster.DatabaseConfig{Address: address, Name: filepath.Base(path), ListenTLS: listen, DialTLS: dial, TLSCertPEM: cert, TLSKeyPEM: key, TLSCACertPEM: bundle.ca}
	expected := store.ClusterIdentity{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Protocol: bundle.info.Protocol}
	marker := path + ".maintenance"
	var intent addressActivationIntent
	err = readAddressActivation(marker, &intent)
	if errors.Is(err, os.ErrNotExist) {
		// A completed retry may follow real application writes. Verify the current
		// authority and member instead of comparing obsolete cold-file digests.
		if _, err := os.Lstat(filepath.Join(path+".dqlite", ".transport-required")); err == nil {
			db, err := cluster.OpenConfiguredDatabase(ctx, path+".dqlite", target.Name)
			if err != nil {
				return member, err
			}
			member = cluster.Member{ID: db.ID, Address: db.Address, Role: "voter"}
			verifyErr := verifyAddressActivation(ctx, db, expected)
			if db.Address != address {
				verifyErr = errors.Join(verifyErr, errors.New("existing transport address differs; refusing a second conversion"))
			}
			if verifyErr == nil {
				verifyErr = initializeAddressAdmission(ctx, db, expected, keyFingerprint, address)
			}
			return member, errors.Join(verifyErr, db.Close())
		} else if !errors.Is(err, os.ErrNotExist) {
			return member, err
		}
		if _, err := os.Lstat(filepath.Join(path+".dqlite", ".transport")); !errors.Is(err, os.ErrNotExist) {
			return member, errors.New("transport bundle without activation marker requires explicit recovery")
		}
		// Only the established native database may be inspected; never synthesize
		// an empty source when files or identity are missing.
		for _, name := range []string{"info.yaml", "cluster.yaml"} {
			if info, err := os.Lstat(filepath.Join(path+".dqlite", name)); err != nil || !info.Mode().IsRegular() {
				return member, errors.New("existing native database identity is required")
			}
		}
		db, err := cluster.OpenSingleDatabase(ctx, path+".dqlite", target.Name)
		if err != nil {
			return member, err
		}
		verifyErr := verifyAddressActivation(ctx, db, expected)
		if db.Address == address {
			verifyErr = errors.Join(verifyErr, errors.New("conversion requires a different database address"))
		}
		if err := errors.Join(verifyErr, db.Close()); err != nil {
			return member, err
		}
		digest, err := addressActivationDigest(ctx, path+".dqlite")
		if err != nil {
			return member, err
		}
		operation, err := os.MkdirTemp(filepath.Dir(path), "."+filepath.Base(path)+"-address-")
		if err != nil {
			return member, err
		}
		intent = addressActivationIntent{Version: 1, Source: path, Address: address, ClusterID: expected.ClusterID, CAFingerprint: expected.CAFingerprint, KeyFingerprint: keyFingerprint, SourceDigest: digest, Operation: operation}
		if err := writeAddressActivation(filepath.Join(operation, "intent.json"), intent); err != nil {
			return member, err
		}
		if err := writeAddressActivation(marker, intent); err != nil {
			return member, err
		}
	} else if err != nil {
		return member, err
	}
	if intent.Version != 1 || intent.Source != path || intent.Address != address || intent.ClusterID != expected.ClusterID || intent.CAFingerprint != expected.CAFingerprint || intent.KeyFingerprint != keyFingerprint || len(intent.SourceDigest) != 64 || filepath.Dir(intent.Operation) != filepath.Dir(path) || !strings.HasPrefix(filepath.Base(intent.Operation), "."+filepath.Base(path)+"-address-") {
		return member, errors.New("maintenance intent does not match this address activation")
	}
	// Reject redirected operation directories even when JSON itself is valid.
	if info, err := os.Lstat(intent.Operation); err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return member, errors.New("address activation requires its original private operation directory")
	}
	var original addressActivationIntent
	if err := readAddressActivation(filepath.Join(intent.Operation, "intent.json"), &original); err != nil {
		return member, err
	}
	if original != intent {
		return member, errors.New("maintenance intent differs from original operation")
	}
	if err := addressActivationCheckpoint(checkpoint, "intent"); err != nil {
		return member, err
	}
	var ready addressActivationReady
	readyPath := filepath.Join(intent.Operation, "ready.json")
	err = readAddressActivation(readyPath, &ready)
	if errors.Is(err, os.ErrNotExist) {
		digest, err := addressActivationDigest(ctx, path+".dqlite")
		if err != nil || digest != intent.SourceDigest {
			return member, errors.Join(errors.New("source changed during address maintenance"), err)
		}
		attempt, err := os.MkdirTemp(intent.Operation, "attempt-")
		if err != nil {
			return member, err
		}
		target.Dir = filepath.Join(attempt, "database")
		maintenance, err := os.ReadFile(marker)
		if err != nil {
			return member, err
		}
		// The database process authorizes each handshake from a policy file.
		// The candidate has no transport bundle until it is verified, so the
		// verification start reads the same one-key policy from this private
		// attempt directory; PrepareDatabaseTransport publishes the durable copy.
		verification := filepath.Join(attempt, "verification")
		if err := os.Mkdir(verification, 0700); err != nil {
			return member, err
		}
		target.TransportPolicyPath = filepath.Join(verification, "policy.json")
		if err := identity.PublishTransportPolicy(target.TransportPolicyPath, 0, policy); err != nil {
			return member, err
		}
		member, err = cluster.PrepareSingleMemberAddressUnderLease(ctx, path, filepath.Join(attempt, "backup"), target, maintenance)
		if err != nil {
			return member, err
		}
		if err := os.RemoveAll(verification); err != nil {
			return member, err
		}
		if err := cluster.PrepareDatabaseTransport(target.Dir, address, expected.ClusterID, expected.CAFingerprint, bundle.ca, cert, key, policy); err != nil {
			return member, err
		}
		db, err := cluster.OpenConfiguredDatabase(ctx, target.Dir, target.Name)
		if err != nil {
			return member, err
		}
		if err := errors.Join(initializeAddressAdmission(ctx, db, expected, keyFingerprint, address), db.Close()); err != nil {
			return member, err
		}
		if err := syncAddressActivationTree(ctx, target.Dir); err != nil {
			return member, err
		}
		digest, err = addressActivationDigest(ctx, target.Dir)
		if err != nil {
			return member, err
		}
		if err := addressActivationCheckpoint(checkpoint, "candidate"); err != nil {
			return member, err
		}
		ready = addressActivationReady{Candidate: target.Dir, SourceDigest: intent.SourceDigest, CandidateDigest: digest, Member: member}
		if err := writeAddressActivation(readyPath, ready); err != nil {
			return member, err
		}
	} else if err != nil {
		return member, err
	}
	if filepath.Dir(filepath.Dir(ready.Candidate)) != intent.Operation || filepath.Base(ready.Candidate) != "database" || !strings.HasPrefix(filepath.Base(filepath.Dir(ready.Candidate)), "attempt-") || ready.SourceDigest != intent.SourceDigest || len(ready.CandidateDigest) != 64 || ready.Member.ID == 0 || ready.Member.Address != address || ready.Member.Role != "voter" {
		return member, errors.New("invalid address activation candidate record")
	}
	if info, err := os.Lstat(filepath.Dir(ready.Candidate)); err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return member, errors.New("candidate attempt must remain a private directory")
	}
	if err := addressActivationCheckpoint(checkpoint, "ready"); err != nil {
		return member, err
	}
	return publishAddressActivation(ctx, path, intent, ready, checkpoint)
}

func verifyAddressActivation(ctx context.Context, db *cluster.Database, expected store.ClusterIdentity) error {
	if id, pin := db.TransportIdentity(); id != "" && (id != expected.ClusterID || pin != expected.CAFingerprint) {
		return store.ErrClusterIdentityConflict
	}
	st, err := store.OpenReplicated(ctx, db.SQL, db.Scheduling, bus.New(), store.ReplicatedBackend())
	if err != nil {
		return err
	}
	// The caller owns db and closes both pools plus native transport after this
	// read-only check. No migration, default seeding or recovery runs here.
	return st.CheckClusterCompatibility(ctx, expected)
}

// The established seed participates in the same durable authorization set as
// invited members. This explicit offline write is never part of normal startup.
func initializeAddressAdmission(ctx context.Context, db *cluster.Database, expected store.ClusterIdentity, key, address string) error {
	if db.TransportMemberKey() != key {
		return store.ErrClusterAdmissionRejected
	}
	if err := verifyAddressActivation(ctx, db, expected); err != nil {
		return err
	}
	st, err := store.OpenReplicated(ctx, db.SQL, db.Scheduling, bus.New(), store.ReplicatedBackend())
	if err != nil {
		return err
	}
	_, err = st.InitializeClusterSeedAdmission(ctx, expected.ClusterID, key, address)
	return err
}

func addressActivationCheckpoint(checkpoint func(string) error, phase string) error {
	if checkpoint != nil {
		return checkpoint(phase)
	}
	return nil
}

func publishAddressActivation(ctx context.Context, path string, intent addressActivationIntent, ready addressActivationReady, checkpoint func(string) error) (cluster.Member, error) {
	active, err := addressActivationDigest(ctx, path+".dqlite")
	if err != nil {
		return cluster.Member{}, err
	}
	candidate, err := addressActivationDigest(ctx, ready.Candidate)
	if err != nil {
		return cluster.Member{}, err
	}
	switch {
	case active == ready.SourceDigest && candidate == ready.CandidateDigest:
		if err := ctx.Err(); err != nil {
			return cluster.Member{}, err
		}
		// Both paths live on the same filesystem. Exchange is atomic: recovery
		// sees either complete layout, never an absent active database directory.
		if err := unix.Renameat2(unix.AT_FDCWD, path+".dqlite", unix.AT_FDCWD, ready.Candidate, unix.RENAME_EXCHANGE); err != nil {
			return cluster.Member{}, err
		}
	case active == ready.CandidateDigest && candidate == ready.SourceDigest:
		// Resume a completed exchange whose directory sync/receipt was interrupted.
	default:
		return cluster.Member{}, errors.New("unknown address activation layout; original and candidate retained without modification")
	}
	if err := syncEnrollmentDirectory(filepath.Dir(path)); err != nil {
		return cluster.Member{}, err
	}
	if err := syncEnrollmentDirectory(filepath.Dir(ready.Candidate)); err != nil {
		return cluster.Member{}, err
	}
	if err := addressActivationCheckpoint(checkpoint, "exchange"); err != nil {
		return cluster.Member{}, err
	}
	if err := writeAddressActivation(filepath.Join(intent.Operation, "complete.json"), ready); err != nil {
		return cluster.Member{}, err
	}
	if err := addressActivationCheckpoint(checkpoint, "complete"); err != nil {
		return cluster.Member{}, err
	}
	if err := os.Remove(path + ".maintenance"); err != nil {
		return cluster.Member{}, err
	}
	if err := syncEnrollmentDirectory(filepath.Dir(path)); err != nil {
		return cluster.Member{}, err
	}
	if err := addressActivationCheckpoint(checkpoint, "unblock"); err != nil {
		return cluster.Member{}, err
	}
	return ready.Member, nil
}

// Verification may create new Raft segments after the conversion helper's
// original sync. Flush that final closed generation before binding its digest.
func syncAddressActivationTree(ctx context.Context, root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("activation candidate contains a link or special file")
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		return errors.Join(file.Sync(), file.Close())
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syncEnrollmentDirectory(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}
