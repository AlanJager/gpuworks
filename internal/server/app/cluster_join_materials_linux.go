//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A single atomic snapshot binds the issued member certificate to the private
// cluster material. It excludes the independently persisted local member key.
type joinMaterialSnapshot struct {
	CertificatePEM string                      `json:"certificate_pem"`
	Secrets        enrollment.BootstrapSecrets `json:"secrets"`
	// StorageTier is the tier the cluster told this member when it issued
	// the certificate, as storagetier records it (absent for standard). The
	// member starts its database node with it and records it for good with
	// its enrollment configuration.
	StorageTier string `json:"storage_tier,omitempty"`
}

// ReceiveClusterMaterials retrieves and durably records cryptographic material.
// It never clears join guards, starts Raft, or claims resource-asset readiness.
// A failed exchange can recover with the same persisted key, never a new one.
func ReceiveClusterMaterials(ctx context.Context, cfg Config, target ClusterJoinTarget, token string) (result ClusterJoinInfo, resultErr error) {
	prepared, err := PrepareClusterJoin(ctx, cfg, target)
	if err != nil {
		return result, err
	}
	root, err := filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return result, err
	}
	path, release, err := store.LockRuntimeDatabase(filepath.Join(root, "gpuworks.db"))
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	if err := store.CheckRestoreCompleted(root); err != nil {
		return result, err
	}
	current, err := finishClusterJoin(path, prepared.ClusterJoinTarget)
	if err != nil {
		return result, err
	}
	if current != prepared {
		return result, errors.New("join identity changed before material transfer")
	}
	dir := path + ".join"
	key, err := identity.LoadMemberKey(filepath.Join(dir, "member.key"))
	if err != nil {
		return result, err
	}
	if found, err := recoverJoinMaterials(dir, prepared, key); err != nil {
		return result, err
	} else if found {
		return prepared, nil
	}
	client, err := enrollment.NewClient(prepared.EnrollmentURL, prepared.ClusterID, prepared.CAFingerprint, prepared.Protocol)
	if err != nil {
		return result, err
	}
	defer client.Close()
	certificate, err := client.RecoverForOrigin(ctx, key, prepared.Address, prepared.EnrollmentOrigin)
	if err != nil && token != "" && ctx.Err() == nil {
		certificate, err = client.ExchangeWithOrigin(ctx, token, key, prepared.Address, prepared.EnrollmentOrigin)
		if err != nil && ctx.Err() == nil {
			certificate, err = client.RecoverForOrigin(ctx, key, prepared.Address, prepared.EnrollmentOrigin)
		}
	}
	if err != nil {
		return result, err
	}
	// Every member elects on the cluster's timing, so a member adopts the
	// tier the cluster records and does not join one it does not know.
	tier, err := storagetier.Parse(certificate.StorageTier)
	if err != nil || tier.Recorded() != certificate.StorageTier {
		return result, fmt.Errorf("cluster runs storage tier %q, which this build cannot run; join with a build that knows it", certificate.StorageTier)
	}
	secrets, err := client.FetchSecrets(ctx, []byte(certificate.CertificatePEM), key)
	if err != nil {
		return result, err
	}
	snapshot := joinMaterialSnapshot{CertificatePEM: certificate.CertificatePEM, Secrets: secrets, StorageTier: tier.Recorded()}
	if err := publishJoinMaterials(ctx, dir, snapshot, prepared, key); err != nil {
		return result, err
	}
	return prepared, nil
}

func validateJoinMaterials(snapshot joinMaterialSnapshot, info ClusterJoinInfo, key []byte) error {
	if tier, err := storagetier.Parse(snapshot.StorageTier); err != nil || tier.Recorded() != snapshot.StorageTier {
		return errors.New("join materials name an unknown storage tier")
	}
	secret := snapshot.Secrets
	if secret.ClusterID != info.ClusterID || secret.Protocol != info.Protocol || len(secret.MasterSecret) == 0 || len(secret.MasterSecret) > 64<<10 {
		return errors.New("join materials differ from expected cluster")
	}
	now := time.Now().UTC()
	if _, err := identity.LoadAuthority([]byte(secret.CACertificatePEM), []byte(secret.CAKeyPEM), info.ClusterID, info.CAFingerprint, now); err != nil {
		return errors.New("join authority material rejected")
	}
	trust, err := identity.NewTrust([]byte(secret.CACertificatePEM), info.ClusterID, info.CAFingerprint, now)
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair([]byte(snapshot.CertificatePEM), key)
	if err != nil || len(pair.Certificate) != 1 {
		return errors.New("join certificate does not match original member key")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return errors.New("invalid join member certificate")
	}
	fp, err := trust.VerifyMember(leaf, now)
	if err != nil || fp != info.KeyFingerprint {
		return errors.New("join certificate identity rejected")
	}
	host, _, err := net.SplitHostPort(info.Address)
	if err != nil || leaf.VerifyHostname(host) != nil {
		return errors.New("join certificate address rejected")
	}
	if info.EnrollmentOrigin != "" {
		publicURL, _ := url.Parse(info.EnrollmentOrigin)
		if leaf.VerifyHostname(publicURL.Hostname()) != nil {
			return errors.New("join certificate public origin rejected")
		}
	}
	return nil
}

func readJoinMaterialFile(path string, limit int64) ([]byte, error) {
	stat, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 || stat.Size() <= 0 || stat.Size() > limit {
		return nil, errors.New("join material must be a bounded private regular file")
	}
	return os.ReadFile(path)
}

func loadJoinMaterialSnapshot(dir string, info ClusterJoinInfo, key []byte, wantDigest string) (joinMaterialSnapshot, error) {
	var snapshot joinMaterialSnapshot
	stat, err := os.Lstat(dir)
	if err != nil {
		return snapshot, err
	}
	if !stat.IsDir() || stat.Mode().Perm()&0077 != 0 {
		return snapshot, errors.New("join material directory must be private and not a link")
	}
	raw, err := readJoinMaterialFile(filepath.Join(dir, "bundle.json"), 2<<20)
	if err != nil {
		return snapshot, err
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != wantDigest {
		return snapshot, errors.New("join material snapshot differs from its committed digest")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return joinMaterialSnapshot{}, errors.New("invalid join material snapshot")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return joinMaterialSnapshot{}, errors.New("trailing join material snapshot")
	}
	if err := validateJoinMaterials(snapshot, info, key); err != nil {
		return joinMaterialSnapshot{}, err
	}
	return snapshot, nil
}

// The durable intent names one candidate. Losing the published snapshot must
// not turn a retry into a new download or silently replace the master secret.
func recoverJoinMaterials(dir string, info ClusterJoinInfo, key []byte) (bool, error) {
	marker := filepath.Join(dir, "materials.commit")
	raw, err := readJoinMaterialFile(marker, 256)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(filepath.Join(dir, "materials")); !errors.Is(err, os.ErrNotExist) {
			return false, errors.New("material snapshot exists without its commit identity")
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	fields := strings.Split(string(raw), "\n")
	if len(fields) != 3 || fields[2] != "" || !strings.HasPrefix(fields[0], ".materials-") || filepath.Base(fields[0]) != fields[0] || len(fields[1]) != 64 {
		return false, errors.New("invalid join material commit identity")
	}
	if _, err := hex.DecodeString(fields[1]); err != nil {
		return false, errors.New("invalid material digest")
	}
	destination := filepath.Join(dir, "materials")
	if _, err := os.Lstat(destination); errors.Is(err, os.ErrNotExist) {
		candidate := filepath.Join(dir, fields[0])
		if _, err := loadJoinMaterialSnapshot(candidate, info, key, fields[1]); err != nil {
			return false, err
		}
		if err := unix.Renameat2(unix.AT_FDCWD, candidate, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	if _, err := loadJoinMaterialSnapshot(destination, info, key, fields[1]); err != nil {
		return false, err
	}
	if err := syncEnrollmentDirectory(dir); err != nil {
		return false, err
	}
	return true, nil
}

func publishJoinMaterials(ctx context.Context, dir string, snapshot joinMaterialSnapshot, info ClusterJoinInfo, key []byte) error {
	if err := validateJoinMaterials(snapshot, info, key); err != nil {
		return err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	candidate, err := os.MkdirTemp(dir, ".materials-")
	if err != nil {
		return err
	}
	// Retain interrupted candidates for diagnosis; only the durable commit file
	// can select one for installation. Retries never publish an arbitrary stage.
	if err := writeEnrollmentFile(filepath.Join(candidate, "bundle.json"), raw); err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	marker := []byte(filepath.Base(candidate) + "\n" + hex.EncodeToString(digest[:]) + "\n")
	if err := writeEnrollmentFile(filepath.Join(candidate, "commit"), marker); err != nil {
		return err
	}
	if err := syncEnrollmentDirectory(candidate); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, filepath.Join(candidate, "commit"), unix.AT_FDCWD, filepath.Join(dir, "materials.commit"), unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err := syncEnrollmentDirectory(dir); err != nil {
		return err
	}
	_, err = recoverJoinMaterials(dir, info, key)
	return err
}
