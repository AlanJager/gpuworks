package storagemount

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hbPath(root, id string) string {
	return filepath.Join(root, ".gpuworks", "control-planes", id+".json")
}

// setAge makes id's file look `age` older than ref, as the file server's clock would stamp it.
func setAge(t *testing.T, root, id string, ref time.Time, age time.Duration) {
	t.Helper()
	m := ref.Add(-age)
	if err := os.Chtimes(hbPath(root, id), m, m); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatWriteReadRemove(t *testing.T) {
	root := t.TempDir()
	mt, err := WriteHeartbeat(root, Heartbeat{ID: "cp-self", Name: "Self", Version: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(hbPath(root, "cp-self")); !fi.ModTime().Equal(mt) {
		t.Errorf("returned mtime %v != file mtime %v", mt, fi.ModTime())
	}
	if fi, _ := os.Stat(hbPath(root, "cp-self")); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644 so other nodes can read it", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(hbPath(root, "cp-self"))
	if string(b) != `{"id":"cp-self","name":"Self","version":"1.2.3"}` {
		t.Errorf("wire content = %s", b)
	}
	if _, err := WriteHeartbeat(root, Heartbeat{ID: "peer1", Name: "Peer One", Version: "1.2.2"}); err != nil {
		t.Fatal(err)
	}
	// Rewriting replaces in place and leaves no temp file behind.
	if _, err := WriteHeartbeat(root, Heartbeat{ID: "peer1", Name: "Peer One", Version: "1.2.4"}); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(filepath.Dir(hbPath(root, "x")))
	if len(ents) != 2 {
		t.Errorf("directory has %d entries, want 2: %v", len(ents), ents)
	}

	peers, err := ReadPeers(root, "cp-self", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].ID != "peer1" || peers[0].Name != "Peer One" || peers[0].Version != "1.2.4" {
		t.Fatalf("peers = %+v", peers)
	}

	if err := RemoveHeartbeat(root, "cp-self"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveHeartbeat(root, "cp-self"); err != nil {
		t.Errorf("removing a missing heartbeat must be fine, got %v", err)
	}
	if _, err := ReadPeers(root, "cp-self", time.Minute); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadPeers without own file = %v, want a not-exist error", err)
	}
}

func TestHeartbeatRejectsBadIDs(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"", "../x", "a/b", "a.b", "a b", "a\x00b", strings.Repeat("a", 65), "x.json", ".."} {
		if _, err := WriteHeartbeat(root, Heartbeat{ID: id}); err == nil {
			t.Errorf("WriteHeartbeat(%q) must fail", id)
		}
		if _, err := ReadPeers(root, id, time.Minute); err == nil {
			t.Errorf("ReadPeers(%q) must fail", id)
		}
		if err := RemoveHeartbeat(root, id); err == nil {
			t.Errorf("RemoveHeartbeat(%q) must fail", id)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".gpuworks")); err == nil {
		t.Error("rejected ids must not create anything")
	}
	if _, err := WriteHeartbeat(root, Heartbeat{ID: strings.Repeat("a", 64)}); err != nil {
		t.Errorf("64 chars is allowed: %v", err)
	}
}

func TestReadPeersUsesFileServerClock(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"self", "fresh", "stale"} {
		if _, err := WriteHeartbeat(root, Heartbeat{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	// Whatever the local clock says, only differences between the files matter. Put the whole
	// group an hour in the past and in the future of the local clock: the result must not change.
	for _, shift := range []time.Duration{0, -time.Hour, time.Hour, -48 * time.Hour} {
		ref := time.Now().Add(shift).Truncate(time.Second)
		setAge(t, root, "self", ref, 0)
		setAge(t, root, "fresh", ref, 5*time.Minute)
		setAge(t, root, "stale", ref, 3*time.Hour)
		peers, err := ReadPeers(root, "self", 30*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if len(peers) != 2 {
			t.Fatalf("shift %v: %+v", shift, peers)
		}
		f, s := peers[0], peers[1] // sorted by id: fresh, stale
		if f.ID != "fresh" || !f.Fresh || f.LastSeen != 5*time.Minute {
			t.Errorf("shift %v: fresh = %+v", shift, f)
		}
		if s.ID != "stale" || s.Fresh || s.LastSeen != 3*time.Hour {
			t.Errorf("shift %v: stale = %+v", shift, s)
		}
	}
}

func TestReadPeersThirtyMinuteBoundary(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"self", "edge", "over", "under", "future"} {
		if _, err := WriteHeartbeat(root, Heartbeat{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	ref := time.Now().Add(-time.Hour).Truncate(time.Second)
	setAge(t, root, "self", ref, 0)
	setAge(t, root, "edge", ref, 30*time.Minute)
	setAge(t, root, "over", ref, 30*time.Minute+time.Second)
	setAge(t, root, "under", ref, 30*time.Minute-time.Second)
	setAge(t, root, "future", ref, -10*time.Second) // newer than self: clamps to zero age
	peers, err := ReadPeers(root, "self", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Peer{}
	for _, p := range peers {
		got[p.ID] = p
	}
	for id, fresh := range map[string]bool{"edge": true, "under": true, "future": true, "over": false} {
		if got[id].Fresh != fresh {
			t.Errorf("%s: Fresh = %v, want %v (%+v)", id, got[id].Fresh, fresh, got[id])
		}
	}
	if got["future"].LastSeen != 0 {
		t.Errorf("future LastSeen = %v, want 0", got["future"].LastSeen)
	}
}

func TestReadPeersIgnoresJunk(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"self", "good"} {
		if _, err := WriteHeartbeat(root, Heartbeat{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Dir(hbPath(root, "x"))
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("corrupt.json", "{not json")
	write("empty.json", "")
	write("liar.json", `{"id":"someone-else","name":"x"}`)
	write(".good.json.tmp-123", `{"id":"good","name":"half written"}`)
	write("notes.txt", `{"id":"notes"}`)
	write("bad name.json", `{"id":"bad name"}`)
	if err := os.Mkdir(filepath.Join(dir, "adir.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	peers, err := ReadPeers(root, "self", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].ID != "good" {
		t.Fatalf("only the intact peer must be listed, got %+v", peers)
	}
}
