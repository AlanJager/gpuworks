//go:build linux && dqlite

package cluster

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestOfflineSegmentNormalizationKeepsValidAllocation(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	original, member := offlineSegmentTestSource(t, ctx)
	for _, mode := range []string{"valid-zero-tail", "empty", "zero-allocation", "version-with-zero-allocation", "nonzero-tail", "short-header", "closed-crc"} {
		t.Run(mode, func(t *testing.T) {
			parent := t.TempDir()
			scratch := filepath.Join(parent, "scratch")
			if _, err := copyConversionDirectory(ctx, original, scratch); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "valid-zero-tail", "nonzero-tail":
				raw := offlineSegmentTestUnfinalize(t, scratch, false)
				suffix := make([]byte, 64<<10)
				if mode == "nonzero-tail" {
					suffix[24] = 42
				}
				if err := os.WriteFile(filepath.Join(scratch, "open-1"), append(raw, suffix...), 0600); err != nil {
					t.Fatal(err)
				}
			case "empty", "zero-allocation", "version-with-zero-allocation", "short-header":
				var raw []byte
				if mode == "zero-allocation" || mode == "version-with-zero-allocation" {
					raw = make([]byte, 64<<10)
				}
				if mode == "version-with-zero-allocation" {
					binary.LittleEndian.PutUint64(raw, 1)
				}
				if mode == "short-header" {
					raw = []byte{1, 0, 0}
				}
				if err := os.WriteFile(filepath.Join(scratch, "open-1"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "closed-crc":
				raw := offlineSegmentTestUnfinalize(t, scratch, true)
				// Rename back to the actual closed range, preserving the corrupt batch.
				if err := os.Remove(filepath.Join(scratch, "open-1")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(scratch, "0000000000000002-0000000000000006"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			output := filepath.Join(parent, "export")
			err := offlinePreviewTestExport(ctx, scratch, "gpuworks.db", member, output)
			refuse := mode == "nonzero-tail" || mode == "short-header" || mode == "closed-crc"
			if refuse {
				if err == nil {
					t.Fatal("damaged native persistence qualified")
				}
				if _, err := os.Lstat(output); !os.IsNotExist(err) {
					t.Fatalf("damaged export published: %v", err)
				}
				t.Logf("real native refusal: %v", err)
			} else {
				if err != nil {
					t.Fatal(err)
				}
				prefix, tail := offlineSegmentTestRows(t, ctx, output)
				if prefix != 1 || tail != 1 {
					t.Fatalf("valid allocation lost actual rows: %d %d", prefix, tail)
				}
			}
		})
	}
}

func TestOfflineSegmentPreservationRequiresDistinctNewClosedFiles(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	original, _ := offlineSegmentTestSource(t, ctx)
	scratch := filepath.Join(t.TempDir(), "scratch")
	if _, err := copyConversionDirectory(ctx, original, scratch); err != nil {
		t.Fatal(err)
	}
	raw := offlineSegmentTestUnfinalize(t, scratch, false)
	if err := os.WriteFile(filepath.Join(scratch, "open-2"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	opens, protected, err := offlinePreviewSegmentInputs(ctx, scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(opens) != 2 {
		t.Fatalf("actual original opens: %d", len(opens))
	}
	if err := describeLastRaftEntry(ctx, scratch); err != nil {
		t.Fatal(err)
	}
	if err := offlinePreviewVerifySegments(ctx, scratch, opens, protected); err != nil {
		t.Fatal("two actual normalized files did not preserve two opens", err)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	var created []string
	for _, entry := range entries {
		kind, err := offlinePreviewSegmentKind(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if kind == "closed" {
			if _, old := protected[entry.Name()]; !old {
				created = append(created, entry.Name())
			}
		}
	}
	sort.Strings(created)
	if len(created) != 2 {
		t.Fatalf("canonical parser produced %v", created)
	}
	if err := os.Remove(filepath.Join(scratch, created[1])); err != nil {
		t.Fatal(err)
	}
	if err := offlinePreviewVerifySegments(ctx, scratch, opens, protected); err == nil {
		t.Fatal("one new closed file satisfied two actual original opens")
	}
	// An identical preexisting closed file is also not a replacement receipt.
	only := map[string]offlinePreviewSegmentBytes{"open-1": opens["open-1"]}
	old := map[string]offlinePreviewSegmentBytes{}
	for name, fact := range protected {
		old[name] = fact
	}
	prior, err := offlinePreviewHashSegment(ctx, filepath.Join(scratch, created[0]), false)
	if err != nil {
		t.Fatal(err)
	}
	old[created[0]] = prior
	if err := offlinePreviewVerifySegments(ctx, scratch, only, old); err == nil {
		t.Fatal("identical preexisting closed bytes stood in for dropped open")
	}
	t.Logf("real normalized files=%v; missing distinct receipt and old-file substitution refused", created)
}

func TestOfflineSegmentPreservationRejectsChangedProtectedBytesAndCancellation(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	original, _ := offlineSegmentTestSource(t, ctx)
	for _, mode := range []string{"closed-content", "closed-replacement", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			scratch := filepath.Join(t.TempDir(), "scratch")
			if _, err := copyConversionDirectory(ctx, original, scratch); err != nil {
				t.Fatal(err)
			}
			opens, protected, err := offlinePreviewSegmentInputs(ctx, scratch)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cancel" {
				canceled, stop := context.WithCancel(ctx)
				stop()
				if err := offlinePreviewVerifySegments(canceled, scratch, opens, protected); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel guard: %v", err)
				}
				return
			}
			var name string
			for candidate := range protected {
				if strings.Contains(candidate, "-") {
					name = candidate
					break
				}
			}
			if name == "" {
				t.Fatal("no actual closed file")
			}
			path := filepath.Join(scratch, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "closed-content" {
				raw[len(raw)-1] ^= 1
			} else {
				// Create the replacement while the original inode still exists.
				if err := os.WriteFile(path+".replacement", raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".replacement", path); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "closed-content" {
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := offlinePreviewVerifySegments(ctx, scratch, opens, protected); err == nil {
				t.Fatal("protected native bytes/identity changed without refusal")
			}
		})
	}
}

func TestOfflineSegmentSnapshotPairsRefuseOmissionAndCorruption(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	original := filepath.Join(t.TempDir(), "original")
	db, err := OpenSingleDatabase(ctx, original, "gpuworks.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.SQL.ExecContext(ctx, "CREATE TABLE snapshot_rows (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	// Produce a genuine C snapshot, rather than inventing a paired filename.
	for i := 1; i <= 1050; i++ {
		if _, err := db.SQL.ExecContext(ctx, "INSERT INTO snapshot_rows VALUES (?)", i); err != nil {
			t.Fatal(err)
		}
	}
	var meta string
	conversionWait(t, ctx, "actual native snapshot", func() bool {
		names, _ := filepath.Glob(filepath.Join(original, "snapshot-*.meta"))
		sort.Strings(names)
		if len(names) > 0 {
			meta = filepath.Base(names[len(names)-1])
			return true
		}
		return false
	})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual native snapshot: %s", meta)
	for _, mode := range []string{"intact", "missing-data", "missing-meta", "corrupt-meta", "corrupt-data"} {
		t.Run(mode, func(t *testing.T) {
			scratch := filepath.Join(t.TempDir(), "scratch")
			if _, err := copyConversionDirectory(ctx, original, scratch); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(scratch, meta)
			if mode == "missing-data" || mode == "corrupt-data" {
				path = strings.TrimSuffix(path, ".meta")
			}
			if strings.HasPrefix(mode, "missing-") {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(mode, "corrupt-") {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if len(raw) < 8 {
					t.Fatal("actual snapshot too short")
				}
				if mode == "corrupt-meta" {
					raw[0] ^= 1
				} else {
					raw = bytes.Repeat([]byte{255}, len(raw))
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := offlinePreviewTestHashes(t, scratch)
			err := offlinePreviewPreserveSegments(ctx, scratch)
			if mode == "intact" {
				if err != nil {
					t.Fatal(err)
				}
			} else if mode == "corrupt-data" {
				// DescribeLastEntry loads the snapshot bytes but does not execute FSM.
				// Complete exporter/start must reject them before qualified output.
				output := filepath.Join(filepath.Dir(scratch), "export")
				member := Member{ID: db.ID, Address: db.Address, Role: "voter"}
				if err := offlinePreviewTestExport(ctx, scratch, "gpuworks.db", member, output); err == nil {
					t.Fatal("corrupt snapshot FSM data exported")
				}
			} else {
				if err == nil {
					t.Fatal("missing/corrupt snapshot qualified")
				}
				if strings.HasPrefix(mode, "missing-") && !reflect.DeepEqual(before, offlinePreviewTestHashes(t, scratch)) {
					t.Fatal("orphan cleanup began before refusal")
				}
				t.Logf("actual snapshot refusal: %v", err)
			}
		})
	}
}
