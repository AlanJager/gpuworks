package store

import (
	"errors"
	"strings"

	"gorm.io/gorm"
)

// PinModelRevision fixes a model registered without a commit to commit, the
// one its only copy - a directory just registered on a node - was downloaded
// at. It does nothing, and reports false, when the model is already pinned,
// when it has copies elsewhere (they may hold other bytes under the branch
// name), or when another model row already has that identity.
func (s *Store) PinModelRevision(id uint, commit string) (bool, error) {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return false, nil
	}
	pinned := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var model Model
		if err := tx.First(&model, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if isCommitRevision(model.Revision) {
			return nil
		}
		var copies int64
		if err := tx.Model(&ModelFile{}).Where("model_id = ?", id).Count(&copies).Error; err != nil {
			return err
		}
		if copies > 1 {
			return nil
		}
		candidate := model
		candidate.Revision = commit
		var clash int64
		if err := modelIdentityQuery(tx.Model(&Model{}), &candidate).Where("id <> ?", id).Count(&clash).Error; err != nil {
			return err
		}
		if clash > 0 {
			return nil
		}
		if err := tx.Model(&Model{}).Where("id = ?", id).Update("revision", commit).Error; err != nil {
			return err
		}
		pinned = true
		return nil
	})
	return pinned, err
}

// isCommitRevision reports whether a revision names a commit (hex, at least
// seven digits) rather than a branch or tag.
func isCommitRevision(revision string) bool {
	revision = strings.TrimSpace(revision)
	if len(revision) < 7 {
		return false
	}
	for _, r := range strings.ToLower(revision) {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
