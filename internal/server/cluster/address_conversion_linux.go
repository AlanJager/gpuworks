//go:build linux && dqlite

package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
	"gpuworks.ai/gpuworks/internal/filelock"
)

// PrepareSingleMemberAddress produces a verified, closed candidate at target.Dir.
// sourcePath is the runtime database path, whose native files live at the
// .dqlite sibling. The source must be stopped; this function owns its runtime
// lock until return. It never replaces the source or starts application work.
//
// The target requires mutual TLS, no Seeds, and a different address. backupDir
// and target.Dir must not exist. A cold copy remains at backupDir/database even
// on failure. Installation/configuration switching is the caller's separate
// maintenance operation; the original and candidate must never run together.
func PrepareSingleMemberAddress(ctx context.Context, sourcePath, backupDir string, target DatabaseConfig) (member Member, resultErr error) {
	var err error
	sourcePath, backupDir, target, err = validateAddressPreparation(ctx, sourcePath, backupDir, target)
	if err != nil {
		return member, err
	}
	release, err := filelock.Acquire(sourcePath + ".runtime.lock")
	if err != nil {
		return member, fmt.Errorf("stop the source runtime before address conversion: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	return prepareSingleMemberAddress(ctx, sourcePath, backupDir, target, nil)
}

// PrepareSingleMemberAddressUnderLease prepares the same closed candidate while
// its caller continuously owns the canonical source runtime lease. The caller
// must retain that lease until activation or explicit maintenance recovery ends.
// Only the exact nonempty, durably published maintenance intent is accepted.
func PrepareSingleMemberAddressUnderLease(ctx context.Context, sourcePath, backupDir string, target DatabaseConfig, maintenance []byte) (Member, error) {
	if len(maintenance) == 0 {
		return Member{}, errors.New("held-lease address conversion requires an explicit maintenance intent")
	}
	var err error
	sourcePath, backupDir, target, err = validateAddressPreparation(ctx, sourcePath, backupDir, target)
	if err != nil {
		return Member{}, err
	}
	return prepareSingleMemberAddress(ctx, sourcePath, backupDir, target, maintenance)
}

func validateAddressPreparation(ctx context.Context, sourcePath, backupDir string, target DatabaseConfig) (string, string, DatabaseConfig, error) {
	if err := ctx.Err(); err != nil {
		return "", "", target, err
	}
	if err := target.validate(); err != nil {
		return "", "", target, err
	}
	if target.ListenTLS == nil || len(target.Seeds) != 0 {
		return "", "", target, errors.New("address conversion requires mutual TLS and no joining seeds")
	}
	var err error
	sourcePath, backupDir, target.Dir, err = conversionPaths(sourcePath, backupDir, target.Dir)
	if err != nil {
		return "", "", target, err
	}
	if target.Name != filepath.Base(sourcePath) {
		return "", "", target, errors.New("address conversion must retain the database name")
	}
	return sourcePath, backupDir, target, nil
}

func prepareSingleMemberAddress(ctx context.Context, sourcePath, backupDir string, target DatabaseConfig, maintenance []byte) (member Member, resultErr error) {
	marker := sourcePath + ".maintenance"
	if maintenance == nil {
		if _, err := os.Lstat(marker); err == nil {
			return member, errors.New("address conversion refuses incomplete maintenance state")
		} else if !errors.Is(err, os.ErrNotExist) {
			return member, err
		}
	} else {
		actual, err := transportRead(marker)
		if err != nil {
			return member, err
		}
		if !bytes.Equal(actual, maintenance) {
			return member, errors.New("address conversion maintenance intent differs from the caller's durable intent")
		}
	}
	for _, path := range []string{sourcePath, sourcePath + ".join", filepath.Join(filepath.Dir(sourcePath), ".restore-in-progress")} {
		if _, err := os.Lstat(path); err == nil {
			return member, fmt.Errorf("address conversion refuses existing legacy, join or incomplete restore state: %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return member, err
		}
	}
	sourceDir := sourcePath + ".dqlite"
	info, err := conversionIdentity(sourceDir)
	if err != nil {
		return member, err
	}
	host, _, err := net.SplitHostPort(info.Address)
	if err != nil || !net.ParseIP(host).IsLoopback() || target.Address == info.Address {
		return member, errors.New("address conversion requires an existing loopback single member and a different target address")
	}
	// Reject a running source and prevent a plain source restart while its cold
	// files are copied. Verification below uses an isolated Unix endpoint.
	reservation, err := net.Listen("tcp", info.Address)
	if err != nil {
		return member, fmt.Errorf("source address is active; stop the source before conversion: %w", err)
	}
	defer reservation.Close()
	if err := os.Mkdir(backupDir, 0700); err != nil {
		return member, err
	}
	backupData := filepath.Join(backupDir, "database")
	files, err := copyConversionDirectory(ctx, sourceDir, backupData)
	if err != nil {
		return member, fmt.Errorf("cold backup incomplete at %s: %w", backupDir, err)
	}
	if err := writeConversionManifest(backupDir, info, target.Address, files); err != nil {
		return member, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(target.Dir), ".address-conversion-")
	if err != nil {
		return member, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("address conversion failed; source unchanged, cold backup at %s, candidate retained at %s: %w", backupDir, staging, resultErr)
		}
	}()
	candidate := filepath.Join(staging, "database")
	if _, err := copyConversionDirectory(ctx, backupData, candidate); err != nil {
		return member, err
	}
	schema, err := inspectIsolatedConversionSource(ctx, candidate, target.Name, info)
	if err != nil {
		return member, err
	}
	if err := ctx.Err(); err != nil {
		return member, err
	}
	// The public dqlite API changes the persisted Raft configuration. Updating
	// the application's YAML files alone would leave the old Raft address.
	updated := client.NodeInfo{ID: info.ID, Address: target.Address, Role: client.Voter}
	if err := reconfigureSingleVoter(ctx, candidate, updated); err != nil {
		return member, fmt.Errorf("reconfigure cold single-member Raft state: %w", err)
	}
	for name, value := range map[string]any{"info.yaml": updated, "cluster.yaml": []client.NodeInfo{updated}} {
		payload, err := yaml.Marshal(value)
		if err != nil {
			return member, err
		}
		if err := writeConversionFile(filepath.Join(candidate, name), payload); err != nil {
			return member, err
		}
	}
	checking := target
	checking.Dir = candidate
	db, err := OpenDatabase(ctx, checking)
	if err != nil {
		return member, fmt.Errorf("verify converted address and mutual TLS: %w", err)
	}
	verifyErr := verifyConvertedDatabase(ctx, db, updated, schema)
	if err := errors.Join(verifyErr, db.Close()); err != nil {
		return member, err
	}
	if err := ctx.Err(); err != nil {
		return member, err
	}
	if err := syncConversionTree(ctx, candidate); err != nil {
		return member, err
	}
	// No empty output directory is exposed, even if the process dies during
	// publication. An existing destination is never replaced.
	if err := ctx.Err(); err != nil {
		return member, err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, candidate, unix.AT_FDCWD, target.Dir, unix.RENAME_NOREPLACE); err != nil {
		return member, fmt.Errorf("publish conversion candidate without replacement: %w", err)
	}
	if err := syncConversionDirectory(filepath.Dir(target.Dir)); err != nil {
		return member, fmt.Errorf("candidate published at %s but directory sync failed: %w", target.Dir, err)
	}
	_ = os.Remove(staging)
	return Member{ID: updated.ID, Address: updated.Address, Role: updated.Role.String()}, nil
}

func conversionSchema(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, "SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_schema ORDER BY type, name")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	hash := sha256.New()
	count := 0
	for rows.Next() {
		var row [4]string
		if err := rows.Scan(&row[0], &row[1], &row[2], &row[3]); err != nil {
			return "", err
		}
		if err := json.NewEncoder(hash).Encode(row); err != nil {
			return "", err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if count == 0 {
		return "", errors.New("address conversion refuses an empty application database")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyConvertedDatabase(ctx context.Context, db *Database, expected client.NodeInfo, schema string) error {
	members, err := db.Members(ctx)
	if err != nil {
		return err
	}
	if db.ID != expected.ID || len(members) != 1 || members[0].ID != expected.ID || members[0].Address != expected.Address || members[0].Role != "voter" {
		return fmt.Errorf("converted member identity or Raft address mismatch: %+v", members)
	}
	actual, err := conversionSchema(ctx, db.SQL)
	if err != nil {
		return err
	}
	if actual != schema {
		return errors.New("converted database schema differs from the cold source")
	}
	var integrity string
	if err := db.SQL.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("converted database integrity check failed: %s", integrity)
	}
	return nil
}

func writeConversionManifest(dir string, member client.NodeInfo, target string, files map[string]conversionFile) error {
	payload, err := json.MarshalIndent(struct {
		Version int                       `json:"version"`
		Reason  string                    `json:"reason"`
		At      time.Time                 `json:"created_at"`
		Source  client.NodeInfo           `json:"source"`
		Target  string                    `json:"target_address"`
		Files   map[string]conversionFile `json:"files"`
	}{1, "single-member-address-conversion", time.Now().UTC(), member, target, files}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeConversionFile(filepath.Join(dir, "manifest.json"), payload); err != nil {
		return err
	}
	return syncConversionDirectory(filepath.Dir(dir))
}
