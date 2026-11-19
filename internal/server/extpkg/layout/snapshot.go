package layout

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// KeepSnapshots is how many snapshots of a package's data are kept; an
// older one is removed once a newer one is complete.
const KeepSnapshots = 2

const (
	snapshotMeta = "snapshot.json"
	snapshotData = "data"
	// restoreDir and displacedDir sit beside data while Restore swaps it:
	// the copy being built, and the data it replaces.
	restoreDir   = ".data-restore" + partialExt
	displacedDir = ".data-displaced" + partialExt
)

// Snapshot describes one copy of a package's data directory, taken before
// an upgrade with the plugin stopped (docs/reference/install-layout.md).
type Snapshot struct {
	// Name is the snapshot's directory under snapshots/; it sorts by Taken.
	Name string `json:"-"`
	// From is the version whose data it holds.
	From string `json:"from"`
	// Marker is the package's data marker version when it was taken, 0
	// when it had none, and MarkerBy the build that wrote that marker: a
	// restore puts the marker back to them.
	Marker   int       `json:"marker,omitempty"`
	MarkerBy string    `json:"marker_by,omitempty"`
	Taken    time.Time `json:"taken"`
}

// SnapshotName is the name of a snapshot of from's data taken at t.
func SnapshotName(from string, t time.Time) string {
	return t.UTC().Format("20060102T150405.000000000Z") + "-" + from
}

// TakeSnapshot copies package id's data directory to snapshots/<s.Name>
// (SnapshotName of s.From and s.Taken when s.Name is empty). The copy is
// written to a .partial directory and renamed into place once complete, so
// a snapshot that exists is whole; then all but the newest KeepSnapshots
// are removed (one that cannot be is tried again the next time). Regular files, directories and symbolic links are copied
// (links as links, never followed); sockets and other special files are
// left out. The plugin must not be running.
func (d Dir) TakeSnapshot(id string, s Snapshot) (Snapshot, error) {
	if s.Name == "" {
		s.Name = SnapshotName(s.From, s.Taken)
	}
	if !validSnapshotName(s.Name) {
		return Snapshot{}, fmt.Errorf("invalid snapshot name %q", s.Name)
	}
	root := d.Snapshots(id)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Snapshot{}, err
	}
	final := filepath.Join(root, s.Name)
	if _, err := os.Lstat(final); err == nil {
		return Snapshot{}, fmt.Errorf("snapshot %s already exists", s.Name)
	}
	partial := final + partialExt
	if err := RemoveAll(partial); err != nil {
		return Snapshot{}, err
	}
	if err := os.Mkdir(partial, 0o700); err != nil {
		return Snapshot{}, err
	}
	fail := func(err error) (Snapshot, error) {
		_ = RemoveAll(partial)
		return Snapshot{}, err
	}
	if err := copyTree(d.Data(id), filepath.Join(partial, snapshotData)); err != nil {
		return fail(err)
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fail(err)
	}
	if err := writeAtomic(filepath.Join(partial, snapshotMeta), append(raw, '\n'), 0o600); err != nil {
		return fail(err)
	}
	if err := os.Rename(partial, final); err != nil {
		return fail(err)
	}
	if err := syncDir(root); err != nil {
		return s, err
	}
	// The snapshot is whole: an older one that cannot be removed now is
	// removed after the next snapshot.
	_ = d.pruneSnapshots(id)
	return s, nil
}

// pruneSnapshots removes all but the newest KeepSnapshots snapshots.
func (d Dir) pruneSnapshots(id string) error {
	list, err := d.ListSnapshots(id)
	if err != nil {
		return err
	}
	var errs []error
	for len(list) > KeepSnapshots {
		errs = append(errs, RemoveAll(filepath.Join(d.Snapshots(id), list[0].Name)))
		list = list[1:]
	}
	return errors.Join(errs...)
}

// ListSnapshots lists package id's complete snapshots, oldest first.
func (d Dir) ListSnapshots(id string) ([]Snapshot, error) {
	entries, err := os.ReadDir(d.Snapshots(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	for _, e := range entries {
		if !e.IsDir() || !validSnapshotName(e.Name()) {
			continue
		}
		s, err := d.readSnapshot(id, e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// FindSnapshot returns package id's snapshot called name.
func (d Dir) FindSnapshot(id, name string) (Snapshot, bool, error) {
	if !validSnapshotName(name) {
		return Snapshot{}, false, nil
	}
	if _, err := os.Stat(filepath.Join(d.Snapshots(id), name)); errors.Is(err, fs.ErrNotExist) {
		return Snapshot{}, false, nil
	}
	s, err := d.readSnapshot(id, name)
	return s, err == nil, err
}

func (d Dir) readSnapshot(id, name string) (Snapshot, error) {
	raw, err := os.ReadFile(filepath.Join(d.Snapshots(id), name, snapshotMeta))
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return Snapshot{}, fmt.Errorf("read snapshot %s of %s: %w", name, id, err)
	}
	s.Name = name
	return s, nil
}

// Restore replaces package id's data directory with the copy in snapshot
// name. The copy is built beside data first; data is then moved aside, the
// copy renamed into its place and the old data removed. A crash between
// the two renames leaves data missing and the old data aside, which
// Cleanup moves back. The plugin must not be running.
func (d Dir) Restore(id, name string) error {
	if !validSnapshotName(name) {
		return fmt.Errorf("invalid snapshot name %q", name)
	}
	src := filepath.Join(d.Snapshots(id), name, snapshotData)
	if _, err := os.Stat(src); err != nil {
		return err
	}
	pkg := d.Package(id)
	building, displaced := filepath.Join(pkg, restoreDir), filepath.Join(pkg, displacedDir)
	if err := RemoveAll(building); err != nil {
		return err
	}
	if err := copyTree(src, building); err != nil {
		_ = RemoveAll(building)
		return err
	}
	if err := RemoveAll(displaced); err != nil {
		return err
	}
	if err := os.Rename(d.Data(id), displaced); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = RemoveAll(building)
		return err
	}
	if err := os.Rename(building, d.Data(id)); err != nil {
		return err
	}
	if err := syncDir(pkg); err != nil {
		return err
	}
	return RemoveAll(displaced)
}

// cleanupSnapshots removes partial snapshots and restore copies, and moves
// back data a restore displaced but did not replace. The partial snapshots
// of a package with an upgrade marker are left alone: an upgrade may be
// copying its data without the directory's lock, and RemoveUpgrade
// removes them once the upgrade ends.
func (d Dir) cleanupSnapshots() error {
	var errs []error
	partial, _ := filepath.Glob(filepath.Join(d.Root, "*", "snapshots", "*"+partialExt))
	partial = slices.DeleteFunc(partial, func(p string) bool {
		_, err := os.Lstat(filepath.Join(filepath.Dir(filepath.Dir(p)), upgradeFile))
		return err == nil
	})
	building, _ := filepath.Glob(filepath.Join(d.Root, "*", restoreDir))
	for _, p := range append(partial, building...) {
		errs = append(errs, RemoveAll(p))
	}
	displaced, _ := filepath.Glob(filepath.Join(d.Root, "*", displacedDir))
	for _, p := range displaced {
		data := filepath.Join(filepath.Dir(p), "data")
		if _, err := os.Lstat(data); errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, os.Rename(p, data))
			continue
		}
		errs = append(errs, RemoveAll(p))
	}
	return errors.Join(errs...)
}

// removePartialSnapshots removes the partial snapshots of package id.
func (d Dir) removePartialSnapshots(id string) error {
	partial, _ := filepath.Glob(filepath.Join(d.Snapshots(id), "*"+partialExt))
	var errs []error
	for _, p := range partial {
		errs = append(errs, RemoveAll(p))
	}
	return errors.Join(errs...)
}

func validSnapshotName(name string) bool {
	return name != "" && !strings.HasPrefix(name, ".") && !strings.HasSuffix(name, partialExt) &&
		!strings.ContainsAny(name, `/\`) && name != ".."
}

// copyTree copies the directory src to dst, which must not exist: regular
// files with their permission bits, directories and symbolic links (as
// links). A missing src copies to an empty dst.
func copyTree(src, dst string) error {
	if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		return os.Mkdir(dst, 0o700)
	}
	return filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := e.Info()
		if err != nil {
			return err
		}
		switch {
		case e.IsDir():
			return os.Mkdir(target, info.Mode().Perm()|0o700)
		case e.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case e.Type().IsRegular():
			return copyFile(p, target, info.Mode().Perm())
		}
		return nil
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
