// Package agentbin embeds the cross-compiled agent binaries so the single server
// binary can serve them at /dl/ for the curl|sh install flow (no separate
// download host). The Makefile cross-compiles the agents and stores each in
// dl/ zstd-compressed (<name>.zst): three agents are most of the server's
// size, and a release archive's gzip shrinks them less than zstd does. A
// plain <name> in dl/ (a test's own build) is served as it is. A .gitkeep keeps
// the dir present so go:embed always compiles.
package agentbin

import (
	"bytes"
	"embed"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

//go:embed all:dl
var embedded embed.FS

// binaries is what the handlers read: the embedded tree, which tests replace.
var binaries fs.FS = embedded

// compressedSuffix marks an embedded agent stored zstd-compressed.
const compressedSuffix = ".zst"

// Handler serves agent binaries at /dl/<name>. It prefers a real file in
// overrideDir (dev/manual placement); otherwise it serves the embedded copy,
// expanding a compressed one as it streams.
func Handler(overrideDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/dl/")
		if !validName(name) {
			http.NotFound(w, r)
			return
		}
		// Override dir wins (lets ops swap in a patched agent without rebuilding).
		if overrideDir != "" {
			if f, err := os.Open(filepath.Join(overrideDir, name)); err == nil {
				defer f.Close()
				if st, err := f.Stat(); err == nil && !st.IsDir() {
					w.Header().Set("Content-Type", "application/octet-stream")
					http.ServeContent(w, r, name, st.ModTime(), f)
					return
				}
			}
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if raw, err := fs.ReadFile(binaries, "dl/"+name); err == nil {
			w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
			_, _ = w.Write(raw)
			return
		}
		compressed, err := fs.ReadFile(binaries, "dl/"+name+compressedSuffix)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		dec, err := zstd.NewReader(bytes.NewReader(compressed), zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
		if err != nil {
			http.Error(w, "agent binary unreadable", http.StatusInternalServerError)
			return
		}
		defer dec.Close()
		if size, ok := expandedSize(compressed); ok {
			w.Header().Set("Content-Length", strconv.FormatUint(size, 10))
		}
		_, _ = io.Copy(w, dec)
	})
}

// Read returns an agent binary by name, preferring overrideDir over embedded data.
func Read(overrideDir, name string) ([]byte, error) {
	if !validName(name) {
		return nil, fs.ErrNotExist
	}
	if overrideDir != "" {
		path := filepath.Join(overrideDir, name)
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return os.ReadFile(path)
		}
	}
	if raw, err := fs.ReadFile(binaries, "dl/"+name); err == nil {
		return raw, nil
	}
	compressed, err := fs.ReadFile(binaries, "dl/"+name+compressedSuffix)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(compressed, nil)
}

// Has reports whether an embedded agent binary exists (a real build vs the
// placeholder-only tree).
func Has(name string) bool {
	if !validName(name) {
		return false
	}
	for _, path := range []string{"dl/" + name, "dl/" + name + compressedSuffix} {
		if _, err := fs.Stat(binaries, path); err == nil {
			return true
		}
	}
	return false
}

func validName(name string) bool {
	return name != "" && !strings.Contains(name, "..") && !strings.Contains(name, "/") && !strings.HasSuffix(name, compressedSuffix)
}

// expandedSize is the size a zstd frame records for its content, which the
// packer writes, so a download can state its length before it is expanded.
func expandedSize(compressed []byte) (uint64, bool) {
	var h zstd.Header
	if err := h.Decode(compressed); err != nil || !h.HasFCS {
		return 0, false
	}
	return h.FrameContentSize, true
}
