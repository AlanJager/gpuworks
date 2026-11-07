package store

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ErrModelSourceIDFixed: the model's source id names its public repository,
// so changing it would describe a different model.
var ErrModelSourceIDFixed = errors.New("the source id of a model from a public repository cannot be changed")

// ErrModelTransferActive: a copy, download or eviction of the model is in
// flight; its target path was derived from the current source id.
var ErrModelTransferActive = errors.New("a transfer of this model is in progress")

// ModelSourceIDTakenError: another model already has the identity the change
// would give this one.
type ModelSourceIDTakenError struct{ ModelID uint }

func (e *ModelSourceIDTakenError) Error() string {
	return fmt.Sprintf("model %d already has this source id", e.ModelID)
}

// editableSourceIDSources are the sources whose source id is a label the user
// chose at registration (a local directory, an HTTP directory) rather than a
// repository identity.
var editableSourceIDSources = map[string]bool{"local": true, "url": true}

// ModelSourceIDEditable reports whether a model's source id may be changed.
func ModelSourceIDEditable(m *Model) bool {
	return m != nil && editableSourceIDSources[strings.ToLower(strings.TrimSpace(m.Source))]
}

// modelTransferStates are the file states whose target path is still being
// written from the source id (see ModelArtifactPath).
var modelTransferStates = []string{proto.MFPending, proto.MFDownloading, proto.MFSyncing, proto.MFDeleting}

// SetModelSourceID renames a local or URL model's source id. Paths already
// recorded on its files are kept; later copies derive theirs from the new id.
// changed is false when the id was already the requested one.
func (s *Store) SetModelSourceID(id uint, sourceID string) (model *Model, changed bool, err error) {
	sourceID = strings.Trim(strings.TrimSpace(sourceID), "/")
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var current Model
		if err := tx.First(&current, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if !ModelSourceIDEditable(&current) {
			return ErrModelSourceIDFixed
		}
		if current.SourceID == sourceID {
			return nil
		}
		var taken Model
		err := modelIdentityQuery(tx, &Model{Source: current.Source, SourceID: sourceID, Revision: current.Revision, ArtifactSubfolder: current.ArtifactSubfolder}).
			Where("id <> ?", id).Select("id").First(&taken).Error
		if err == nil {
			return &ModelSourceIDTakenError{ModelID: taken.ID}
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var active int64
		if err := tx.Model(&ModelFile{}).Where("model_id = ? AND state IN ?", id, modelTransferStates).Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return ErrModelTransferActive
		}
		if err := tx.Model(&Model{}).Where("id = ?", id).Update("source_id", sourceID).Error; err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if changed {
		s.Bus.PublishChange("model", uintKey(id), "source_id")
	}
	model, err = s.GetModel(id)
	return model, changed, err
}

// ModelArtifactsBySourceIDs returns the identity of every model registered
// under one of the repository ids (case-insensitive), newest first. Only the
// identity columns are read: callers on the deploy evaluation path match and
// rank artifacts, and the config.json and README text a row carries would
// otherwise be read on every keystroke. No ids yields no models, never all.
func (s *Store) ModelArtifactsBySourceIDs(sourceIDs ...string) ([]Model, error) {
	ids := make([]string, 0, len(sourceIDs))
	for _, id := range sourceIDs {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var ms []Model
	err := modelArtifactsBySourceIDsQuery(s.db, ids).Find(&ms).Error
	return ms, err
}

// modelArtifactsBySourceIDsQuery reads through idx_model_source_id_lower; the
// ids must already be case-folded.
func modelArtifactsBySourceIDsQuery(db *gorm.DB, ids []string) *gorm.DB {
	return db.Model(&Model{}).Select("id", "source", "source_id", "revision", "artifact_subfolder").
		Where("lower(source_id) IN ?", ids).Order("id desc")
}
