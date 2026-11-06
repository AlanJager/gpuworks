package serverconfig_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

func writeOutboundConfig(t *testing.T, path, body string) {
	t.Helper()
	writeRawOutboundConfig(t, path, "outbound_restrict_private_hosts: true\n"+body)
}

func writeRawOutboundConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertOutboundHosts(t *testing.T, read func() []string, want ...string) {
	t.Helper()
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("private hosts = %v, want %v", got, want)
	}
}

func TestOutboundPrivateHostsWatchReflectsFileEditsAndRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	writeOutboundConfig(t, path, "outbound_allow_private_hosts: [first.internal]\n")
	read := serverconfig.WatchOutboundPrivateHosts(path, true, []string{"startup.internal"}, nil)
	assertOutboundHosts(t, read, "first.internal")
	writeOutboundConfig(t, path, "outbound_allow_private_hosts: [second.internal, 10.2.0.0/16]\n")
	assertOutboundHosts(t, read, "second.internal", "10.2.0.0/16")
	writeOutboundConfig(t, path, "outbound_allow_private_hosts: []\n")
	assertOutboundHosts(t, read)
	writeOutboundConfig(t, path, "data_dir: /srv/gpuworks\n")
	assertOutboundHosts(t, read)
}

func TestOutboundPrivateHostsWatchSeesSameLengthAtomicReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.yaml")
	writeOutboundConfig(t, path, "outbound_allow_private_hosts: [first.internal]\n")
	read := serverconfig.WatchOutboundPrivateHosts(path, true, nil, nil)
	assertOutboundHosts(t, read, "first.internal")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "replacement.yaml")
	writeOutboundConfig(t, replacement, "outbound_allow_private_hosts: [other.internal]\n")
	if err := os.Chtimes(replacement, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("fixture must retain length and mtime")
	}
	assertOutboundHosts(t, read, "other.internal")
}

func TestOutboundPrivateHostsWatchFailsClosedAndRecovers(t *testing.T) {
	for _, scenario := range []string{"missing", "malformed", "unknown-field", "unreadable"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.yaml")
			writeOutboundConfig(t, path, "outbound_allow_private_hosts: [first.internal]\n")
			var reports atomic.Int32
			read := serverconfig.WatchOutboundPrivateHosts(path, true, []string{"startup.internal"}, func(error) { reports.Add(1) })
			assertOutboundHosts(t, read, "first.internal")
			switch scenario {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				writeOutboundConfig(t, path, "outbound_allow_private_hosts: [unterminated\n")
			case "unknown-field":
				writeOutboundConfig(t, path, "outbound_allow_private_hosts: [first.internal]\nmisspelled: true\n")
			case "unreadable":
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0600) })
				if _, err := os.ReadFile(path); err == nil {
					t.Skip("current identity can read mode-000 files")
				}
			}
			assertOutboundHosts(t, read)
			assertOutboundHosts(t, read)
			if reports.Load() != 1 {
				t.Fatalf("unchanged failure reports=%d, want 1", reports.Load())
			}
			if scenario == "unreadable" {
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			writeOutboundConfig(t, path, "outbound_allow_private_hosts: [restored.internal]\n")
			assertOutboundHosts(t, read, "restored.internal")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			assertOutboundHosts(t, read)
			if reports.Load() != 2 {
				t.Fatalf("new failure after recovery reports=%d, want 2", reports.Load())
			}
		})
	}
}

func TestOutboundPrivateHostsWatchWithoutFileCopiesFallback(t *testing.T) {
	initial := []string{"first.internal"}
	read := serverconfig.WatchOutboundPrivateHosts("", true, initial, nil)
	initial[0] = "changed.internal"
	assertOutboundHosts(t, read, "first.internal")
	result := read()
	result[0] = "caller.internal"
	assertOutboundHosts(t, read, "first.internal")
}

func TestOutboundPrivateHostsWatchCopiesFileResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	writeOutboundConfig(t, path, "outbound_allow_private_hosts: [first.internal]\n")
	read := serverconfig.WatchOutboundPrivateHosts(path, true, nil, nil)
	result := read()
	if len(result) != 1 {
		t.Fatalf("private hosts=%v", result)
	}
	result[0] = "caller.internal"
	assertOutboundHosts(t, read, "first.internal")
}

func TestOutboundPrivateHostsWatchConcurrentReaders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.yaml")
	writeOutboundConfig(t, path, "outbound_allow_private_hosts: [first.internal]\n")
	read := serverconfig.WatchOutboundPrivateHosts(path, true, nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				got := read()
				if len(got) > 0 && got[0] != "first.internal" && got[0] != "other.internal" {
					t.Errorf("unexpected policy %v", got)
				}
			}
		}()
	}
	for i := 0; i < 10; i++ {
		replacement := filepath.Join(dir, "replacement.yaml")
		value := "first.internal"
		if i%2 == 0 {
			value = "other.internal"
		}
		writeOutboundConfig(t, replacement, "outbound_allow_private_hosts: ["+value+"]\n")
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	assertOutboundHosts(t, read, "first.internal")
}

func TestOutboundPrivateHostsWatchNeverUsesFallbackForUnavailableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	read := serverconfig.WatchOutboundPrivateHosts(path, true, []string{"startup.internal"}, nil)
	assertOutboundHosts(t, read)
	writeOutboundConfig(t, path, "outbound_allow_private_hosts: [unterminated\n")
	assertOutboundHosts(t, read)
	writeOutboundConfig(t, path, "outbound_allow_private_hosts: [restored.internal]\n")
	assertOutboundHosts(t, read, "restored.internal")
}

func TestOutboundPrivateHostsWatchReportsOutsideTheReadLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	var reports atomic.Int32
	var read func() []string
	read = serverconfig.WatchOutboundPrivateHosts(path, true, nil, func(error) {
		reports.Add(1)
		if got := read(); len(got) != 0 {
			t.Errorf("recursive failed read returned grants: %v", got)
		}
	})
	done := make(chan struct{})
	go func() { defer close(done); read() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("error callback cannot read the current policy")
	}
	if reports.Load() != 1 {
		t.Fatalf("recursive failure reports=%d, want 1", reports.Load())
	}
}

func TestOutboundPrivateHostsWatchReflectsRevocationWithoutMetadataChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	initial := "outbound_allow_private_hosts: [private.internal]\n"
	writeOutboundConfig(t, path, initial)
	read := serverconfig.WatchOutboundPrivateHosts(path, true, nil, nil)
	assertOutboundHosts(t, read, "private.internal")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	head := "outbound_allow_private_hosts: []"
	revoked := head + strings.Repeat(" ", len(initial)-len(head)-1) + "\n"
	writeOutboundConfig(t, path, revoked)
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
		t.Fatal("fixture must retain inode, length, mtime, and mode")
	}
	assertOutboundHosts(t, read)
}

func TestOutboundPrivateHostsWatchRejectsOversizedFileAndRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	writeOutboundConfig(t, path, "outbound_allow_private_hosts: [first.internal]\n")
	var reports atomic.Int32
	read := serverconfig.WatchOutboundPrivateHosts(path, true, nil, func(error) { reports.Add(1) })
	assertOutboundHosts(t, read, "first.internal")
	// Static startup loading remains compatible with valid large files; only
	// the repeated runtime security read has a bounded allocation budget.
	large := strings.Repeat("# padding\n", 220000) + "outbound_allow_private_hosts: [first.internal]\n"
	writeOutboundConfig(t, path, large)
	if _, err := serverconfig.Load(path); err != nil {
		t.Fatalf("static Load changed: %v", err)
	}
	assertOutboundHosts(t, read)
	assertOutboundHosts(t, read)
	if reports.Load() != 1 {
		t.Fatalf("oversized failure reports=%d, want 1", reports.Load())
	}
	writeOutboundConfig(t, path, "outbound_allow_private_hosts: [restored.internal]\n")
	assertOutboundHosts(t, read, "restored.internal")
}
