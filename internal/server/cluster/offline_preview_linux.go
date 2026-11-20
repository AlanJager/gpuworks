//go:build linux && dqlite

package cluster

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/canonical/go-dqlite/v3/client"
	"github.com/canonical/go-dqlite/v3/driver"
	"golang.org/x/sys/unix"
)

const maxOfflineDatabaseBytes = 1 << 30

// ExportOfflineSingleDatabase exports the canonical reopened view of a cold
// native single voter. scratchDir must be the caller's exclusively owned copy,
// never its original database. Native startup may change files inside scratch.
// The caller owns source hash verification and schema/material qualification.
// exportDir must not exist; an error never authorizes using its output.
// Run this function only in a disposable worker process and join that process
// before scratch cleanup: upstream failed-start bindings cannot release every
// native owner in process. Successful export still requires actual Node.Close.
func ExportOfflineSingleDatabase(ctx context.Context, scratchDir, name string, expected Member, exportDir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	scratchDir, exportDir, err := offlinePreviewPaths(scratchDir, name, expected, exportDir)
	if err != nil {
		return err
	}
	if err := filepath.WalkDir(scratchDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		stat, err := entry.Info()
		if err != nil {
			return err
		}
		if !stat.Mode().IsRegular() {
			return fmt.Errorf("offline preview copy contains a linked or special file: %s", path)
		}
		return ctx.Err()
	}); err != nil {
		return err
	}
	info, err := conversionIdentity(scratchDir)
	if err != nil {
		return err
	}
	if info.ID != expected.ID || info.Address != expected.Address {
		return errors.New("offline preview identity differs from the expected member")
	}
	metadata := false
	for _, name := range []string{"metadata1", "metadata2"} {
		stat, err := os.Lstat(filepath.Join(scratchDir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !stat.Mode().IsRegular() || stat.Size() != 32 {
			return errors.New("offline preview requires complete supported Raft metadata")
		}
		metadata = true
	}
	if !metadata {
		return errors.New("offline preview refuses an uninitialized native directory")
	}
	if err := offlinePreviewPreserveSegments(ctx, scratchDir); err != nil {
		return err
	}
	files, err := exportOfflineNativeFiles(ctx, scratchDir, name, info)
	if err != nil {
		return err
	}
	return publishOfflineNativeFiles(ctx, exportDir, name, files)
}

func offlinePreviewPaths(scratchDir, name string, expected Member, exportDir string) (string, string, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsRune(name, 0) {
		return "", "", errors.New("offline preview requires a simple database name")
	}
	if expected.ID == 0 || expected.Address == "" || strings.ContainsRune(expected.Address, 0) || expected.Role != "voter" {
		return "", "", errors.New("offline preview requires the exact existing voter")
	}
	paths := []string{scratchDir, exportDir}
	for i, value := range paths {
		if !filepath.IsAbs(value) || filepath.Clean(value) == string(filepath.Separator) {
			return "", "", errors.New("offline preview requires absolute private directories")
		}
		absolute := filepath.Clean(value)
		parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
		if err != nil {
			return "", "", err
		}
		paths[i] = filepath.Join(parent, filepath.Base(absolute))
	}
	for i, left := range paths {
		right := paths[1-i]
		relative, err := filepath.Rel(left, right)
		if err != nil {
			return "", "", err
		}
		if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return "", "", errors.New("offline preview native and export directories must not overlap")
		}
	}
	stat, err := os.Lstat(paths[0])
	if err != nil {
		return "", "", err
	}
	if !stat.IsDir() || stat.Mode()&os.ModeSymlink != 0 {
		return "", "", errors.New("offline preview native copy must be a regular directory")
	}
	if _, err := os.Lstat(paths[1]); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = errors.New("offline preview export directory already exists")
		}
		return "", "", err
	}
	return paths[0], paths[1], nil
}

func exportOfflineNativeFiles(ctx context.Context, dir, name string, info client.NodeInfo) (_ []client.File, resultErr error) {
	socket, closeNode, err := openIsolatedNode(ctx, info.ID, info.Address, dir, "@", "offline preview rejects every outbound Raft connection")
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeNode()) }()
	if !strings.HasPrefix(socket, "@") || len(socket) < 2 {
		return nil, errors.New("offline preview native node did not bind a private Unix socket")
	}
	dial := func(dialCtx context.Context, requested string) (net.Conn, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if requested != info.Address {
			return nil, errors.New("offline preview rejects a different member address")
		}
		conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", socket)
		if err != nil {
			return nil, err
		}
		bound := &offlinePreviewConnection{Conn: conn}
		bound.stop = context.AfterFunc(ctx, func() { _ = bound.closeSocket() })
		return bound, nil
	}
	c, err := client.New(ctx, info.Address, client.WithDialFunc(dial))
	if err != nil {
		return nil, errors.Join(err, ctx.Err())
	}
	defer func() { resultErr = errors.Join(resultErr, c.Close()) }()
	members, err := c.Cluster(ctx)
	if err != nil {
		return nil, errors.Join(err, ctx.Err())
	}
	if len(members) != 1 || members[0].ID != info.ID || members[0].Address != info.Address || members[0].Role != client.Voter {
		return nil, errors.New("offline preview refuses actual multi-member or different Raft membership")
	}
	seeds := client.NewInmemNodeStore()
	if err := seeds.Set(ctx, []client.NodeInfo{members[0]}); err != nil {
		return nil, err
	}
	d, err := driver.New(seeds, driver.WithDialFunc(dial))
	if err != nil {
		return nil, err
	}
	connector, err := d.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	defer func() { resultErr = errors.Join(resultErr, db.Close()) }()
	// This goes through the real leader read barrier, unlike Client.Dump.
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&count); err != nil {
		return nil, errors.Join(err, ctx.Err())
	}
	if count == 0 {
		return nil, errors.New("offline preview refuses an empty application database")
	}
	files, err := c.Dump(ctx, name)
	if err != nil {
		return nil, errors.Join(err, ctx.Err())
	}
	if err := validateOfflineNativeFiles(name, files); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return files, nil
}

// Cancellation and SQL/client cleanup share the actual once-only socket close.
// This transient owner prevents cancellation from returning ahead of Close.
type offlinePreviewConnection struct {
	net.Conn
	stop     func() bool
	once     sync.Once
	closeErr error
}

func (c *offlinePreviewConnection) closeSocket() error {
	c.once.Do(func() { c.closeErr = c.Conn.Close() })
	return c.closeErr
}

func (c *offlinePreviewConnection) Close() error {
	c.stop()
	return c.closeSocket()
}

func validateOfflineNativeFiles(name string, files []client.File) error {
	if len(files) != 2 {
		return errors.New("offline preview dump must contain exactly database and WAL")
	}
	seen := map[string]bool{}
	total := int64(0)
	for _, file := range files {
		if (file.Name != name && file.Name != name+"-wal") || seen[file.Name] {
			return errors.New("offline preview dump contains an unexpected or duplicate filename")
		}
		seen[file.Name] = true
		total += int64(len(file.Data))
		if total > maxOfflineDatabaseBytes {
			return errors.New("offline preview dump exceeds the supported byte limit")
		}
		if file.Name == name && (len(file.Data) < 100 || !bytes.Equal(file.Data[:16], []byte("SQLite format 3\x00"))) {
			return errors.New("offline preview dump is not a SQLite database")
		}
	}
	return nil
}

func publishOfflineNativeFiles(ctx context.Context, target, name string, files []client.File) (resultErr error) {
	if err := validateOfflineNativeFiles(name, files); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(target), ".native-preview-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			resultErr = errors.Join(resultErr, os.RemoveAll(stage))
		}
	}()
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(stage, file.Name)
		output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		written, writeErr := output.Write(file.Data)
		if writeErr == nil && written != len(file.Data) {
			writeErr = io.ErrShortWrite
		}
		syncErr := error(nil)
		if writeErr == nil {
			syncErr = output.Sync()
		}
		if err := errors.Join(writeErr, syncErr, output.Close()); err != nil {
			return err
		}
		actual, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(actual, file.Data) {
			return errors.New("offline preview export readback differs")
		}
	}
	if err := syncConversionDirectory(stage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	published = true
	if err := syncConversionDirectory(filepath.Dir(target)); err != nil {
		return fmt.Errorf("offline preview export published but parent sync failed: %w", err)
	}
	return ctx.Err()
}
