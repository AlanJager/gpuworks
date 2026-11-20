//go:build linux && dqlite

package cluster

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/filelock"
)

func TestSingleAddressConversionUnderLease(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	parent := t.TempDir()
	source, original := conversionSource(t, ctx, parent)
	before, err := inspectConversionDirectory(ctx, source+".dqlite")
	if err != nil {
		t.Fatal(err)
	}
	release, err := filelock.Acquire(source + ".runtime.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	maintenance := []byte(`{"operation":"convert-with-continuous-lease"}`)
	if err := os.WriteFile(source+".maintenance", maintenance, 0600); err != nil {
		t.Fatal(err)
	}
	target := newConversionTestCA(t).config(t, filepath.Join(parent, "candidate"), "127.0.0.1", 2)
	backup := filepath.Join(parent, "backup")
	converted, err := PrepareSingleMemberAddressUnderLease(ctx, source, backup, target, maintenance)
	if err != nil {
		t.Fatal(err)
	}
	if converted.ID != original.ID || converted.Address != target.Address {
		t.Fatalf("identity: %#v", converted)
	}
	if unexpected, err := filelock.Acquire(source + ".runtime.lock"); err == nil {
		_ = unexpected()
		t.Fatal("preparation released caller's lease")
	}
	actual, err := os.ReadFile(source + ".maintenance")
	if err != nil || string(actual) != string(maintenance) {
		t.Fatalf("intent changed: %q %v", actual, err)
	}
	after, err := inspectConversionDirectory(ctx, source+".dqlite")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("source changed: %v", err)
	}
	db, err := OpenDatabase(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var uuid string
	if err := db.SQL.QueryRowContext(ctx, "SELECT uuid FROM resources WHERE id=1").Scan(&uuid); err != nil || uuid != "uuid-before" {
		t.Fatalf("candidate data: %q %v", uuid, err)
	}
	members, err := db.Members(ctx)
	if err != nil || len(members) != 1 || members[0].ID != original.ID || members[0].Address != target.Address {
		t.Fatalf("candidate live membership: %#v %v", members, err)
	}
}

func TestSingleAddressConversionUnderLeaseRejectsIntentAndJoin(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	parent := t.TempDir()
	source, _ := conversionSource(t, ctx, parent)
	release, err := filelock.Acquire(source + ".runtime.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	target := newConversionTestCA(t).config(t, filepath.Join(parent, "candidate"), "127.0.0.1", 2)
	backup := filepath.Join(parent, "backup")
	marker := source + ".maintenance"
	intent := []byte("exact durable intent")
	for _, name := range []string{"empty", "missing", "mismatch", "symlink", "public", "join", "restore", "legacy"} {
		t.Run(name, func(t *testing.T) {
			_ = os.Remove(marker)
			supplied := intent
			if name != "missing" {
				if err := os.WriteFile(marker, intent, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var extra string
			switch name {
			case "empty":
				supplied = nil
			case "mismatch":
				supplied = []byte("other intent")
			case "symlink":
				if err := os.Rename(marker, marker+".real"); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(marker + ".real")
				if err := os.Symlink(marker+".real", marker); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.Chmod(marker, 0644); err != nil {
					t.Fatal(err)
				}
			case "join":
				extra = source + ".join"
			case "restore":
				extra = filepath.Join(parent, ".restore-in-progress")
			case "legacy":
				extra = source
			}
			if extra != "" {
				if err := os.WriteFile(extra, []byte("guard"), 0600); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(extra)
			}
			if _, err := PrepareSingleMemberAddressUnderLease(ctx, source, backup, target, supplied); err == nil {
				t.Fatal("unsafe intent accepted")
			}
			for _, path := range []string{backup, target.Dir} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("rejection created output %s: %v", path, err)
				}
			}
		})
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source+".join", []byte("unfinished join"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSingleMemberAddress(ctx, source, backup, target); err == nil {
		t.Fatal("public preparation bypassed join guard")
	}
}
