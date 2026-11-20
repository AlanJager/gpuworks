//go:build linux && dqlite

package cluster

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestOfflineSingleExportRejectsCorruptNonzeroOpenTail(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	original, member := offlineSegmentTestSource(t, ctx)
	before := offlinePreviewTestHashes(t, original)
	for _, corrupt := range []bool{false, true} {
		name := "healthy-open"
		if corrupt {
			name = "corrupt-final-committed-batch"
		}
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			scratch := filepath.Join(parent, "scratch")
			if _, err := copyConversionDirectory(ctx, original, scratch); err != nil {
				t.Fatal(err)
			}
			offlineSegmentTestUnfinalize(t, scratch, corrupt)
			output := filepath.Join(parent, "export")
			err := offlinePreviewTestExport(ctx, scratch, "gpuworks.db", member, output)
			if corrupt {
				if err == nil {
					prefix, tail := offlineSegmentTestRows(t, ctx, output)
					t.Fatalf("corrupt nonzero committed tail qualified: prefix=%d tail=%d (expected refusal)", prefix, tail)
				}
				if _, statErr := os.Lstat(output); !os.IsNotExist(statErr) {
					t.Fatalf("corrupt export published: %v", statErr)
				}
				t.Logf("actual corrupt-tail refusal: %v", err)
			} else {
				if err != nil {
					t.Fatal(err)
				}
				prefix, tail := offlineSegmentTestRows(t, ctx, output)
				if prefix != 1 || tail != 1 {
					t.Fatalf("healthy actual committed rows: %d %d", prefix, tail)
				}
			}
			if !reflect.DeepEqual(before, offlinePreviewTestHashes(t, original)) {
				t.Fatal("source bytes changed")
			}
		})
	}
}

func offlineSegmentTestSource(t *testing.T, ctx context.Context) (string, Member) {
	t.Helper()
	original := filepath.Join(t.TempDir(), "original")
	db, err := OpenSingleDatabase(ctx, original, "gpuworks.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY)`,
		`INSERT INTO schema_migrations VALUES(17)`,
		`CREATE TABLE records(id INTEGER PRIMARY KEY, value TEXT)`,
		`INSERT INTO records VALUES(1,'confirmed-prefix')`,
		`INSERT INTO records VALUES(2,'confirmed-final-tail')`,
	} {
		if _, err := db.SQL.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	member := Member{ID: db.ID, Address: db.Address, Role: "voter"}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return original, member
}

// Traverse only the real fixture's known format to locate its final batch.
// Production delegates CRC/format interpretation to the pinned native parser.
func offlineSegmentTestUnfinalize(t *testing.T, dir string, corrupt bool) []byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var closed []string
	for _, entry := range entries {
		name := entry.Name()
		if len(name) == 33 && name[16] == '-' && !strings.HasPrefix(name, "snapshot-") {
			closed = append(closed, name)
		}
	}
	sort.Strings(closed)
	if len(closed) == 0 {
		t.Fatal("real native source has no closed segment")
	}
	path := filepath.Join(dir, closed[len(closed)-1])
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 8 || binary.LittleEndian.Uint64(raw[:8]) != 1 {
		t.Fatal("fixture not actual supported format1")
	}
	offset, last, batches := 8, -1, 0
	for offset < len(raw) {
		start := offset
		if len(raw)-offset < 16 {
			t.Fatal("fixture truncated batch preamble")
		}
		n := binary.LittleEndian.Uint64(raw[offset+8 : offset+16])
		if n == 0 || n > uint64((len(raw)-offset-16)/16) {
			t.Fatal("fixture invalid batch count")
		}
		header := offset + 16
		payload := uint64(0)
		for i := uint64(0); i < n; i++ {
			payload += uint64(binary.LittleEndian.Uint32(raw[header+int(i)*16+12 : header+int(i)*16+16]))
		}
		end := uint64(header) + 16*n + payload
		if end > uint64(len(raw)) {
			t.Fatal("fixture short real payload")
		}
		offset = int(end)
		last = start
		batches++
	}
	if batches < 2 || last < 0 {
		t.Fatalf("fixture insufficient actual batches: %d", batches)
	}
	if corrupt {
		raw[last] ^= 1
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "open-1"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual closed %s -> open-1: %d bytes, %d batches, finalCRC=%d corrupt=%v", filepath.Base(path), len(raw), batches, last, corrupt)
	return raw
}

func offlineSegmentTestRows(t *testing.T, ctx context.Context, output string) (int, int) {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.Join(output, "gpuworks.db")}
	uri.RawQuery = url.Values{"mode": {"ro"}, "immutable": {"1"}, "_pragma": {"query_only(1)"}}.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	values := []int{0, 0}
	for i := range values {
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM records WHERE id=%d", i+1)).Scan(&values[i]); err != nil {
			t.Fatal(err)
		}
	}
	return values[0], values[1]
}
