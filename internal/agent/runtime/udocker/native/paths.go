package native

import (
	"fmt"
	"hash/maphash"
	"path"
	"strings"
	"sync"
)

// pathTable resolves the one ordering constraint that stops image layers from
// being unpacked in parallel: when several layers carry the same path, the
// highest layer must win.
//
// Each path gets a state guarded by its own mutex. A worker claims a path
// before materializing it; a lower layer that finds a higher claim skips the
// entry entirely, and a higher layer that arrives while a lower one is still
// writing waits for it and then overwrites. Because collisions are rare in
// practice -- engine images overwrite very little between layers -- workers
// almost never block on each other, while the result is identical to a strict
// sequential application.
type pathTable struct {
	seed   maphash.Seed
	shards [pathShards]pathShard
}

const pathShards = 256

type pathShard struct {
	mu sync.Mutex
	m  map[string]*pathState
}

type pathState struct {
	// mu is held exclusively while the entry is being materialized, and shared
	// by every entry below the path while it is being written (holdAncestors),
	// so a higher layer cannot replace a directory under a lower layer's write.
	mu    sync.RWMutex
	layer int32 // highest layer index that has claimed this path
	isDir bool
}

func newPathTable() *pathTable {
	t := &pathTable{seed: maphash.MakeSeed()}
	for i := range t.shards {
		t.shards[i].m = make(map[string]*pathState)
	}
	return t
}

func (t *pathTable) shard(name string) *pathShard {
	return &t.shards[maphash.String(t.seed, name)%pathShards]
}

// claim reserves name for layer. It returns the locked state when the caller
// owns the path and must call release, or nil when a higher layer already owns
// it and the entry must be skipped.
func (t *pathTable) claim(name string, layer int32) *pathState {
	shard := t.shard(name)
	shard.mu.Lock()
	state, ok := shard.m[name]
	if !ok {
		state = &pathState{layer: -1}
		shard.m[name] = state
	}
	shard.mu.Unlock()

	state.mu.Lock()
	if state.layer > layer {
		state.mu.Unlock()
		return nil
	}
	state.layer = layer
	return state
}

func (t *pathTable) release(state *pathState) {
	if state != nil {
		state.mu.Unlock()
	}
}

// claimedLayer reports the highest layer that wrote name, and whether the path
// was ever written at all. The whiteout pass uses it to tell content that a
// whiteout deletes from content a later layer restored.
func (t *pathTable) claimedLayer(name string) (int32, bool) {
	shard := t.shard(name)
	shard.mu.Lock()
	state, ok := shard.m[name]
	shard.mu.Unlock()
	if !ok {
		return 0, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.layer, state.layer >= 0
}

// state returns the state of name, creating it unclaimed (layer -1) when
// nobody has touched the path yet.
func (t *pathTable) state(name string) *pathState {
	shard := t.shard(name)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	state, ok := shard.m[name]
	if !ok {
		state = &pathState{layer: -1}
		shard.m[name] = state
	}
	return state
}

// holdAncestors guards a write of name by layer against a higher layer
// replacing one of its ancestor directories while the write is under way, the
// case of a layer turning /lib into a symlink to /usr/lib while a lower layer
// still carries /lib/old.so. Applied sequentially the higher layer's
// replacement wins and the lower entry disappears with the directory.
//
// It returns shadowed when an ancestor was already replaced by a higher layer
// with something that is not a directory; the entry must then be dropped and
// no lock is held. Otherwise every ancestor is held shared until release, so a
// higher layer that wants to replace one of them (claim takes the path
// exclusively) waits for this write to finish and then removes its result,
// instead of pulling the directory away between the parent being created and
// the file being opened. Ancestors are taken from the root down, the same
// order for every goroutine.
func (t *pathTable) holdAncestors(name string, layer int32) (shadowed bool, release func()) {
	var ancestors []string
	for dir := path.Dir(name); dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
		ancestors = append(ancestors, dir)
	}
	held := make([]*pathState, 0, len(ancestors))
	unlock := func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i].mu.RUnlock()
		}
	}
	for i := len(ancestors) - 1; i >= 0; i-- {
		state := t.state(ancestors[i])
		state.mu.RLock()
		held = append(held, state)
		if state.layer > layer && !state.isDir {
			unlock()
			return true, nil
		}
	}
	return false, unlock
}

// cleanEntryPath normalizes a tar entry name to a root-relative slash path and
// rejects anything that would escape the destination directory.
func cleanEntryPath(name string) (string, error) {
	// Check for an escape on the relative form first: cleaning "/"+name would
	// silently absorb a leading "../" into the root and place the entry
	// somewhere plausible instead of reporting the archive as malformed.
	relative := path.Clean(strings.TrimPrefix(name, "/"))
	if relative == ".." || strings.HasPrefix(relative, "../") {
		return "", fmt.Errorf("entry %q escapes the destination", name)
	}
	if relative == "" || relative == "." {
		return "", errSkipEntry
	}
	return relative, nil
}

// whiteout classifies an entry name against the AUFS whiteout convention that
// Docker layers use: ".wh..wh..opq" hides everything the lower layers put in
// the containing directory, and ".wh.<name>" hides that one sibling.
type whiteout struct {
	target string // path hidden by this marker
	opaque bool   // hide the whole directory's lower-layer contents
}

func parseWhiteout(name string) (whiteout, bool) {
	base := path.Base(name)
	if !strings.HasPrefix(base, whiteoutPrefix) {
		return whiteout{}, false
	}
	dir := path.Dir(name)
	if dir == "." {
		dir = ""
	}
	if base == whiteoutOpaque {
		return whiteout{target: dir, opaque: true}, true
	}
	return whiteout{target: path.Join(dir, strings.TrimPrefix(base, whiteoutPrefix))}, true
}

const (
	whiteoutPrefix = ".wh."
	whiteoutOpaque = ".wh..wh..opq"
)

// excluded mirrors the paths udocker's create passes to tar as --exclude:
// device nodes cannot be created without privileges and udev's device
// directory is regenerated by the host anyway.
func excluded(name string) bool {
	return strings.HasPrefix(name, "dev/") || strings.HasPrefix(name, "etc/udev/devices/")
}
