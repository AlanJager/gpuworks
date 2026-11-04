package selfinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBackupAndRestoreRoundTrip(t *testing.T) {
	h := newHarness(t, t.TempDir())
	existing := h.writeExistingInstall("v1.0.0")
	// A database that was running when it was snapshotted has a write-ahead log
	// beside it, which the restore must also bring back.
	writeFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db-wal"), "wal contents", 0o600)
	writeFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db-shm"), "shm contents", 0o600)
	if err := mkdirAll(h.layout.BackupsDir, dirMode); err != nil {
		t.Fatalf("mkdir backups: %v", err)
	}

	path, err := h.installer.createBackup(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatalf("createBackup: %v", err)
	}
	if filepath.Dir(path) != h.layout.BackupsDir {
		t.Fatalf("backup path = %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	policy, modeErr := h.installer.sensitiveFilePolicy()
	if modeErr != nil {
		t.Fatal(modeErr)
	}
	if info.Mode().Perm() != policy.Mode() {
		t.Fatalf("backup mode = %v, want installation-sensitive permissions", info.Mode().Perm())
	}
	members := readArchive(t, path)
	for _, want := range []string{"data/gpuworks.db", "data/gpuworks.db-wal", "data/gpuworks.db-shm",
		"data/master.key", "server.yaml", "bin/gpuworks"} {
		if _, ok := members[want]; !ok {
			t.Errorf("backup is missing %q (has %v)", want, keysOf(members))
		}
	}
	if got := members["server.yaml"]; !strings.Contains(got, existing.Addr) {
		t.Fatalf("the archived server.yaml is not the one on disk: %s", got)
	}
	// An upgrade that changed the configuration is rolled back with it.
	if err := os.WriteFile(h.layout.ConfigPath(), []byte("listen:\n  console: [10.9.9.9:1]\n"), 0o664); err != nil {
		t.Fatal(err)
	}

	// Everything the new version wrote is replaced by the snapshot, including
	// the write-ahead log: replaying a newer log over an older database is how
	// a rollback corrupts itself.
	writeFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db"), "upgraded database", 0o600)
	writeFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db-wal"), "newer wal", 0o600)
	writeFile(t, h.layout.Binary(), "new binary", 0o755)

	if err := h.installer.restoreBackup(path); err != nil {
		t.Fatalf("restoreBackup: %v", err)
	}
	if got := readFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db")); got != "old database" {
		t.Fatalf("database = %q", got)
	}
	if got := readFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db-wal")); got != "wal contents" {
		t.Fatalf("write-ahead log = %q", got)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("binary = %q", got)
	}
	info, err = os.Stat(h.layout.Binary())
	if err != nil {
		t.Fatalf("stat restored binary: %v", err)
	}
	if info.Mode().Perm() != binaryMode {
		t.Fatalf("restored binary mode = %v", info.Mode().Perm())
	}
	if got := h.config(); got.Listen.Console[0] != existing.Addr {
		t.Fatalf("server.yaml = %+v, want the snapshot's", got)
	}
	// The restore keeps the mode the snapshot recorded, not a blanket 0600: a
	// server.yaml restored as 0600 would lock the service account out.
	info, err = os.Stat(h.layout.ConfigPath())
	if err != nil {
		t.Fatalf("stat restored server.yaml: %v", err)
	}
	wantConfigMode := os.FileMode(0644)
	if _, group := policy.GroupID(); group {
		wantConfigMode = 0664
	}
	if info.Mode().Perm() != wantConfigMode {
		t.Fatalf("restored server.yaml mode=%#o want%#o", info.Mode().Perm(), wantConfigMode)
	}
}

func TestBackupSkipsFilesThatAreNotThere(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	// A database that has never been checkpointed has no -wal, and a
	// configuration removed by hand has no server.yaml; neither may fail the
	// snapshot.
	if err := os.Remove(h.layout.ConfigPath()); err != nil {
		t.Fatalf("remove server.yaml: %v", err)
	}
	if err := mkdirAll(h.layout.BackupsDir, dirMode); err != nil {
		t.Fatalf("mkdir backups: %v", err)
	}

	path, err := h.installer.createBackup(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatalf("createBackup: %v", err)
	}
	members := readArchive(t, path)
	if _, ok := members["data/gpuworks.db"]; !ok {
		t.Fatal("the database was not backed up")
	}
	if _, ok := members["data/gpuworks.db-wal"]; ok {
		t.Fatal("a file that does not exist was written into the archive")
	}
	if _, ok := members["server.yaml"]; ok {
		t.Fatal("a missing server.yaml was written into the archive")
	}
}

func TestRestoreRejectsAnArchiveThatDoesNotBelong(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	if err := mkdirAll(h.layout.BackupsDir, dirMode); err != nil {
		t.Fatalf("mkdir backups: %v", err)
	}

	for name, members := range map[string]map[string]string{
		"escape.tar.gz":    {"data/../../etc/passwd": "root:x:0:0"},
		"unknown.tar.gz":   {"secrets": "something else"},
		"unrelated.tar.gz": {"other/thing": "else"},
	} {
		path := writeArchive(t, h.layout.BackupsDir, name, members)
		err := h.installer.restoreBackup(path)
		if err == nil {
			t.Fatalf("%s: a snapshot with foreign members was accepted", name)
		}
		if got := readFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db")); got != "old database" {
			t.Fatalf("%s: the database was overwritten by a rejected restore: %q", name, got)
		}
	}
}

func TestRestoreRejectsACorruptArchive(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	if err := mkdirAll(h.layout.BackupsDir, dirMode); err != nil {
		t.Fatalf("mkdir backups: %v", err)
	}
	path := filepath.Join(h.layout.BackupsDir, "truncated.tar.gz")
	writeFile(t, path, "not a gzip stream", 0o600)

	if err := h.installer.restoreBackup(path); err == nil {
		t.Fatal("a corrupt snapshot was accepted")
	}
}

func TestPruneBackupsKeepsTheNewest(t *testing.T) {
	h := newHarness(t, t.TempDir())
	if err := mkdirAll(h.layout.BackupsDir, dirMode); err != nil {
		t.Fatalf("mkdir backups: %v", err)
	}
	for _, name := range []string{
		"v1.0.0-20260101T000000Z.tar.gz",
		"v1.0.0-20260102T000000Z.tar.gz",
		"v1.0.0-20260103T000000Z.tar.gz",
		"v1.0.0-20260104T000000Z.tar.gz",
		"notes.txt",
		"v1.0.0-20260105T000000Z.tar.gz.part",
	} {
		writeFile(t, filepath.Join(h.layout.BackupsDir, name), "snapshot", 0o600)
	}

	h.installer.pruneBackups(2, "")
	names := backupNames(t, h.layout.BackupsDir)
	if len(names) != 2 {
		t.Fatalf("backups = %v", names)
	}
	if names[0] != "v1.0.0-20260103T000000Z.tar.gz" || names[1] != "v1.0.0-20260104T000000Z.tar.gz" {
		t.Fatalf("the wrong backups survived: %v", names)
	}
	for _, keep := range []string{"notes.txt", "v1.0.0-20260105T000000Z.tar.gz.part"} {
		if _, err := os.Stat(filepath.Join(h.layout.BackupsDir, keep)); err != nil {
			t.Fatalf("pruning removed %s, which is not a snapshot: %v", keep, err)
		}
	}

	// A keep of zero or less means "no limit", not "delete everything": the
	// upgrade path would otherwise delete the snapshot it just took.
	h.installer.pruneBackups(0, "")
	if got := backupNames(t, h.layout.BackupsDir); len(got) != 2 {
		t.Fatalf("backups = %v", got)
	}
}

// The release ids in this test are the ones GWKS-146 measured: `aaa-` sorts
// before both `dev-` and `sit-`, so ordering snapshots by their whole name
// deletes the newest one first. Ordering by the timestamp in the name keeps the
// newest two whatever the versions are called.
func TestPruneBackupsOrdersByTimeNotReleaseID(t *testing.T) {
	h := newHarness(t, t.TempDir())
	if err := mkdirAll(h.layout.BackupsDir, dirMode); err != nil {
		t.Fatalf("mkdir backups: %v", err)
	}
	// Oldest first, and the release id runs the opposite way.
	newest := h.layout.BackupPath("aaa-keep20000", time.Date(2026, 10, 24, 9, 24, 15, 0, time.UTC))
	for _, path := range []string{
		h.layout.BackupPath("dev-0b0801dfd", time.Date(2026, 10, 24, 9, 23, 15, 0, time.UTC)),
		h.layout.BackupPath("sit-zkeep1000", time.Date(2026, 10, 24, 9, 23, 27, 0, time.UTC)),
		newest,
	} {
		writeFile(t, path, "snapshot", 0o600)
	}

	h.installer.pruneBackups(2, "")

	if got, want := backupNames(t, h.layout.BackupsDir), []string{
		"aaa-keep20000-20261024T092415Z.tar.gz",
		"sit-zkeep1000-20261024T092327Z.tar.gz",
	}; !slices.Equal(got, want) {
		t.Fatalf("survivors = %v, want the newest two %v", got, want)
	}
	if _, err := os.Stat(newest); err != nil {
		t.Fatalf("the newest snapshot was pruned: %v", err)
	}
}

// Pruning runs at the end of the upgrade that took the snapshot, so the archive
// it just wrote must survive even if an older one carries the same stamp and
// wins the tie-break — otherwise the upgrade reports a backup path, and writes
// it to the history as the rollback point, that does not exist.
func TestPruneBackupsNeverRemovesTheArchiveJustWritten(t *testing.T) {
	h := newHarness(t, t.TempDir())
	if err := mkdirAll(h.layout.BackupsDir, dirMode); err != nil {
		t.Fatalf("mkdir backups: %v", err)
	}
	at := time.Date(2026, 10, 24, 9, 23, 15, 0, time.UTC)
	current := h.layout.BackupPath("sit-zkeep1000", at)
	// Same second, and a name that sorts later, so a name tie-break alone would
	// hand the surviving slot to the older snapshot.
	writeFile(t, filepath.Join(h.layout.BackupsDir, "zzz-older-20261024T092315Z.tar.gz"), "snapshot", 0o600)
	writeFile(t, current, "snapshot", 0o600)

	h.installer.pruneBackups(1, current)

	if got := backupNames(t, h.layout.BackupsDir); !slices.Equal(got, []string{filepath.Base(current)}) {
		t.Fatalf("survivors = %v, want only the snapshot just taken", got)
	}
}

func TestPruneBackupsLeavesNamesItCannotRead(t *testing.T) {
	h := newHarness(t, t.TempDir())
	if err := mkdirAll(h.layout.BackupsDir, dirMode); err != nil {
		t.Fatalf("mkdir backups: %v", err)
	}
	// Each of these is unreadable one way or another: no stamp, an impossible
	// month, a stamp that is not at the end, and a stamp with no release id
	// separator. They must not be ranked, so they must not be deleted either.
	unreadable := []string{
		"hand-made.tar.gz",
		"v1.0.0-20261324T092315Z.tar.gz",
		"v1.0.0-20261024T092315Z-copy.tar.gz",
		"20261024T092315Z.tar.gz",
	}
	for _, name := range unreadable {
		writeFile(t, filepath.Join(h.layout.BackupsDir, name), "snapshot", 0o600)
	}
	for _, at := range []time.Time{
		time.Date(2026, 10, 24, 9, 23, 15, 0, time.UTC),
		time.Date(2026, 10, 24, 9, 23, 16, 0, time.UTC),
		time.Date(2026, 10, 24, 9, 23, 17, 0, time.UTC),
	} {
		writeFile(t, h.layout.BackupPath("v1.0.0", at), "snapshot", 0o600)
	}

	h.installer.pruneBackups(1, "")

	got := backupNames(t, h.layout.BackupsDir)
	want := append([]string{"v1.0.0-20261024T092317Z.tar.gz"}, unreadable...)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("survivors = %v, want the newest readable snapshot plus every unreadable name %v", got, want)
	}
}

func TestExtractBinary(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "gpuworks")

	if err := extractBinary(writeArchive(t, dir, "ok.tar.gz", map[string]string{
		"gpuworks": "the binary", "README.md": "docs",
	}), dest); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if got := readFile(t, dest); got != "the binary" {
		t.Fatalf("binary = %q", got)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != binaryMode {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}

	for name, members := range map[string]map[string]string{
		"no-binary.tar.gz":    {"README.md": "docs"},
		"two-binaries.tar.gz": {"gpuworks": "first", "bin/gpuworks": "second"},
	} {
		err := extractBinary(writeArchive(t, dir, name, members), filepath.Join(dir, "out"))
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if err := extractBinary(filepath.Join(dir, "missing.tar.gz"), dest); err == nil {
		t.Fatal("a missing archive was accepted")
	}
	writeFile(t, filepath.Join(dir, "corrupt.tar.gz"), "not gzip", 0o600)
	if err := extractBinary(filepath.Join(dir, "corrupt.tar.gz"), dest); err == nil {
		t.Fatal("a corrupt archive was accepted")
	}
}

func TestExtractBinaryRejectsAStuntedMember(t *testing.T) {
	dir := t.TempDir()
	// A header that promises more bytes than the archive holds: the extractor
	// must notice rather than install half a program.
	path := filepath.Join(dir, "stunted.tar.gz")
	writeFile(t, path, string(truncatedMember(t, "gpuworks", 100, "short")), 0o600)

	err := extractBinary(path, filepath.Join(dir, "out"))
	if err == nil {
		t.Fatal("an archive with a stunted member was accepted")
	}
	if !strings.Contains(err.Error(), "truncated") && !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractBinaryRejectsAnOversizedMember(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.tar.gz")
	header := &tar.Header{Name: binaryName, Mode: 0o755, Typeflag: tar.TypeReg,
		Size: maxExtractedBinaryBytes + 1}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	writer := tar.NewWriter(gz)
	if err := writer.WriteHeader(header); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	writeFile(t, path, buf.String(), 0o600)

	if err := extractBinary(path, filepath.Join(dir, "out")); err == nil ||
		!strings.Contains(err.Error(), "beyond the") {
		t.Fatalf("err = %v", err)
	}
}

// truncatedMember builds a gzip stream whose tar header claims size bytes but
// whose body stops short.
func truncatedMember(t *testing.T, name string, size int64, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	writer := tar.NewWriter(gz)
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeReg, Size: size}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := writer.Write([]byte(body)); err != nil {
		t.Fatalf("write body: %v", err)
	}
	// Close reports the missing bytes; the stream is written anyway, which is
	// exactly the damaged archive under test.
	_ = writer.Close()
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func TestCopyFileAtomicReplacesTheTarget(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "sub", "dst")
	writeFile(t, src, "content", 0o644)

	if err := copyFileAtomic(src, dst, binaryMode); err != nil {
		t.Fatalf("copyFileAtomic: %v", err)
	}
	if got := readFile(t, dst); got != "content" {
		t.Fatalf("dst = %q", got)
	}
	if err := copyFileAtomic(filepath.Join(dir, "missing"), dst, binaryMode); err == nil {
		t.Fatal("copying a missing source was accepted")
	}
	if got := readFile(t, dst); got != "content" {
		t.Fatalf("a failed copy damaged the target: %q", got)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "sub"))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("a temporary file was left behind: %s", entry.Name())
		}
	}
}

func TestReplaceBinaryRejectsAMissingStagedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "gpuworks")
	writeFile(t, target, "old", 0o755)

	if err := replaceBinary(filepath.Join(dir, "nothing"), target); err == nil {
		t.Fatal("replacing from a missing file was accepted")
	}
	if got := readFile(t, target); got != "old" {
		t.Fatalf("the installed binary was damaged: %q", got)
	}
}

func TestChownIsSkippedForRoot(t *testing.T) {
	h := newHarness(t, t.TempDir())
	if err := h.installer.chownTree(context.Background(), "root"); err != nil {
		t.Fatalf("chownTree(root): %v", err)
	}
	if err := h.installer.chownTree(context.Background(), ""); err != nil {
		t.Fatalf("chownTree(empty): %v", err)
	}
	if h.run.ran("sudo chown") {
		t.Fatalf("root needed no chown: %v", h.run.executed())
	}
	h.users["gpuworks"] = true
	if err := h.installer.chownTree(context.Background(), "gpuworks"); err != nil {
		t.Fatalf("chownTree: %v", err)
	}
	if !h.run.ran("sudo chown -R gpuworks:gpuworks") {
		t.Fatalf("the tree was not given to the service user: %v", h.run.executed())
	}
}

func TestEnsureServiceUserIsIdempotent(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.users["gpuworks"] = true
	if err := h.installer.ensureServiceUser(context.Background(), "gpuworks"); err != nil {
		t.Fatalf("ensureServiceUser: %v", err)
	}
	if h.run.ran("sudo useradd") {
		t.Fatalf("an existing account was recreated: %v", h.run.executed())
	}
	// A failure from both spellings is reported, not swallowed.
	h2 := newHarness(t, t.TempDir())
	h2.run.fail["sudo useradd"] = errors.New("permission denied")
	h2.run.fail["sudo adduser"] = errors.New("permission denied")
	if err := h2.installer.ensureServiceUser(context.Background(), "gpuworks"); err == nil {
		t.Fatal("a failed account creation was reported as success")
	}
}

// keysOf sorts the member names for a failure message.
func keysOf(members map[string]string) []string {
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	return names
}

// writeArchive writes a tar.gz fixture and returns its path.
func writeArchive(t *testing.T, dir, name string, members map[string]string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	writeFile(t, path, string(tarGz(t, members)), 0o600)
	return path
}

// A snapshot written by a version that skipped the master key (GWKS-148)
// restores its database without complaint and leaves the key alone: the
// database comes back, but the credentials sealed inside it stay unreadable
// because the key that sealed them is not in the archive. This pins the
// behaviour rather than endorsing it — the fix refuses to write such a
// snapshot, so the only way to have one is to carry it from an older build.
func TestRestoreOfAKeylessSnapshotBringsBackTheDatabaseWithoutAKey(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	if err := mkdirAll(h.layout.BackupsDir, dirMode); err != nil {
		t.Fatalf("mkdir backups: %v", err)
	}
	path := writeArchive(t, h.layout.BackupsDir, "keyless.tar.gz", map[string]string{
		"data/gpuworks.db": "old database",
		"bin/gpuworks":     "old binary",
	})
	// The newer version generated a fresh key after the snapshot was taken.
	writeFile(t, filepath.Join(h.layout.DataDir, "master.key"), "regenerated key", 0o600)

	if err := h.installer.restoreBackup(path); err != nil {
		t.Fatalf("restoreBackup: %v", err)
	}
	if got := readFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db")); got != "old database" {
		t.Fatalf("database = %q", got)
	}
	// The key that is there is the one the snapshot never held, so the restored
	// database's encrypted columns cannot be read with it.
	if got := readFile(t, filepath.Join(h.layout.DataDir, "master.key")); got != "regenerated key" {
		t.Fatalf("master.key = %q, want the one already on disk", got)
	}
	// With no key on disk at all the restore still succeeds and writes none:
	// the next start would generate one, which is the same loss.
	if err := os.Remove(filepath.Join(h.layout.DataDir, "master.key")); err != nil {
		t.Fatal(err)
	}
	if err := h.installer.restoreBackup(path); err != nil {
		t.Fatalf("restoreBackup without a key on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.layout.DataDir, "master.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the restore wrote a master key: %v", err)
	}
}
