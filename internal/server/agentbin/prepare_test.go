package agentbin

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"

	"sync"
	"testing"
	"testing/fstest"

	"github.com/klauspost/compress/zstd"
)

// Make a real dictionary frame; raw targets are independent references for
// the HTTP, provisioning and verified upgrade readers.
func packedTree(t *testing.T, names ...string) (fstest.MapFS, map[string][]byte) {
	t.Helper()
	tree := fstest.MapFS{packDir + "base": {Data: []byte(names[0])}}
	m := Manifest{Version: "fixture", Binaries: map[string]ManifestBinary{}}
	raw := map[string][]byte{}
	for i, name := range names {
		data := bytes.Repeat([]byte("shared agent instructions and data\n"), 4096)
		copy(data[len(data)/2:], name)
		opts := []zstd.EOption{zstd.WithEncoderConcurrency(1)}
		if i > 0 {
			opts = append(opts, zstd.WithEncoderDictRaw(0, raw[names[0]]))
		}
		enc, err := zstd.NewWriter(nil, opts...)
		if err != nil {
			t.Fatal(err)
		}
		tree[packDir+name+compressedSuffix] = &fstest.MapFile{Data: enc.EncodeAll(data, nil)}
		enc.Close()
		raw[name] = data
		m.Binaries[name] = ManifestBinary{Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	tree["dl/"+ManifestName] = &fstest.MapFile{Data: manifest}
	return tree, raw
}

func TestPreparedAgentsServeVerifiedFilesUntilRestart(t *testing.T) {
	const base, target = "gpuworks-agent-arm", "gpuworks-agent-other"
	tree, raw := packedTree(t, base, target)
	input := Files{tree}
	dir := t.TempDir()
	files, err := input.prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	// No request or warm restart should need to open either packed frame.
	delete(tree, packDir+base+compressedSuffix)
	delete(tree, packDir+target+compressedSuffix)
	files, err = input.prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range raw {
		if !files.Available("", name, "fixture") {
			t.Fatalf("%s unavailable", name)
		}
		r := httptest.NewRecorder()
		files.Handler("").ServeHTTP(r, httptest.NewRequest("GET", "/dl/"+name, nil))
		if r.Code != 200 || r.Header().Get("Content-Length") != strconv.Itoa(len(want)) || !bytes.Equal(r.Body.Bytes(), want) {
			t.Fatalf("%s download mismatch", name)
		}
		got, err := files.ReadVerified("", name, "fixture")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("verified %s: %v", name, err)
		}
	}
	for _, path := range []string{"pack/base", "pack/" + target + compressedSuffix, ".prepare-x"} {
		r := httptest.NewRecorder()
		files.Handler("").ServeHTTP(r, httptest.NewRequest("GET", "/dl/"+path, nil))
		if r.Code != http.StatusNotFound {
			t.Fatalf("private path %s: %d", path, r.Code)
		}
	}
	if _, err := files.ReadVerified("", target, "previous"); err == nil {
		t.Fatal("accepted another build")
	}
}

func TestPreparationRepairsDamageAndSupportsVerifiedRecovery(t *testing.T) {
	const base = "gpuworks-agent-single"
	tree, raw := packedTree(t, base)
	dir := t.TempDir()
	input := Files{tree}
	_, err := input.prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := filepath.Glob(filepath.Join(dir, "agent-cache", "*", base))
	if err != nil || len(cache) != 1 {
		t.Fatalf("cache paths %v: %v", cache, err)
	}
	damage := bytes.Repeat([]byte("!"), len(raw[base]))
	if err := os.WriteFile(cache[0], damage, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := input.prepare(dir); err != nil {
		t.Fatal(err)
	}
	check, err := os.ReadFile(cache[0])
	if err != nil || !bytes.Equal(check, raw[base]) {
		t.Fatal("cache repair failed")
	}
	if err := os.Remove(cache[0]); err != nil {
		t.Fatal(err)
	}
	tree[packDir+base+compressedSuffix].Data = []byte("corrupt")
	if _, err := input.prepare(dir); err == nil {
		t.Fatal("accepted corrupt generation")
	}
	override := filepath.Join(dir, "dl")
	if err := os.Mkdir(override, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(override, base), raw[base], 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := input.prepare(dir); err == nil {
		t.Fatal("trusted unmanifested recovery binary")
	}
	if err := os.WriteFile(filepath.Join(override, ManifestName), tree["dl/"+ManifestName].Data, 0600); err != nil {
		t.Fatal(err)
	}
	files, err := input.prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := files.ReadVerified("", base, "fixture"); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedGenerationsAndConcurrentStartup(t *testing.T) {
	const base, target = "gpuworks-agent-one", "gpuworks-agent-two"
	tree, raw := packedTree(t, base, target)
	dir := t.TempDir()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := (Files{tree}).prepare(dir); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	old, err := (Files{tree}).prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	single, _ := packedTree(t, base)
	files, err := (Files{single}).prepare(dir)
	if err != nil || files.Has(target) {
		t.Fatalf("single-platform generation contains stale target: %v", err)
	}
	if got, err := old.ReadVerified("", target, "fixture"); err != nil || !bytes.Equal(got, raw[target]) {
		t.Fatal("previous generation lost")
	}
	delete(single, packDir+"base")
	if _, err := (Files{single}).prepare(dir); err == nil {
		t.Fatal("accepted a pack without a base declaration")
	}
	loose := Files{fstest.MapFS{"dl/" + target: {Data: []byte("developer fixture")}}}
	dev, err := loose.prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := dev.Read("", target)
	if err != nil || string(got) != "developer fixture" {
		t.Fatal("lost loose build")
	}
	if _, err := fs.Stat(old.tree, "dl/"+target); err != nil {
		t.Fatal(err)
	}
}
