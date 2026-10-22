package storagefiles

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// IncomingMaxAge is how long an upload's staging directory may sit untouched
// before a sweep deletes it: a leftover of an agent that died mid-upload.
const IncomingMaxAge = 24 * time.Hour

// SweepIncoming deletes the upload staging directories below root
// (.gpuworks/incoming/*/upload-*) that nothing has written to for longer than
// olderThan, judged by the directory and its direct entries, so an upload
// still writing its body is left alone. Other staging (model files) is not
// touched. It returns how many were removed and the first error met; a
// missing incoming directory is not an error. It touches a shared path, so
// callers bound it with a deadline.
func SweepIncoming(root string, olderThan time.Duration, now time.Time) (int, error) {
	incoming := filepath.Join(root, proto.StorageMetaDir, proto.StorageIncomingDir)
	planes, err := os.ReadDir(incoming)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var first error
	keep := func(err error) {
		if first == nil && err != nil && !os.IsNotExist(err) {
			first = err
		}
	}
	removed := 0
	for _, plane := range planes {
		if !plane.IsDir() {
			continue
		}
		dir := filepath.Join(incoming, plane.Name())
		ents, err := os.ReadDir(dir)
		if err != nil {
			keep(err)
			continue
		}
		for _, ent := range ents {
			if !strings.HasPrefix(ent.Name(), "upload-") {
				continue
			}
			p := filepath.Join(dir, ent.Name())
			latest, err := lastWrite(p)
			if err != nil {
				keep(err)
				continue
			}
			if now.Sub(latest) <= olderThan {
				continue
			}
			if err := os.RemoveAll(p); err != nil {
				keep(err)
				continue
			}
			removed++
		}
	}
	return removed, first
}

// lastWrite is the newest modification time of p and, for a directory, of
// its direct entries.
func lastWrite(p string) (time.Time, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return time.Time{}, err
	}
	latest := info.ModTime()
	if !info.IsDir() {
		return latest, nil
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		return time.Time{}, err
	}
	for _, ent := range ents {
		if fi, err := ent.Info(); err == nil && fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest, nil
}
