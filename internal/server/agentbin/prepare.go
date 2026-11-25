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
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const packDir = "dl/pack/"
const degradedStartupEnv = "GPUWORKS_ALLOW_DEGRADED_AGENT_STARTUP"

// PrepareForServer applies the operator's startup policy only to recovering
// packed agents. Build tools and explicit verification remain strict.
func PrepareForServer(dataDir string, log *slog.Logger) (Files, error) {
	if log == nil {
		log = slog.Default()
	}
	allow := false
	if v := os.Getenv(degradedStartupEnv); v != "" {
		var err error
		allow, err = strconv.ParseBool(v)
		if err != nil {
			return Files{}, fmt.Errorf("%s: %w", degradedStartupEnv, err)
		}
	}
	files, err := bundled.preparePack(dataDir, allow, log)
	if err == nil {
		return files, nil
	}
	if allow {
		// Without readable pack metadata there is no cache we can trust.
		// Existing loose overrides and their own manifests are still usable.
		log.Error("agent pack metadata unavailable; starting with loose agents", "error", err, "recovery_dir", filepath.Join(dataDir, "dl"))
		return bundled, nil
	}
	log.Error("agent preparation blocked server startup", "error", err)
	return Files{}, err
}

// Prepare opens the generation belonging to this build. Old and developer
// builds with only loose assets retain their original lookup behavior.
func Prepare(dataDir string) (Files, error) {
	return bundled.prepare(dataDir)
}

func (f Files) prepare(dataDir string) (Files, error) {
	return f.preparePack(dataDir, false, slog.Default())
}

func (f Files) preparePack(dataDir string, allowDegraded bool, log *slog.Logger) (Files, error) {
	baseFile, err := fs.ReadFile(f.tree, packDir+"base")
	if errors.Is(err, fs.ErrNotExist) {
		if _, dirErr := fs.Stat(f.tree, strings.TrimSuffix(packDir, "/")); errors.Is(dirErr, fs.ErrNotExist) {
			return f, nil
		}
	}
	baseErr := err
	manifest, err := fs.ReadFile(f.tree, "dl/"+ManifestName)
	if err != nil {
		return Files{}, err
	}
	var m Manifest
	if err = json.Unmarshal(manifest, &m); err != nil {
		return Files{}, err
	}
	base := strings.TrimSpace(string(baseFile))
	_, hasBase := m.Binaries[base]
	if !hasBase && baseErr == nil {
		baseErr = fmt.Errorf("base agent %q missing from manifest", base)
	}
	if baseErr != nil && !allowDegraded {
		return Files{}, baseErr
	}
	if len(m.Binaries) == 0 {
		return Files{}, errors.New("empty agent build manifest")
	}
	dir := filepath.Join(dataDir, "agent-cache", fmt.Sprintf("%x", sha256.Sum256(manifest)))
	cacheErr := os.MkdirAll(dir, 0700)
	recoveryDir := filepath.Join(dataDir, "dl")
	missing := make(map[string]bool)
	var dict []byte
	// Prepare the base first. Read it as a dictionary only on a cache miss.
	ensure := func(name string, entry ManifestBinary) error {
		if cacheErr != nil {
			return cacheErr
		}
		path := filepath.Join(dir, name)
		if matchesFile(path, entry) {
			return nil
		}
		if baseErr != nil {
			return baseErr
		}
		if name != base && missing[base] && dict == nil {
			return fmt.Errorf("reference agent %s unavailable", base)
		}
		if name != base && dict == nil {
			dict, err = os.ReadFile(filepath.Join(dir, base))
			if err != nil {
				return err
			}
		}
		raw, err := f.expand(name, dict, entry)
		if err != nil {
			return err
		}
		return writeAtomic(path, raw)
	}
	names := make([]string, 0, len(m.Binaries))
	if hasBase {
		names = append(names, base)
	}
	for name := range m.Binaries {
		if name != base {
			names = append(names, name)
		}
	}
	for _, name := range names {
		entry, entryErr := m.entry(name)
		if entryErr != nil {
			return Files{}, entryErr
		}
		cause := ensure(name, entry)
		if cause == nil {
			continue
		}
		path := filepath.Join(recoveryDir, name)
		if matchesFile(path, entry) {
			// A manually restored executable is already complete. Use it
			// directly if cache storage is unavailable, including as a base.
			raw, err := os.ReadFile(path)
			if err == nil && matchesBytes(raw, entry) {
				if writeErr := writeAtomic(filepath.Join(dir, name), raw); writeErr != nil {
					missing[name] = true
					if name == base {
						dict = raw
					}
				}
				log.Warn("agent restoration used verified recovery file", "agent", name, "version", m.Version, "path", path, "error", cause)
				continue
			}
		}
		log.Error("agent restoration failed", "agent", name, "version", m.Version, "allow_degraded_startup", allowDegraded, "error", cause, "recovery_file", path, "expected_sha256", entry.SHA256)
		if !allowDegraded {
			return Files{}, fmt.Errorf("prepare %s: %w", name, cause)
		}
		missing[name] = true
	}
	return Files{preparedTree{loose: f.tree, cache: os.DirFS(dir), entries: m.Binaries, missing: missing, recoveryDir: recoveryDir}}, nil
}

// Keep loose embedded files (including the public manifest and developer
// fixtures) ahead of the generated files. Only manifest members are exposed.
type preparedTree struct {
	loose       fs.FS
	cache       fs.FS
	entries     map[string]ManifestBinary
	missing     map[string]bool
	recoveryDir string
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
	if p.missing[name] {
		return p.openRecovery(name)
	}
	return p.cache.Open(name)
}

// The override of an unavailable agent must be the exact release executable.
var errAgentUnavailable = errors.New("agent unavailable")

func (p preparedTree) openRecovery(name string) (fs.File, error) {
	f, err := os.Open(filepath.Join(p.recoveryDir, name))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errAgentUnavailable, name)
	}
	entry := p.entries[name]
	if !matchesOpenFile(f, entry) {
		f.Close()
		return nil, fmt.Errorf("%w: recovery file for %s differs from release manifest", errAgentUnavailable, name)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Route degraded reads through openRecovery for downloads, upgrades and the
// offline installer, even when a caller would normally prefer a loose override.
func (f Files) overrideDir(dir, name string) string {
	if p, ok := f.tree.(preparedTree); ok && p.missing[name] {
		return ""
	}
	return dir
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
	return matchesOpenFile(f, entry)
}

func matchesOpenFile(f *os.File, entry ManifestBinary) bool {
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != entry.Size {
		return false
	}
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
