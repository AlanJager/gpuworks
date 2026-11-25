// Release frames are build inputs. Once Prepare returns, consumers open plain
// files from the verified generation; no dictionary or decoder is needed.
package agentbin

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const packDir = "dl/pack/"

// Prepare opens the generation belonging to this build. Old and developer
// builds with only loose assets retain their original lookup behavior.
func Prepare(dataDir string) (Files, error) {
	return bundled.prepare(dataDir)
}

func (f Files) prepare(dataDir string) (Files, error) {
	baseFile, err := fs.ReadFile(f.tree, packDir+"base")
	if errors.Is(err, fs.ErrNotExist) {
		if _, dirErr := fs.Stat(f.tree, strings.TrimSuffix(packDir, "/")); errors.Is(dirErr, fs.ErrNotExist) {
			return f, nil
		}
	}
	if err != nil {
		return Files{}, err
	}
	manifest, err := fs.ReadFile(f.tree, "dl/"+ManifestName)
	if err != nil {
		return Files{}, err
	}
	var m Manifest
	if err = json.Unmarshal(manifest, &m); err != nil {
		return Files{}, err
	}
	base := strings.TrimSpace(string(baseFile))
	if _, ok := m.Binaries[base]; !ok {
		return Files{}, fmt.Errorf("base agent %q missing from manifest", base)
	}
	dir := filepath.Join(dataDir, "agent-cache", fmt.Sprintf("%x", sha256.Sum256(manifest)))
	if err = os.MkdirAll(dir, 0700); err != nil {
		return Files{}, err
	}
	var dict []byte
	// Prepare the base first. Read it as a dictionary only on a cache miss.
	ensure := func(name string) error {
		entry, err := m.entry(name)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, name)
		if matchesFile(path, entry) {
			return nil
		}
		if name != base && dict == nil {
			dict, err = os.ReadFile(filepath.Join(dir, base))
			if err != nil {
				return err
			}
		}
		raw, decodeErr := f.expand(name, dict, entry)
		if decodeErr != nil {
			// A verified same-build override can recover a damaged packed asset.
			raw, err = f.ReadVerified(filepath.Join(dataDir, "dl"), name, m.Version)
			if err != nil || !matchesBytes(raw, entry) {
				return fmt.Errorf("prepare %s: %w", name, decodeErr)
			}
		}
		return writeAtomic(path, raw)
	}
	if err := ensure(base); err != nil {
		return Files{}, err
	}
	for name := range m.Binaries {
		if name != base {
			if err := ensure(name); err != nil {
				return Files{}, err
			}
		}
	}
	// Only a fully verified generation is handed to any caller.
	return Files{preparedTree{f.tree, os.DirFS(dir), m.Binaries}}, nil
}

// Keep loose embedded files (including the public manifest and developer
// fixtures) ahead of the generated files. Only manifest members are exposed.
type preparedTree struct {
	loose   fs.FS
	cache   fs.FS
	entries map[string]ManifestBinary
}

func (p preparedTree) Open(path string) (fs.File, error) {
	file, err := p.loose.Open(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return file, err
	}
	name, ok := strings.CutPrefix(path, "dl/")
	if _, listed := p.entries[name]; !ok || !listed {
		return nil, err
	}
	return p.cache.Open(name)
}

func (f Files) expand(name string, dict []byte, entry ManifestBinary) ([]byte, error) {
	frame, err := fs.ReadFile(f.tree, packDir+name+compressedSuffix)
	if err != nil {
		return nil, err
	}
	opts := []zstd.DOption{zstd.WithDecoderConcurrency(1)}
	if dict != nil {
		opts = append(opts, zstd.WithDecoderDictRaw(0, dict))
	}
	dec, err := zstd.NewReader(bytes.NewReader(frame), opts...)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	raw, err := io.ReadAll(io.LimitReader(dec, entry.Size+1))
	if err != nil {
		return nil, err
	}
	if !matchesBytes(raw, entry) {
		return nil, fmt.Errorf("expanded %s does not match its manifest", name)
	}
	return raw, nil
}

func matchesBytes(raw []byte, entry ManifestBinary) bool {
	return int64(len(raw)) == entry.Size && strings.EqualFold(fmt.Sprintf("%x", sha256.Sum256(raw)), entry.SHA256)
}

func matchesFile(path string, entry ManifestBinary) bool {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() != entry.Size {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, f)
	return err == nil && strings.EqualFold(fmt.Sprintf("%x", h.Sum(nil)), entry.SHA256)
}

func writeAtomic(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".prepare-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
