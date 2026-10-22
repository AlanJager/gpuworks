package store

import (
	"path"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ScanSighting is what one scan page says about a shared storage's directories.
// Covers decides whether the page looked at a relative path at all; Found maps
// the directories it accepted as models to their size; Unsure holds the paths
// it saw but could not judge (unreadable, too deep), which stay as they are.
// A page cannot see everything above it (an earlier page may have stopped at an
// ancestor), so a present file is only marked missing once Gone confirms it.
type ScanSighting struct {
	Covers func(rel string) bool
	Found  map[string]int64
	Unsure map[string]bool
	Gone   func(rel string) bool
}

// notEntered reports whether the scan stopped above rel: at a directory it
// could not judge, or at a model directory, which it does not walk into.
func (seen ScanSighting) notEntered(rel string) bool {
	for dir := path.Dir(rel); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if _, found := seen.Found[dir]; found || seen.Unsure[dir] {
			return true
		}
	}
	return false
}

// ScanChanges counts what ApplyStorageScan changed.
type ScanChanges struct {
	Missing, Restored, Resized int
}

// MessageMissingInStorage is the state message of a shared model file whose
// directory a scan no longer found, e.g. after another control plane deleted it.
const MessageMissingInStorage = "the directory is no longer on the storage"

// ApplyStorageScan reconciles a shared storage's model files with a scan page:
// a present file whose directory the page covered but did not find becomes
// missing, a missing one that is found again is present, and a changed size is
// recorded. Files still being written (any other state) are left alone, since
// their directory appears only when the write completes.
func (s *Store) ApplyStorageScan(storageID uint, seen ScanSighting) (ScanChanges, error) {
	var out ScanChanges
	var rows []ModelFile
	if err := s.db.Where("storage_id = ? AND node_id = 0 AND state IN ?", storageID,
		[]string{proto.MFPresent, proto.MFMissing}).Find(&rows).Error; err != nil {
		return out, err
	}
	for _, mf := range rows {
		if seen.Covers == nil || !seen.Covers(mf.Path) || seen.Unsure[mf.Path] || seen.notEntered(mf.Path) {
			continue
		}
		size, found := seen.Found[mf.Path]
		switch {
		case !found && mf.State == proto.MFPresent:
			if seen.Gone == nil || !seen.Gone(mf.Path) {
				continue
			}
			if err := s.SetModelFileState(mf.ID, proto.MFMissing, MessageMissingInStorage); err != nil {
				return out, err
			}
			out.Missing++
		case found && mf.State == proto.MFMissing:
			if err := s.setScannedModelFile(mf.ID, proto.MFPresent, size); err != nil {
				return out, err
			}
			out.Restored++
		case found && size > 0 && size != mf.SizeBytes:
			if err := s.setScannedModelFile(mf.ID, proto.MFPresent, size); err != nil {
				return out, err
			}
			out.Resized++
		}
	}
	return out, nil
}

func (s *Store) setScannedModelFile(id uint, state string, size int64) error {
	updates := map[string]any{"state": state, "message": ""}
	if size > 0 {
		updates["size_bytes"], updates["downloaded"] = size, size
	}
	res := s.db.Model(&ModelFile{}).Where("id = ?", id).Updates(updates)
	if res.Error == nil && res.RowsAffected > 0 {
		s.publishModelFile(id, state)
	}
	return res.Error
}
