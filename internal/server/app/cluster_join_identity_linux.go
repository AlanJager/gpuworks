//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// installJoinedEnrollmentIdentity derives this member's local enrollment
// identity from its committed Join snapshot. Source member private keys and
// source listener configuration are never installed on another member.
func (s *ClusterRaftSession) installJoinedEnrollmentIdentity(ctx context.Context, cfg Config) error {
	if s == nil || s.db == nil {
		return errors.New("joined enrollment installation requires a running member")
	}
	if s.info.EnrollmentOrigin == "" && s.info.EnrollmentListen == "" {
		return nil // Legacy members have no authorized public enrollment listener.
	}
	if s.info.EnrollmentOrigin == "" || s.info.EnrollmentListen == "" {
		return errors.New("joined enrollment listener identity is incomplete")
	}
	if err := validateJoinMaterials(s.snapshot, s.info, s.key); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	master := s.snapshot.Secrets.MasterSecret
	if configured := os.Getenv("GPUWORKS_MASTER_SECRET"); configured != "" && !bytes.Equal([]byte(configured), master) {
		return errors.New("configured master secret differs from joined cluster")
	}
	masterPath := filepath.Join(root, "master.key")
	if err := installJoinedMasterSecret(masterPath, master); err != nil {
		return err
	}
	info := EnrollmentInfo{ClusterID: s.info.ClusterID, CAFingerprint: s.info.CAFingerprint, Protocol: store.ClusterProtocolVersion, ListenAddress: s.info.EnrollmentListen, PublicURL: s.info.EnrollmentOrigin, StorageTier: s.snapshot.StorageTier}
	verify := func() error {
		bundle, err := readEnrollmentBundle(root, false)
		if err != nil || bundle == nil {
			return errors.Join(errors.New("joined enrollment identity is unavailable"), err)
		}
		if bundle.info != info || !bytes.Equal(bundle.ca, []byte(s.snapshot.Secrets.CACertificatePEM)) || !bytes.Equal(bundle.caKey, []byte(s.snapshot.Secrets.CAKeyPEM)) || bundle.master != string(master) {
			return errors.New("existing enrollment identity differs from joined member")
		}
		key, err := bundle.trust.VerifyMember(bundle.tls.Certificates[0].Leaf, time.Now().UTC())
		if err != nil || key != s.info.KeyFingerprint {
			return errors.New("enrollment certificate does not belong to joined member")
		}
		return nil
	}
	dest := filepath.Join(root, enrollmentDirectory)
	if _, err := os.Lstat(dest); err == nil {
		return verify()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	settings, err := json.Marshal(info)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(root, ".joined-enrollment-")
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"config.json":    settings,
		"ca.pem":         []byte(s.snapshot.Secrets.CACertificatePEM),
		"ca.key":         []byte(s.snapshot.Secrets.CAKeyPEM),
		"server.pem":     []byte(s.snapshot.CertificatePEM),
		"server.key":     s.key,
		"master.binding": enrollmentMasterBinding([]byte(s.snapshot.Secrets.CAKeyPEM), master),
	}
	for name, body := range files {
		if err := writeEnrollmentFile(filepath.Join(stage, name), body); err != nil {
			return fmt.Errorf("stage joined enrollment %s: %w", name, err)
		}
	}
	if err := syncEnrollmentDirectory(stage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, dest, unix.RENAME_NOREPLACE); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	if err := syncEnrollmentDirectory(root); err != nil {
		return err
	}
	return verify()
}

func installJoinedMasterSecret(path string, master []byte) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := writeEnrollmentFile(path, master); err != nil {
			return err
		}
		return syncEnrollmentDirectory(filepath.Dir(path))
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != int64(len(master)) {
		return errors.New("existing master secret is invalid or differs")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return errors.New("existing master secret changed before reading")
	}
	actual, err := io.ReadAll(io.LimitReader(file, int64(len(master)+1)))
	if err != nil || !bytes.Equal(actual, master) {
		return errors.New("existing master secret differs from joined cluster")
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return errors.New("existing master secret changed after reading")
	}
	return nil
}
