package app

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ClusterBackupFormat is the archive layout this executable writes and reads.
const ClusterBackupFormat = 1

// clusterBackupManifestName is the first entry of every archive.
const clusterBackupManifestName = "manifest.json"

// clusterBackupDatabaseName is the database image inside an archive.
const clusterBackupDatabaseName = "database/gpuworks.db"

// clusterBackupEnrollment is the directory of the cluster authority and the
// member key, named as a member's data directory names it.
const clusterBackupEnrollment = "cluster-enrollment"

// clusterBackupMaterialDirectories are the directories of files the database
// refers to by digest. They are copied as found; each is checked against the
// manifest on restore, so a file that changed while it was read is detected.
var clusterBackupMaterialDirectories = []string{"catalog-packages", "material-replicas", "model-logos"}

// clusterBackupExcluded names what an archive deliberately leaves out.
var clusterBackupExcluded = []string{
	"batch-files: batch inputs and results stay on the member that holds them",
	"logs.db, metrics.db, stats.db, ux.db: per-member history that is not replicated",
	"agent-upgrades, dl: rebuilt from the executable",
	"the other members' keys: they join the restored cluster again",
}

// ErrClusterBackupInvalid reports an archive that is not a complete cluster
// backup: a missing, extra or altered entry, or a manifest this executable
// does not read.
var ErrClusterBackupInvalid = errors.New("cluster backup archive is invalid")

// ClusterBackupFile is one archived file as the manifest records it.
type ClusterBackupFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ClusterBackupMember is one database member at the time of the backup.
type ClusterBackupMember struct {
	ID      uint64 `json:"id,string"`
	Address string `json:"address"`
	Role    string `json:"role"`
}

// ClusterBackupManifest describes one archive. Member is the member that took
// the backup: the archive carries that member's key, and a restore rebuilds
// the cluster from that member's addresses. The database image lies between
// the leader's barriers IndexFrom and IndexTo of Term. Release is the program
// that wrote the archive, which during an upgrade is ahead of the schema;
// Migrations says which schema the image has.
type ClusterBackupManifest struct {
	Format             int                   `json:"format"`
	CreatedAt          time.Time             `json:"created_at"`
	Release            string                `json:"release"`
	ClusterID          string                `json:"cluster_id"`
	CAFingerprint      string                `json:"ca_fingerprint"`
	Protocol           uint                  `json:"protocol"`
	StorageTier        string                `json:"storage_tier"`
	MemberAddress      string                `json:"member_address"`
	MemberKey          string                `json:"member_key"`
	EnrollmentListen   string                `json:"enrollment_listen"`
	EnrollmentURL      string                `json:"enrollment_url"`
	Members            []ClusterBackupMember `json:"members"`
	Term               uint64                `json:"term"`
	IndexFrom          uint64                `json:"index_from"`
	IndexTo            uint64                `json:"index_to"`
	Migrations         []int                 `json:"migrations"`
	ExtensionMigration map[string][]int      `json:"extension_migrations,omitempty"`
	Files              []ClusterBackupFile   `json:"files"`
	Excluded           []string              `json:"excluded"`
}

// ClusterBackup is an archive on disk and what its manifest says.
type ClusterBackup struct {
	Path     string
	Size     int64
	Manifest ClusterBackupManifest
}

// clusterBackupEntry is one file to archive: its archive name and where it is.
type clusterBackupEntry struct {
	name string
	path string
}

func validClusterBackupName(name string) bool {
	if name == "" || name == clusterBackupManifestName || path.Clean(name) != name || path.IsAbs(name) || strings.HasPrefix(name, "../") || name == ".." || strings.ContainsRune(name, 0) {
		return false
	}
	switch {
	case name == clusterBackupDatabaseName, name == "master.key", name == "server.yaml":
		return true
	case strings.HasPrefix(name, clusterBackupEnrollment+"/"):
		return path.Dir(name) == clusterBackupEnrollment
	}
	for _, directory := range clusterBackupMaterialDirectories {
		if strings.HasPrefix(name, "data/"+directory+"/") {
			return true
		}
	}
	return false
}

// clusterBackupMaterial lists the regular files of the material directories
// under dataDir. A link or special file is refused: an archive holds bytes,
// and restoring a link would point outside the data directory.
func clusterBackupMaterial(dataDir string) ([]clusterBackupEntry, error) {
	var entries []clusterBackupEntry
	for _, directory := range clusterBackupMaterialDirectories {
		root := filepath.Join(dataDir, directory)
		err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil // removed while the directory was read
				}
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("cluster backup refuses %s: not a regular file", current)
			}
			relative, err := filepath.Rel(dataDir, current)
			if err != nil {
				return err
			}
			entries = append(entries, clusterBackupEntry{name: "data/" + filepath.ToSlash(relative), path: current})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries, nil
}

// writeClusterBackupArchive writes the archive beside dest and publishes it
// under dest only when every byte is on disk; a failure leaves no archive and
// no partial file. dest must not exist. manifest.Files is filled here from
// what was actually archived; an entry that vanished meanwhile is left out.
func writeClusterBackupArchive(dest string, manifest ClusterBackupManifest, entries []clusterBackupEntry) (_ ClusterBackupManifest, resultErr error) {
	if _, err := os.Lstat(dest); err == nil {
		return manifest, fmt.Errorf("cluster backup %s already exists", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return manifest, err
	}
	body, err := os.CreateTemp(filepath.Dir(dest), ".cluster-backup-body-")
	if err != nil {
		return manifest, err
	}
	defer func() { resultErr = errors.Join(resultErr, body.Close(), os.Remove(body.Name())) }()
	manifest.Files = nil
	bodyWriter := tar.NewWriter(body)
	for _, entry := range entries {
		if !validClusterBackupName(entry.name) {
			return manifest, fmt.Errorf("cluster backup refuses the entry name %q", entry.name)
		}
		file, err := archiveClusterBackupFile(bodyWriter, entry, manifest.CreatedAt)
		if errors.Is(err, os.ErrNotExist) && strings.HasPrefix(entry.name, "data/") {
			continue
		}
		if err != nil {
			return manifest, err
		}
		manifest.Files = append(manifest.Files, file)
	}
	// The body has no end marker yet: the manifest goes first in the final
	// archive, and the body's entries are appended to it unchanged.
	if err := bodyWriter.Flush(); err != nil {
		return manifest, err
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return manifest, err
	}
	stage, err := os.CreateTemp(filepath.Dir(dest), ".cluster-backup-")
	if err != nil {
		return manifest, err
	}
	stagePath := stage.Name()
	published := false
	defer func() {
		if !published {
			resultErr = errors.Join(resultErr, stage.Close(), os.Remove(stagePath))
		}
	}()
	if err := stage.Chmod(0600); err != nil {
		return manifest, err
	}
	writer := tar.NewWriter(stage)
	header := &tar.Header{Name: clusterBackupManifestName, Mode: 0600, Size: int64(len(encoded)), ModTime: manifest.CreatedAt, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	if err := writer.WriteHeader(header); err != nil {
		return manifest, err
	}
	if _, err := writer.Write(encoded); err != nil {
		return manifest, err
	}
	if err := writer.Flush(); err != nil {
		return manifest, err
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return manifest, err
	}
	if _, err := io.Copy(stage, body); err != nil {
		return manifest, err
	}
	// Two zero blocks end a tar archive.
	if _, err := stage.Write(make([]byte, 1024)); err != nil {
		return manifest, err
	}
	if err := errors.Join(stage.Sync(), stage.Close()); err != nil {
		return manifest, err
	}
	// Link, not rename: an archive another run published under the same name
	// is never replaced.
	if err := os.Link(stagePath, dest); err != nil {
		return manifest, err
	}
	published = true
	if err := os.Remove(stagePath); err != nil {
		return manifest, err
	}
	return manifest, syncClusterBackupDirectory(filepath.Dir(dest))
}

func archiveClusterBackupFile(writer *tar.Writer, entry clusterBackupEntry, at time.Time) (ClusterBackupFile, error) {
	file, err := os.Open(entry.path)
	if err != nil {
		return ClusterBackupFile{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ClusterBackupFile{}, err
	}
	if !info.Mode().IsRegular() {
		return ClusterBackupFile{}, fmt.Errorf("cluster backup refuses %s: not a regular file", entry.path)
	}
	header := &tar.Header{Name: entry.name, Mode: 0600, Size: info.Size(), ModTime: at, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	if err := writer.WriteHeader(header); err != nil {
		return ClusterBackupFile{}, err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(writer, hash), io.LimitReader(file, info.Size()))
	if err != nil {
		return ClusterBackupFile{}, err
	}
	if written != info.Size() {
		return ClusterBackupFile{}, fmt.Errorf("cluster backup read %d of %d bytes of %s: the file shrank while it was read", written, info.Size(), entry.path)
	}
	return ClusterBackupFile{Name: entry.name, Size: written, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func syncClusterBackupDirectory(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(handle.Sync(), handle.Close())
}

// InspectClusterBackup reads the whole archive and checks every entry against
// its manifest: the entries are exactly the manifest's files, in order, with
// the recorded sizes and digests. It changes nothing.
func InspectClusterBackup(archive string) (ClusterBackup, error) {
	return readClusterBackup(archive, "")
}

// readClusterBackup checks the archive as InspectClusterBackup does and, when
// dir is not empty, writes every entry beneath dir as a private file. dir
// must be an existing private directory the caller owns; on failure the
// caller discards it.
func readClusterBackup(archive, dir string) (backup ClusterBackup, resultErr error) {
	file, err := os.Open(archive)
	if err != nil {
		return backup, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return backup, err
	}
	if !info.Mode().IsRegular() {
		return backup, fmt.Errorf("%w: %s is not a regular file", ErrClusterBackupInvalid, archive)
	}
	backup.Path, backup.Size = archive, info.Size()
	reader := tar.NewReader(file)
	header, err := reader.Next()
	if err != nil || header.Name != clusterBackupManifestName || header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > 16<<20 {
		return backup, fmt.Errorf("%w: the first entry is not a manifest", ErrClusterBackupInvalid)
	}
	raw, err := io.ReadAll(io.LimitReader(reader, header.Size))
	if err != nil {
		return backup, fmt.Errorf("%w: %w", ErrClusterBackupInvalid, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&backup.Manifest); err != nil {
		return backup, fmt.Errorf("%w: manifest: %w", ErrClusterBackupInvalid, err)
	}
	if err := validateClusterBackupManifest(backup.Manifest); err != nil {
		return backup, err
	}
	for _, expected := range backup.Manifest.Files {
		header, err := reader.Next()
		if err != nil {
			return backup, fmt.Errorf("%w: %s is missing", ErrClusterBackupInvalid, expected.Name)
		}
		if header.Name != expected.Name || header.Typeflag != tar.TypeReg || header.Size != expected.Size {
			return backup, fmt.Errorf("%w: found %s where the manifest lists %s", ErrClusterBackupInvalid, header.Name, expected.Name)
		}
		var output io.Writer = io.Discard
		var target *os.File
		if dir != "" {
			path := filepath.Join(dir, filepath.FromSlash(expected.Name))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return backup, err
			}
			target, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return backup, err
			}
			output = target
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(output, hash), reader)
		if target != nil {
			copyErr = errors.Join(copyErr, target.Sync(), target.Close())
		}
		if copyErr != nil {
			return backup, copyErr
		}
		if written != expected.Size || hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
			return backup, fmt.Errorf("%w: %s differs from its manifest entry", ErrClusterBackupInvalid, expected.Name)
		}
	}
	if header, err := reader.Next(); err != io.EOF {
		name := "unreadable data"
		if err == nil {
			name = header.Name
		}
		return backup, fmt.Errorf("%w: %s follows the manifest's last file", ErrClusterBackupInvalid, name)
	}
	return backup, nil
}

func validateClusterBackupManifest(manifest ClusterBackupManifest) error {
	fail := func(reason string) error { return fmt.Errorf("%w: manifest: %s", ErrClusterBackupInvalid, reason) }
	if manifest.Format != ClusterBackupFormat {
		return fail(fmt.Sprintf("format %d, this executable reads format %d", manifest.Format, ClusterBackupFormat))
	}
	if manifest.CreatedAt.IsZero() || manifest.ClusterID == "" || manifest.CAFingerprint == "" || manifest.MemberAddress == "" || manifest.MemberKey == "" || manifest.EnrollmentListen == "" {
		return fail("the cluster or member identity is incomplete")
	}
	if len(manifest.Migrations) == 0 || !sort.IntsAreSorted(manifest.Migrations) {
		return fail("the migration list is missing or unordered")
	}
	if manifest.IndexFrom == 0 || manifest.IndexTo < manifest.IndexFrom {
		return fail("the database image has no leader barriers")
	}
	seen := make(map[string]bool, len(manifest.Files))
	required := map[string]bool{clusterBackupDatabaseName: false, "master.key": false}
	for _, name := range []string{"config.json", "ca.pem", "ca.key", "master.binding", "server.pem", "server.key"} {
		required[clusterBackupEnrollment+"/"+name] = false
	}
	for _, file := range manifest.Files {
		if !validClusterBackupName(file.Name) || seen[file.Name] || file.Size < 0 || len(file.SHA256) != 64 {
			return fail(fmt.Sprintf("the entry %q is not one a cluster backup holds", file.Name))
		}
		seen[file.Name] = true
		if _, ok := required[file.Name]; ok {
			required[file.Name] = true
		}
	}
	for name, present := range required {
		if !present {
			return fail(name + " is missing")
		}
	}
	return nil
}

// clusterBackupStamp is the fixed-width time in an archive's name, so names
// order by time.
const clusterBackupStamp = "20060102T150405Z"

func clusterBackupArchiveName(at time.Time) string {
	return "cluster-backup-" + at.UTC().Format(clusterBackupStamp) + ".tar"
}

// ListClusterBackups returns the archives in dir, newest first, each checked
// in full. An archive that fails the check is reported, not skipped: a
// directory with a damaged backup is not one to rely on.
func ListClusterBackups(dir string) ([]ClusterBackup, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type().IsRegular() && strings.HasPrefix(name, "cluster-backup-") && strings.HasSuffix(name, ".tar") {
			names = append(names, name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	backups := make([]ClusterBackup, 0, len(names))
	for _, name := range names {
		backup, err := InspectClusterBackup(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		backups = append(backups, backup)
	}
	return backups, nil
}

// PruneClusterBackups removes all but the keep newest archives in dir and
// returns the removed paths. Only files this code names are considered.
func PruneClusterBackups(dir string, keep int) ([]string, error) {
	if keep < 1 {
		return nil, errors.New("at least one cluster backup is kept")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, "cluster-backup-"), ".tar")
		if _, err := time.Parse(clusterBackupStamp, stamp); err == nil && entry.Type().IsRegular() && clusterBackupArchiveName(mustStamp(stamp)) == name {
			names = append(names, name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	var removed []string
	for _, name := range names[min(keep, len(names)):] {
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			return removed, err
		}
		removed = append(removed, path)
	}
	return removed, nil
}

func mustStamp(stamp string) time.Time {
	at, _ := time.Parse(clusterBackupStamp, stamp)
	return at
}
