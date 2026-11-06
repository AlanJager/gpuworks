package diagnostics

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeSized(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestServerDiskReportsRoomAndWhatFillsEachDirectory(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("statfs is reported on Linux and macOS")
	}
	data := t.TempDir()
	writeSized(t, filepath.Join(data, "gpuworks.db"), 1000)
	writeSized(t, filepath.Join(data, "gpuworks.db-wal"), 5000)
	writeSized(t, filepath.Join(data, "diagnostics", "bundles", "a.tar.gz"), 300)
	writeSized(t, filepath.Join(data, "diagnostics", "debug", "s1", "x.log"), 200)
	tmp := t.TempDir()
	writeSized(t, filepath.Join(tmp, "unrelated"), 10)
	missing := filepath.Join(data, "no-such-dir")

	var buf bytes.Buffer
	if _, _, err := writeServerDisk(&buf, []ServerDir{{Kind: "data", Path: data}, {Kind: "sqlite_tmp", Path: tmp}, {Kind: "backups", Path: missing}}); err != nil {
		t.Fatal(err)
	}
	var got ServerDisk
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("%s: %v", buf.String(), err)
	}
	if len(got.Dirs) != 3 {
		t.Fatalf("dirs = %+v", got.Dirs)
	}
	d := got.Dirs[0]
	if d.Kind != "data" || d.TotalBytes <= 0 || d.AvailableBytes <= 0 || d.AvailableBytes > d.TotalBytes || d.Error != "" || d.Partial {
		t.Fatalf("data = %+v", d)
	}
	want := []ServerDirEntry{{Name: "gpuworks.db-wal", Bytes: 5000}, {Name: "gpuworks.db", Bytes: 1000}, {Name: "diagnostics", Dir: true, Bytes: 500}}
	if len(d.Entries) != len(want) {
		t.Fatalf("entries = %+v, want %+v", d.Entries, want)
	}
	for i := range want {
		if d.Entries[i] != want[i] {
			t.Fatalf("entries = %+v, want %+v (largest first)", d.Entries, want)
		}
	}
	if s := got.Dirs[1]; s.Kind != "sqlite_tmp" || s.TotalBytes <= 0 || len(s.Entries) != 0 {
		t.Fatalf("sqlite_tmp = %+v, want its room but not the shared directory's files", s)
	}
	if b := got.Dirs[2]; b.Error == "" || b.TotalBytes != 0 || len(b.Entries) != 0 {
		t.Fatalf("missing dir = %+v, want its error", b)
	}
	if !strings.Contains(buf.String(), `"available_bytes"`) {
		t.Fatalf("wire names = %s", buf.String())
	}
}

func TestServerDiskStopsWalkingAtItsBudget(t *testing.T) {
	data := t.TempDir()
	for i := 0; i < 5; i++ {
		writeSized(t, filepath.Join(data, "many", "f"+string(rune('a'+i))), 1)
	}
	budget := 3
	entries, partial := dirEntries(data, &budget)
	if !partial || len(entries) != 1 || entries[0].Name != "many" || entries[0].Bytes >= 5 {
		t.Fatalf("entries = %+v partial = %v, want a lower bound marked partial", entries, partial)
	}
}

func TestSQLiteTempDirFollowsSQLitesOrder(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("SQLite's Unix order")
	}
	own, other := t.TempDir(), t.TempDir()
	t.Setenv("SQLITE_TMPDIR", own)
	t.Setenv("TMPDIR", other)
	if got := SQLiteTempDir(); got != own {
		t.Fatalf("with SQLITE_TMPDIR: %q, want %q", got, own)
	}
	t.Setenv("SQLITE_TMPDIR", filepath.Join(own, "missing"))
	if got := SQLiteTempDir(); got != other {
		t.Fatalf("with a missing SQLITE_TMPDIR: %q, want TMPDIR %q", got, other)
	}
	t.Setenv("SQLITE_TMPDIR", "")
	t.Setenv("TMPDIR", "")
	if got := SQLiteTempDir(); got != "/var/tmp" && got != "/usr/tmp" && got != "/tmp" && got != "." {
		t.Fatalf("without either: %q", got)
	}
}
