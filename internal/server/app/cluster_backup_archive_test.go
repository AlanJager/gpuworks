package app

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// backupFixture is a member's data directory with every file an archive
// takes, and a database image standing in for the leader's.
func backupFixture(t *testing.T) (dataDir string, manifest ClusterBackupManifest, entries []clusterBackupEntry) {
	t.Helper()
	dataDir = filepath.Join(t.TempDir(), "data")
	write := func(name, body string) string {
		path := filepath.Join(dataDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	entries = append(entries, clusterBackupEntry{name: clusterBackupDatabaseName, path: write("image/gpuworks.db", "SQLite format 3\x00 image bytes")})
	entries = append(entries, clusterBackupEntry{name: "master.key", path: write("master.key", "master")})
	for _, name := range []string{"ca.key", "ca.pem", "config.json", "master.binding", "server.key", "server.pem"} {
		entries = append(entries, clusterBackupEntry{name: clusterBackupEnrollment + "/" + name, path: write(clusterBackupEnrollment+"/"+name, "enrollment "+name)})
	}
	write("catalog-packages/aa/package.tar", "package")
	write("material-replicas/0f/replica", "replica")
	write("model-logos/logo.png", "logo")
	write("logs.db", "not archived")
	material, err := clusterBackupMaterial(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	entries = append(entries, material...)
	manifest = ClusterBackupManifest{
		Format: ClusterBackupFormat, CreatedAt: time.Date(2026, 11, 22, 3, 4, 5, 0, time.UTC), Release: "v26.11.0",
		ClusterID: "c4178b0a97cb456cb65709a4d4126d56", CAFingerprint: strings.Repeat("ab", 32), Protocol: 1, StorageTier: "standard",
		MemberAddress: "192.0.2.11:9001", MemberKey: strings.Repeat("cd", 32), EnrollmentListen: "192.0.2.11:9443", EnrollmentURL: "https://192.0.2.11:9443",
		Members: []ClusterBackupMember{{ID: 1, Address: "192.0.2.11:9001", Role: "voter"}, {ID: 2, Address: "192.0.2.12:9001", Role: "voter"}, {ID: 3, Address: "192.0.2.13:9001", Role: "voter"}},
		Term:    7, IndexFrom: 100, IndexTo: 104, Migrations: []int{1, 2, 3}, Excluded: clusterBackupExcluded,
	}
	return dataDir, manifest, entries
}

func TestClusterBackupArchiveRoundTrip(t *testing.T) {
	dataDir, manifest, entries := backupFixture(t)
	dest := filepath.Join(t.TempDir(), clusterBackupArchiveName(manifest.CreatedAt))
	if filepath.Base(dest) != "cluster-backup-20261122T030405Z.tar" {
		t.Fatalf("archive name = %s", filepath.Base(dest))
	}
	written, err := writeClusterBackupArchive(dest, manifest, entries)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dest)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("archive mode = %v, %v; it holds the cluster authority's key", info.Mode(), err)
	}
	var names []string
	for _, file := range written.Files {
		names = append(names, file.Name)
	}
	want := "database/gpuworks.db master.key cluster-enrollment/ca.key cluster-enrollment/ca.pem cluster-enrollment/config.json cluster-enrollment/master.binding cluster-enrollment/server.key cluster-enrollment/server.pem data/catalog-packages/aa/package.tar data/material-replicas/0f/replica data/model-logos/logo.png"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("archived files\n got %s\nwant %s", got, want)
	}
	backup, err := InspectClusterBackup(dest)
	if err != nil {
		t.Fatal(err)
	}
	if backup.Size != info.Size() || backup.Manifest.IndexTo != 104 || backup.Manifest.Members[2].ID != 3 || len(backup.Manifest.Files) != 11 {
		t.Fatalf("inspected manifest = %+v", backup.Manifest)
	}
	sum := sha256.Sum256([]byte("master"))
	if backup.Manifest.Files[1].SHA256 != hex.EncodeToString(sum[:]) || backup.Manifest.Files[1].Size != 6 {
		t.Fatalf("master.key entry = %+v", backup.Manifest.Files[1])
	}
	// The manifest is the first entry and readable with plain tar.
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	header, err := tar.NewReader(bytes.NewReader(raw)).Next()
	if err != nil || header.Name != "manifest.json" {
		t.Fatalf("first entry = %v, %v", header, err)
	}
	if !bytes.Contains(raw, []byte(`"id": "3"`)) || !bytes.Contains(raw, []byte(`"index_from": 100`)) {
		t.Fatal("manifest wire form changed: member ids are strings and barriers are numbers")
	}
	unpacked := filepath.Join(t.TempDir(), "unpacked")
	if err := os.Mkdir(unpacked, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := readClusterBackup(dest, unpacked); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"master.key": "master", "data/material-replicas/0f/replica": "replica", "cluster-enrollment/ca.key": "enrollment ca.key"} {
		got, err := os.ReadFile(filepath.Join(unpacked, filepath.FromSlash(name)))
		if err != nil || string(got) != body {
			t.Fatalf("unpacked %s = %q, %v", name, got, err)
		}
		if info, _ := os.Stat(filepath.Join(unpacked, filepath.FromSlash(name))); info.Mode().Perm() != 0600 {
			t.Fatalf("unpacked %s mode = %v", name, info.Mode())
		}
	}
	if _, err := os.Stat(filepath.Join(unpacked, "logs.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a file outside the archive's list was unpacked")
	}
	if _, err := writeClusterBackupArchive(dest, manifest, entries); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("an existing archive was replaced: %v", err)
	}
	_ = dataDir
}

func TestClusterBackupArchiveFailureLeavesNothing(t *testing.T) {
	leftovers := func(t *testing.T, dir string) []string {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names
	}
	t.Run("a required file is missing", func(t *testing.T) {
		_, manifest, entries := backupFixture(t)
		if err := os.Remove(entries[1].path); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		_, err := writeClusterBackupArchive(filepath.Join(dir, "cluster-backup.tar"), manifest, entries)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing master.key: %v", err)
		}
		if names := leftovers(t, dir); len(names) != 0 {
			t.Fatalf("a failed backup left %v", names)
		}
	})
	t.Run("a material file removed meanwhile is left out", func(t *testing.T) {
		_, manifest, entries := backupFixture(t)
		if err := os.Remove(entries[len(entries)-1].path); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(t.TempDir(), "cluster-backup.tar")
		written, err := writeClusterBackupArchive(dest, manifest, entries)
		if err != nil || len(written.Files) != 10 {
			t.Fatalf("files = %d, %v", len(written.Files), err)
		}
		if _, err := InspectClusterBackup(dest); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the disk fills while the archive is written", func(t *testing.T) {
		// A file size limit makes the write fail exactly as a full disk does,
		// with a short write and an error from the kernel.
		_, manifest, entries := backupFixture(t)
		big := filepath.Join(t.TempDir(), "big")
		if err := os.WriteFile(big, bytes.Repeat([]byte("x"), 256<<10), 0600); err != nil {
			t.Fatal(err)
		}
		entries[0].path = big
		var old syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
			t.Fatal(err)
		}
		limited := syscall.Rlimit{Cur: 64 << 10, Max: old.Max}
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
				t.Error(err)
			}
		}()
		dir := t.TempDir()
		_, err := writeClusterBackupArchive(filepath.Join(dir, "cluster-backup.tar"), manifest, entries)
		if !errors.Is(err, syscall.EFBIG) {
			t.Fatalf("write past the limit: %v", err)
		}
		if names := leftovers(t, dir); len(names) != 0 {
			t.Fatalf("an interrupted backup left %v", names)
		}
	})
	t.Run("a link among the material is refused", func(t *testing.T) {
		dataDir, _, _ := backupFixture(t)
		if err := os.Symlink("/etc/passwd", filepath.Join(dataDir, "model-logos", "link.png")); err != nil {
			t.Fatal(err)
		}
		if _, err := clusterBackupMaterial(dataDir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("link accepted: %v", err)
		}
	})
}

func TestClusterBackupInspectRefusesAlteredArchives(t *testing.T) {
	_, manifest, entries := backupFixture(t)
	dest := filepath.Join(t.TempDir(), "cluster-backup.tar")
	if _, err := writeClusterBackupArchive(dest, manifest, entries); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	alter := func(t *testing.T, change func([]byte) []byte) error {
		t.Helper()
		path := filepath.Join(t.TempDir(), "altered.tar")
		if err := os.WriteFile(path, change(bytes.Clone(original)), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := InspectClusterBackup(path)
		return err
	}
	cases := map[string]func([]byte) []byte{
		"one byte of the database image": func(b []byte) []byte {
			i := bytes.Index(b, []byte(" image bytes"))
			b[i+1] ^= 0x01
			return b
		},
		"one byte of the master key": func(b []byte) []byte {
			i := bytes.LastIndex(b, []byte("master\x00"))
			b[i] = 'M'
			return b
		},
		"truncated before the last file": func(b []byte) []byte { return b[:len(b)-2048] },
		"a file appended after the last": func(b []byte) []byte {
			var extra bytes.Buffer
			w := tar.NewWriter(&extra)
			_ = w.WriteHeader(&tar.Header{Name: "data/model-logos/extra.png", Mode: 0600, Size: 1, Typeflag: tar.TypeReg})
			_, _ = w.Write([]byte("x"))
			_ = w.Close()
			return append(b[:len(b)-1024], extra.Bytes()...)
		},
		"a manifest of another format": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"format": 1`), []byte(`"format": 9`), 1)
		},
		"not an archive": func([]byte) []byte { return []byte("SQLite format 3\x00") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			if err := alter(t, change); !errors.Is(err, ErrClusterBackupInvalid) {
				t.Fatalf("altered archive accepted or misreported: %v", err)
			}
		})
	}
	// An unpack of an altered archive fails too, so a restore never proceeds
	// on files that differ from the manifest.
	path := filepath.Join(t.TempDir(), "altered.tar")
	changed := bytes.Clone(original)
	changed[bytes.Index(changed, []byte(" image bytes"))+1] ^= 0x01
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := readClusterBackup(path, dir); !errors.Is(err, ErrClusterBackupInvalid) {
		t.Fatalf("unpack of an altered archive: %v", err)
	}
}

func TestClusterBackupManifestRequiresEveryRestoreInput(t *testing.T) {
	_, manifest, entries := backupFixture(t)
	for _, missing := range []string{clusterBackupDatabaseName, "master.key", "cluster-enrollment/ca.key", "cluster-enrollment/server.key"} {
		var kept []clusterBackupEntry
		for _, entry := range entries {
			if entry.name != missing {
				kept = append(kept, entry)
			}
		}
		dest := filepath.Join(t.TempDir(), "cluster-backup.tar")
		if _, err := writeClusterBackupArchive(dest, manifest, kept); err != nil {
			t.Fatal(err)
		}
		if _, err := InspectClusterBackup(dest); !errors.Is(err, ErrClusterBackupInvalid) || !strings.Contains(err.Error(), missing+" is missing") {
			t.Fatalf("archive without %s: %v", missing, err)
		}
	}
	for _, name := range []string{"../escape", "/abs", "data/other/file", "cluster-enrollment/sub/file", "manifest.json", "data/model-logos/../x"} {
		if validClusterBackupName(name) {
			t.Errorf("entry name %q accepted", name)
		}
	}
}

func TestClusterBackupListAndPrune(t *testing.T) {
	_, manifest, entries := backupFixture(t)
	dir := t.TempDir()
	if got, err := ListClusterBackups(filepath.Join(dir, "absent")); err != nil || got != nil {
		t.Fatalf("absent directory = %v, %v", got, err)
	}
	for hour := 1; hour <= 4; hour++ {
		manifest.CreatedAt = time.Date(2026, 11, 22, hour, 0, 0, 0, time.UTC)
		if _, err := writeClusterBackupArchive(filepath.Join(dir, clusterBackupArchiveName(manifest.CreatedAt)), manifest, entries); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "cluster-backup-notes.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	backups, err := ListClusterBackups(dir)
	if err != nil || len(backups) != 4 || backups[0].Manifest.CreatedAt.Hour() != 4 || backups[3].Manifest.CreatedAt.Hour() != 1 {
		t.Fatalf("list = %d archives, %v", len(backups), err)
	}
	if _, err := PruneClusterBackups(dir, 0); err == nil {
		t.Fatal("pruning to zero archives accepted")
	}
	removed, err := PruneClusterBackups(dir, 2)
	if err != nil || len(removed) != 2 || filepath.Base(removed[0]) != "cluster-backup-20261122T020000Z.tar" || filepath.Base(removed[1]) != "cluster-backup-20261122T010000Z.tar" {
		t.Fatalf("removed = %v, %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cluster-backup-notes.txt")); err != nil {
		t.Fatal("prune removed a file it does not name")
	}
	// A damaged archive in the directory is reported, not skipped.
	if err := os.WriteFile(filepath.Join(dir, "cluster-backup-20261122T050000Z.tar"), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListClusterBackups(dir); !errors.Is(err, ErrClusterBackupInvalid) || !strings.Contains(err.Error(), "20261122T050000Z") {
		t.Fatalf("damaged archive in the list: %v", err)
	}
}
