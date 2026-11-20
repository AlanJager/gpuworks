//go:build linux && dqlite

package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	_ "github.com/glebarez/go-sqlite"
	"golang.org/x/sys/unix"
)

func TestOfflineSingleExportRealReplayKeepsOriginalAndAvoidsTCP(t *testing.T) {
	requireDatabaseHelper(t)
	if raw := os.Getenv("GPUWORKS_OFFLINE_EXPORT_TEST_CHILD"); raw != "" {
		offlinePreviewTestWorker(raw)
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	parent := t.TempDir()
	path, member := conversionSource(t, ctx, filepath.Join(parent, "original"))
	// These fewer-than-1024 entries are below the default snapshot threshold.
	// Preserve the original closed native bytes rather than opening that source.
	original := path + ".dqlite"
	before := offlinePreviewTestHashes(t, original)
	scratch := filepath.Join(parent, "scratch")
	if _, err := copyConversionDirectory(ctx, original, scratch); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", member.Address)
	if err != nil {
		t.Fatal("old native address must remain independently usable", err)
	}
	accepted := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if conn != nil {
			_ = conn.Close()
		}
		accepted <- err
	}()
	output := filepath.Join(parent, "export")
	err = offlinePreviewTestExport(ctx, scratch, "gpuworks.db", member, output)
	if closeErr := listener.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	acceptErr := <-accepted
	if acceptErr == nil {
		t.Fatal("preview contacted the original TCP endpoint")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, offlinePreviewTestHashes(t, original)) {
		t.Fatal("preview changed original cold native bytes")
	}
	sqlite := url.URL{Scheme: "file", Path: filepath.Join(output, "gpuworks.db")}
	query := url.Values{"mode": {"ro"}, "immutable": {"1"}, "_pragma": {"query_only(1)"}}
	sqlite.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", sqlite.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var uuid, token, created string
	var blob []byte
	if err := db.QueryRowContext(ctx, `SELECT uuid,token_hash,created_at,data FROM resources WHERE id=1`).Scan(&uuid, &token, &created, &blob); err != nil {
		t.Fatal(err)
	}
	if uuid != "uuid-before" || token != "token-hash-before" || created != "2026-01-02T03:04:05Z" || !bytes.Equal(blob, []byte{0, 'a', 'b', 255}) {
		t.Fatal("native logtail export lost actual row bytes")
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT version FROM schema_migrations`).Scan(&version); err != nil || version != 17 {
		t.Fatalf("native schema marker %d: %v", version, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM resources`); err == nil {
		t.Fatal("export reader allowed SQL writes")
	}
	for _, name := range []string{"gpuworks.db", "gpuworks.db-wal"} {
		stat, err := os.Stat(filepath.Join(output, name))
		if err != nil || stat.Mode().Perm() != 0600 {
			t.Fatalf("export file %s mode: %v %v", name, stat, err)
		}
	}
	// Actual native owner was released before export returned.
	if err := describeLastRaftEntry(ctx, scratch); err != nil {
		t.Fatal("export retained the native directory lock", err)
	}
}

func TestOfflineSingleExportRejectsCachedSingleWithActualMultiMember(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	parent := t.TempDir()
	path, member := conversionSource(t, ctx, filepath.Join(parent, "original"))
	scratch := filepath.Join(parent, "scratch")
	if _, err := copyConversionDirectory(ctx, path+".dqlite", scratch); err != nil {
		t.Fatal(err)
	}
	// Only scratch persisted Raft membership changes. The YAML cache remains one.
	// The database process rewrites the persisted configuration; this process
	// holds no native code that could.
	if _, err := runHelper(ctx, "reconfigure", "--dir", scratch,
		"--member", fmt.Sprintf("%d,%s,voter", member.ID, member.Address),
		"--member", fmt.Sprintf("%d,127.0.0.1:19099,voter", member.ID+1)); err != nil {
		if strings.Contains(err.Error(), "flag provided but not defined") {
			t.Skip("database process cannot write a multi-member configuration: ", err)
		}
		t.Fatal(err)
	}
	output := filepath.Join(parent, "export")
	if err := offlinePreviewTestExport(ctx, scratch, "gpuworks.db", member, output); err == nil {
		t.Fatal("YAML singleton hid actual multi-member membership")
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected multi-member export published: %v", err)
	}
}

func TestOfflineSingleExportRejectsBeforeStartingOrPublishing(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	parent := t.TempDir()
	path, member := conversionSource(t, ctx, filepath.Join(parent, "original"))
	original := path + ".dqlite"
	for _, mode := range []string{"cancel", "wrong-member", "existing-output", "overlap", "linked-file", "missing-metadata", "short-metadata", "corrupt-metadata"} {
		t.Run(mode, func(t *testing.T) {
			scratch := filepath.Join(t.TempDir(), "scratch")
			if _, err := copyConversionDirectory(ctx, original, scratch); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(filepath.Dir(scratch), "export")
			inputCtx := ctx
			expected := member
			switch mode {
			case "cancel":
				inputCtx, cancel = context.WithCancel(ctx)
				cancel()
			case "wrong-member":
				expected.ID++
			case "existing-output":
				if err := os.Mkdir(output, 0700); err != nil {
					t.Fatal(err)
				}
			case "overlap":
				output = filepath.Join(scratch, "export")
			case "linked-file":
				if err := os.Symlink(filepath.Join(original, "info.yaml"), filepath.Join(scratch, "unsafe")); err != nil {
					t.Fatal(err)
				}
			case "missing-metadata":
				for _, name := range []string{"metadata1", "metadata2"} {
					if err := os.Remove(filepath.Join(scratch, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
				}
			case "short-metadata":
				if err := os.WriteFile(filepath.Join(scratch, "metadata1"), make([]byte, 8), 0600); err != nil {
					t.Fatal(err)
				}
			case "corrupt-metadata":
				for _, name := range []string{"metadata1", "metadata2"} {
					if err := os.WriteFile(filepath.Join(scratch, name), bytes.Repeat([]byte{255}, 32), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := offlinePreviewTestHashes(t, original)
			err := offlinePreviewTestExport(inputCtx, scratch, "gpuworks.db", expected, output)
			if err == nil {
				t.Fatal("invalid native export succeeded")
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error: %v", err)
			}
			if !reflect.DeepEqual(before, offlinePreviewTestHashes(t, original)) {
				t.Fatal("failure changed original bytes")
			}
			if mode == "corrupt-metadata" {
				lock, lockErr := os.OpenFile(filepath.Join(scratch, "dqlite-lock"), os.O_CREATE|os.O_RDWR, 0600)
				if lockErr != nil {
					t.Fatal(lockErr)
				}
				if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatal("worker Wait did not release native process lock", err)
				}
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "existing-output" {
				if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failure published output: %v", err)
				}
			}
		})
	}
}

func TestOfflineSingleExportSocketCancellationWaitsActualClose(t *testing.T) {
	requireDatabaseHelper(t)
	local, peer := net.Pipe()
	defer peer.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	actual := &offlinePreviewHeldClose{Conn: local, entered: entered, release: release}
	ctx, cancel := context.WithCancel(t.Context())
	owner := &offlinePreviewConnection{Conn: actual}
	owner.stop = context.AfterFunc(ctx, func() { _ = owner.closeSocket() })
	cancel()
	<-entered
	returned := make(chan error, 1)
	go func() { returned <- owner.Close() }()
	select {
	case <-returned:
		t.Fatal("socket cleanup returned before actual Close")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-returned; err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if actual.closes != 1 {
		t.Fatalf("actual Close count %d", actual.closes)
	}
}

type offlinePreviewHeldClose struct {
	net.Conn
	entered, release chan struct{}
	closes           int
	mu               sync.Mutex
}

func (c *offlinePreviewHeldClose) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	err := c.Conn.Close()
	close(c.entered)
	<-c.release
	return err
}

func TestOfflineSingleExportDumpShapeAndPublication(t *testing.T) {
	requireDatabaseHelper(t)
	header := append([]byte("SQLite format 3\x00"), make([]byte, 84)...)
	healthy := []client.File{{Name: "gpuworks.db", Data: header}, {Name: "gpuworks.db-wal"}}
	for _, files := range [][]client.File{healthy[:1], {healthy[0], healthy[0]}, {{Name: "../gpuworks.db", Data: header}, healthy[1]}, {{Name: "gpuworks.db", Data: []byte("not sqlite")}, healthy[1]}} {
		if err := validateOfflineNativeFiles("gpuworks.db", files); err == nil {
			t.Fatalf("invalid dump shape accepted: %d files", len(files))
		}
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "export")
	if err := publishOfflineNativeFiles(t.Context(), target, "gpuworks.db", healthy); err != nil {
		t.Fatal(err)
	}
	before := offlinePreviewTestHashes(t, target)
	if err := publishOfflineNativeFiles(t.Context(), target, "gpuworks.db", healthy); err == nil {
		t.Fatal("export replaced existing directory")
	}
	if !reflect.DeepEqual(before, offlinePreviewTestHashes(t, target)) {
		t.Fatal("existing export changed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := publishOfflineNativeFiles(ctx, filepath.Join(parent, "cancelled"), "gpuworks.db", healthy); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled publication: %v", err)
	}
	stages, err := filepath.Glob(filepath.Join(parent, ".native-preview-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("unconfirmed stages: %v %v", stages, err)
	}
}

func offlinePreviewTestHashes(t *testing.T, dir string) map[string][32]byte {
	t.Helper()
	result := map[string][32]byte{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		result[relative] = sha256.Sum256(raw)
		return nil
	})
	if err != nil {
		t.Fatal(fmt.Errorf("hash directory: %w", err))
	}
	return result
}

func offlinePreviewTestExport(ctx context.Context, scratch, name string, member Member, output string) error {
	raw, err := json.Marshal(struct {
		Scratch string
		Name    string
		Member  Member
		Output  string
	}{scratch, name, member, output})
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOfflineSingleExportRealReplayKeepsOriginalAndAvoidsTCP$")
	command.Env = append(os.Environ(), "GPUWORKS_OFFLINE_EXPORT_TEST_CHILD="+string(raw))
	result, err := command.CombinedOutput() // Run includes actual Wait even after cancellation.
	if err != nil {
		return errors.Join(fmt.Errorf("native export worker failed: %w: %s", err, result), ctx.Err())
	}
	return nil
}

func offlinePreviewTestWorker(raw string) {
	var args struct {
		Scratch string
		Name    string
		Member  Member
		Output  string
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	err := ExportOfflineSingleDatabase(ctx, args.Scratch, args.Name, args.Member, args.Output)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
