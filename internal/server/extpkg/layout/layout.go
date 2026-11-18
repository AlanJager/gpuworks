// Package layout owns the plugin host's part of the data directory
// (docs/reference/install-layout.md):
//
//	<data>/plugins/
//	  <id>/pkg/<version>/       a verified, unpacked package (read-only)
//	  <id>/pkg/current          the active version, replaced by rename
//	  <id>/control.json         Control
//	  <id>/data/  <id>/tmp/     the plugin's storage and temporary files
//	  <id>/snapshots/           snapshots taken before upgrades
//	  .trash/<id>-<time>/       where a purged package goes
//	  .incoming-*.partial/      a package being verified, removed at boot
//
// A package is unpacked into an incoming directory, verified there, renamed
// to its version directory and only then made current, so a crash or a
// refusal never leaves a half-written package in place. The previous
// version stays beside the current one for rollback.
package layout

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg/archive"
)

// DirName is the plugin host's directory inside the data directory.
const DirName = "plugins"

const (
	pkgDir       = "pkg"
	currentFile  = "current"
	controlFile  = "control.json"
	trashDir     = ".trash"
	incomingGlob = ".incoming-*.partial"
	partialExt   = ".partial"
)

// Dir is the plugins directory of one data directory.
type Dir struct{ Root string }

// New returns the plugins directory of dataDir.
func New(dataDir string) Dir { return Dir{Root: filepath.Join(dataDir, DirName)} }

// Package is the directory of package id.
func (d Dir) Package(id string) string { return filepath.Join(d.Root, id) }

// Version is the unpacked package id at version.
func (d Dir) Version(id, version string) string {
	return filepath.Join(d.Root, id, pkgDir, version)
}

// Data, Tmp and Snapshots are the plugin's own directories.
func (d Dir) Data(id string) string      { return filepath.Join(d.Root, id, "data") }
func (d Dir) Tmp(id string) string       { return filepath.Join(d.Root, id, "tmp") }
func (d Dir) Snapshots(id string) string { return filepath.Join(d.Root, id, "snapshots") }

// Current returns the active version of package id, or "" when it has none.
func (d Dir) Current(id string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(d.Root, id, pkgDir, currentFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// Installed returns the ids that have an active version, sorted.
func (d Dir) Installed() ([]string, error) {
	entries, err := os.ReadDir(d.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		v, err := d.Current(e.Name())
		if err != nil {
			return nil, err
		}
		if v != "" {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// Versions returns the unpacked versions of package id, sorted by name.
func (d Dir) Versions(id string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(d.Root, id, pkgDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasSuffix(e.Name(), partialExt) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Cleanup removes what an interrupted install left behind: incoming
// directories and partial versions. The server runs it at boot.
func (d Dir) Cleanup() error {
	var errs []error
	incoming, _ := filepath.Glob(filepath.Join(d.Root, incomingGlob))
	partial, _ := filepath.Glob(filepath.Join(d.Root, "*", pkgDir, "*"+partialExt))
	for _, p := range append(incoming, partial...) {
		errs = append(errs, RemoveAll(p))
	}
	return errors.Join(errs...)
}

// Staging is an incoming package directory; it is an archive.Sink.
type Staging struct {
	dir  string
	done bool
}

// Stage creates an incoming directory.
func (d Dir) Stage() (*Staging, error) {
	if err := os.MkdirAll(d.Root, 0o755); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(d.Root, ".incoming-*"+partialExt)
	if err != nil {
		return nil, err
	}
	return &Staging{dir: dir}, nil
}

// Path is the incoming directory.
func (s *Staging) Path() string { return s.dir }

// Create opens a member for writing inside the incoming directory.
func (s *Staging) Create(path string) (io.WriteCloser, error) {
	if !archive.SafePath(path) {
		return nil, fmt.Errorf("unsafe package member path %q", path)
	}
	full := filepath.Join(s.dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

// WriteSignature stores the signature members beside the files, so the
// installed package can be verified again at boot and before each start.
func (s *Staging) WriteSignature(c *archive.Contents) error {
	for name, body := range map[string][]byte{
		archive.ManifestMember: c.Manifest, archive.SignatureMember: c.Signature, archive.CertificatesMember: c.Certificates,
	} {
		if err := os.WriteFile(filepath.Join(s.dir, name), body, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// Seal makes the incoming package read-only: every file 0444, the ones
// exec names 0555, and every directory 0555 (the top one once committed).
func (s *Staging) Seal(exec map[string]bool) error {
	var dirs []string
	err := filepath.WalkDir(s.dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		rel, err := filepath.Rel(s.dir, p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o444)
		if exec[filepath.ToSlash(rel)] {
			mode = 0o555
		}
		return os.Chmod(p, mode)
	})
	if err != nil {
		return err
	}
	// The top directory stays writable until Commit has moved it: moving a
	// directory to another parent rewrites its own ".." entry.
	for i := len(dirs) - 1; i >= 1; i-- {
		if err := os.Chmod(dirs[i], 0o555); err != nil {
			return err
		}
	}
	return nil
}

// Commit renames the incoming directory to the version directory of id.
// The version directory must not exist.
func (s *Staging) Commit(d Dir, id, version string) error {
	parent := filepath.Join(d.Root, id, pkgDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	final := d.Version(id, version)
	if _, err := os.Lstat(final); err == nil {
		return fmt.Errorf("%s already exists", final)
	}
	if err := os.Rename(s.dir, final); err != nil {
		return err
	}
	s.done = true
	if err := os.Chmod(final, 0o555); err != nil {
		return err
	}
	return syncDir(parent)
}

// Abort removes the incoming directory unless it was committed.
func (s *Staging) Abort() error {
	if s.done {
		return nil
	}
	s.done = true
	return RemoveAll(s.dir)
}

// Activate makes version the active version of id by replacing the current
// file with a rename, creates the plugin's own directories, and keeps only
// the new and the previous version. It returns the previous version.
func (d Dir) Activate(id, version string) (previous string, err error) {
	previous, err = d.Current(id)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(d.Version(id, version)); err != nil {
		return "", err
	}
	for dir, mode := range map[string]os.FileMode{d.Data(id): 0o700, d.Tmp(id): 0o700, d.Snapshots(id): 0o700} {
		if err := os.MkdirAll(dir, mode); err != nil {
			return "", err
		}
	}
	if err := writeAtomic(filepath.Join(d.Root, id, pkgDir, currentFile), []byte(version+"\n"), 0o644); err != nil {
		return "", err
	}
	versions, err := d.Versions(id)
	if err != nil {
		return previous, err
	}
	var errs []error
	for _, v := range versions {
		if v != version && v != previous {
			errs = append(errs, RemoveAll(d.Version(id, v)))
		}
	}
	return previous, errors.Join(errs...)
}

// Remove uninstalls package id. Without purge it removes the package and
// its control file and keeps the plugin's data; with purge it moves the
// whole package directory to .trash.
func (d Dir) Remove(id string, purge bool, now time.Time) error {
	if purge {
		trash := filepath.Join(d.Root, trashDir)
		if err := os.MkdirAll(trash, 0o700); err != nil {
			return err
		}
		base := filepath.Join(trash, id+"-"+now.UTC().Format("20060102T150405Z"))
		dst := base
		for n := 2; ; n++ {
			if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
				break
			}
			dst = fmt.Sprintf("%s.%d", base, n)
		}
		return os.Rename(d.Package(id), dst)
	}
	// The current file goes first: an interrupted removal leaves a package
	// that is simply not installed.
	if err := os.Remove(filepath.Join(d.Root, id, pkgDir, currentFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Remove(filepath.Join(d.Root, id, controlFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return RemoveAll(filepath.Join(d.Root, id, pkgDir))
}

// RemoveAll removes path like os.RemoveAll, first making read-only
// directories writable.
func RemoveAll(path string) error {
	_ = filepath.WalkDir(path, func(p string, e fs.DirEntry, err error) error {
		if err == nil && e.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// Control is a package's control file: whether it is enabled and what the
// administrator approved.
type Control struct {
	Enabled bool `json:"enabled"`
	// ApprovedHash is the approval hash (extpkg.ApprovalHash) approved.
	ApprovedHash string    `json:"approved_hash,omitempty"`
	ApprovedBy   string    `json:"approved_by,omitempty"`
	At           time.Time `json:"at"`
	// HostSetupApplied is not read: whether a package's host setup is
	// applied is the root-owned record of package hostsetup, which the
	// service account, owning this file, could not forge.
	HostSetupApplied bool `json:"host_setup_applied,omitempty"`
}

// ReadControl reads the control file of id; a missing one is the zero
// Control with ok false.
func (d Dir) ReadControl(id string) (c Control, ok bool, err error) {
	raw, err := os.ReadFile(filepath.Join(d.Root, id, controlFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Control{}, false, nil
	}
	if err != nil {
		return Control{}, false, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Control{}, false, fmt.Errorf("read %s control file: %w", id, err)
	}
	return c, true, nil
}

// WriteControl replaces the control file of id.
func (d Dir) WriteControl(id string, c Control) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(d.Root, id, controlFile), append(raw, '\n'), 0o600)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

const indexDir = ".index"

// Saved index files of one publisher.
const (
	IndexFile = "index.json"
	CertsFile = "publisher.crt"
)

// SaveIndex keeps a verified package index of publisher and the
// certificates it was verified with, replacing the previous ones.
func (d Dir) SaveIndex(publisher string, index, certs []byte) error {
	dir := filepath.Join(d.Root, indexDir, publisher)
	if err := writeAtomic(filepath.Join(dir, CertsFile), certs, 0o644); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, IndexFile), index, 0o644)
}

// SavedIndex is one publisher's saved index.
type SavedIndex struct {
	Publisher    string
	Index, Certs []byte
}

// Indexes returns the saved indexes, sorted by publisher.
func (d Dir) Indexes() ([]SavedIndex, error) {
	entries, err := os.ReadDir(filepath.Join(d.Root, indexDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []SavedIndex
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(d.Root, indexDir, e.Name())
		index, err := os.ReadFile(filepath.Join(dir, IndexFile))
		if err != nil {
			return nil, err
		}
		certs, err := os.ReadFile(filepath.Join(dir, CertsFile))
		if err != nil {
			return nil, err
		}
		out = append(out, SavedIndex{Publisher: e.Name(), Index: index, Certs: certs})
	}
	return out, nil
}
