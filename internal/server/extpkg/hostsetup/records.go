package hostsetup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// ErrNotApplied is returned for a package whose host setup was never
// applied on this host, or whose record was removed.
var ErrNotApplied = errors.New("hostsetup: no applied record")

// Records are the applied records, one file per package in Dir holding the
// Digest of the steps root applied. A record counts only when it is owned
// by Owner and writable by no one else, so the service account, which owns
// the plugins directory, cannot forge one.
type Records struct {
	Dir string
	// Owner is the uid a record must belong to: 0 on a real host; a test
	// passes its own.
	Owner int
}

// DefaultRecords are the records under AppliedDir, owned by root.
func DefaultRecords() Records { return Records{Dir: AppliedDir, Owner: 0} }

// File is the name of package id's record in Dir.
func File(id string) (string, error) {
	if !extapi.ValidPackageID(id) {
		return "", fmt.Errorf("hostsetup: %q is not a package id", id)
	}
	return id + appliedSuffix, nil
}

// recordFileID is the package id a record file name belongs to, or "".
func recordFileID(name string) string {
	id, ok := strings.CutSuffix(name, appliedSuffix)
	if !ok || !extapi.ValidPackageID(id) {
		return ""
	}
	return id
}

// Path is package id's record.
func (r Records) Path(id string) (string, error) {
	name, err := File(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.Dir, name), nil
}

// Content is a record's file content for digest.
func Content(d extapi.Digest) []byte { return []byte(string(d) + "\n") }

// Read is the digest package id's record holds: ErrNotApplied when there
// is none, an error when it is not owned by Owner, is writable by group or
// others, or holds no digest.
func (r Records) Read(id string) (extapi.Digest, error) {
	path, err := r.Path(id)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNotApplied
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("hostsetup: %s is not a regular file writable by its owner only", path)
	}
	if uid, ok := ownerOf(info); !ok || uid != r.Owner {
		return "", fmt.Errorf("hostsetup: %s is not owned by uid %d", path, r.Owner)
	}
	buf := make([]byte, 128)
	n, _ := f.Read(buf)
	d := extapi.Digest(strings.TrimSpace(string(buf[:n])))
	if !d.Valid() {
		return "", fmt.Errorf("hostsetup: %s holds no digest", path)
	}
	return d, nil
}

// Matches reports whether package id's applied record holds the digest of
// steps; a package without steps always matches. An unreadable or forged
// record does not match and is returned as the error.
func (r Records) Matches(id string, steps []extapi.HostSetupStep) (bool, error) {
	if len(steps) == 0 {
		return true, nil
	}
	want, err := Digest(steps)
	if err != nil {
		return false, err
	}
	got, err := r.Read(id)
	if errors.Is(err, ErrNotApplied) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return got == want, nil
}
