//go:build linux && dqlite

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func enrollmentPlatform() error { return nil }

// InitializeEnrollment runs only against a stopped existing single-member
// runtime. Candidate material is durable before the database identity commit;
// interrupted retries reuse it and never replace a different cluster identity.
func InitializeEnrollment(ctx context.Context, cfg Config, listen, publicURL string) (EnrollmentInfo, error) {
	return InitializeEnrollmentWithTier(ctx, cfg, listen, publicURL, storagetier.Standard)
}

// InitializeEnrollmentWithTier is InitializeEnrollment for a cluster whose
// members run the given storage tier. The tier is recorded once with the
// enrollment configuration; repeating the initialization with another tier is
// refused like any other difference.
func InitializeEnrollmentWithTier(ctx context.Context, cfg Config, listen, publicURL string, tier storagetier.Tier) (result EnrollmentInfo, resultErr error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, err := storagetier.Parse(string(tier)); err != nil {
		return result, err
	}
	publicAddress, err := validateEnrollmentAddresses(listen, publicURL)
	if err != nil {
		return result, err
	}
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.DataDir, "gpuworks.db")
	}
	if err := store.CheckRestoreCompleted(cfg.DataDir); err != nil {
		return result, err
	}
	dirInfo, err := os.Stat(cfg.DataDir)
	if err != nil {
		return result, err
	}
	owner, ok := dirInfo.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return result, errors.New("run cluster-init as the existing data directory owner")
	}
	if info, err := os.Lstat(cfg.DBPath + ".dqlite"); err != nil || !info.IsDir() {
		return result, errors.New("cluster-init requires an existing native single-member database")
	}
	master, err := enrollmentMasterSecret(cfg.DataDir)
	if err != nil {
		return result, err
	}
	st, err := store.OpenExistingRuntime(ctx, cfg.DBPath, bus.New())
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, st.Close()) }()
	current, currentErr := st.ClusterIdentity(ctx)
	if currentErr != nil && !errors.Is(currentErr, store.ErrClusterNotInitialized) {
		return result, currentErr
	}
	bundle, err := readEnrollmentBundle(cfg.DataDir, true)
	if err != nil {
		return result, err
	}
	if bundle != nil && errors.Is(currentErr, store.ErrClusterNotInitialized) {
		if _, err := os.Lstat(filepath.Join(cfg.DataDir, enrollmentDirectory, ".pending")); err != nil {
			return result, errors.New("completed enrollment materials have no matching database identity; restore a coherent snapshot")
		}
	}
	if bundle == nil {
		if currentErr == nil {
			return result, errors.New("cluster identity exists but its enrollment materials are missing; restore them instead of regenerating")
		}
		clusterID := auth.NewToken(16)
		if clusterID == "" {
			return result, errors.New("generate cluster identity")
		}
		now := time.Now().UTC()
		ca, caKey, err := identity.NewAuthority(clusterID, now)
		if err != nil {
			return result, err
		}
		pin, err := identity.CertificateFingerprint(ca)
		if err != nil {
			return result, err
		}
		issuer, err := identity.LoadAuthority(ca, caKey, clusterID, pin, now)
		if err != nil {
			return result, err
		}
		key, err := identity.NewMemberKey()
		if err != nil {
			return result, err
		}
		csr, err := identity.NewMemberCSR(key)
		if err != nil {
			return result, err
		}
		fingerprint, err := identity.CSRKeyFingerprint(csr)
		if err != nil {
			return result, err
		}
		certificate, err := issuer.IssueMember(csr, fingerprint, publicAddress, now, identity.MaxMemberLifetime)
		if err != nil {
			return result, err
		}
		settings := EnrollmentInfo{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion, ListenAddress: listen, PublicURL: publicURL, StorageTier: tier.Recorded()}
		config, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return result, err
		}
		stage, err := os.MkdirTemp(cfg.DataDir, ".cluster-enrollment-")
		if err != nil {
			return result, err
		}
		// Failed candidates are retained for diagnosis; they are never loaded
		// as active configuration or used to reconstruct database identity.
		for name, data := range map[string][]byte{"config.json": config, "ca.pem": ca, "ca.key": caKey, "server.pem": certificate, "server.key": key, "master.binding": enrollmentMasterBinding(caKey, master), ".pending": []byte("repeat the same offline cluster-init to complete initialization\n")} {
			if err := writeEnrollmentFile(filepath.Join(stage, name), data); err != nil {
				return result, err
			}
		}
		if err := syncEnrollmentDirectory(stage); err != nil {
			return result, err
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, filepath.Join(cfg.DataDir, enrollmentDirectory), unix.RENAME_NOREPLACE); err != nil {
			return result, err
		}
		if err := syncEnrollmentDirectory(cfg.DataDir); err != nil {
			return result, err
		}
		bundle, err = readEnrollmentBundle(cfg.DataDir, true)
		if err != nil {
			return result, err
		}
	}
	if bundle.info.ListenAddress != listen || bundle.info.PublicURL != publicURL || bundle.info.StorageTier != tier.Recorded() {
		return result, errors.New("existing enrollment configuration differs; initialization never replaces it")
	}
	if currentErr == nil && (current.ClusterID != bundle.info.ClusterID || current.CAFingerprint != bundle.info.CAFingerprint || current.Protocol != bundle.info.Protocol) {
		return result, errors.New("enrollment materials differ from the existing database identity")
	}
	if _, err := st.InitializeClusterIdentity(ctx, store.ClusterIdentity{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Protocol: bundle.info.Protocol}); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	marker := filepath.Join(cfg.DataDir, enrollmentDirectory, ".pending")
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, fmt.Errorf("identity committed; repeat cluster-init to finish: %w", err)
	}
	if err := syncEnrollmentDirectory(filepath.Dir(marker)); err != nil {
		return result, err
	}
	return bundle.info, nil
}

func writeEnrollmentFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	return errors.Join(writeErr, file.Sync(), file.Close())
}

func syncEnrollmentDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
