package store

import (
	"errors"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

func addModelDownloadDeployment(db *gorm.DB) error {
	if !db.Migrator().HasTable(&ModelFile{}) {
		return nil
	}
	if !db.Migrator().HasColumn(&ModelFile{}, "DownloadDeploymentID") {
		if err := db.Migrator().AddColumn(&ModelFile{}, "DownloadDeploymentID"); err != nil {
			return err
		}
	}
	if !db.Migrator().HasIndex(&ModelFile{}, "DownloadDeploymentID") {
		return db.Migrator().CreateIndex(&ModelFile{}, "DownloadDeploymentID")
	}
	return nil
}

var ErrModelDownloadInactive = errors.New("the model file has no active public download")

// CancelModelDownload fences late status reports and withdraws the task without
// deleting resumable data. Keeping the writer lets reconnect deliver withdrawal.
func (s *Store) CancelModelDownload(id uint, transferID, message string, owner ...uint) (*ModelFile, error) {
	row, err := s.ModelFileByID(id)
	if err != nil {
		return nil, err
	}
	if row.State == proto.MFCanceled {
		return row, nil
	}
	if row.Acquisition != proto.ModelAcquireDownload || row.Ready() || row.State == proto.MFDeleting {
		return nil, ErrModelDownloadInactive
	}
	q := s.db.Model(&ModelFile{}).Where("id = ? AND transfer_id = ? AND acquisition = ? AND state NOT IN ?", id, transferID, proto.ModelAcquireDownload, []string{proto.MFPresent, proto.MFRegistered, proto.MFDeleting, proto.MFCanceled})
	if len(owner) > 0 {
		q = q.Where("download_deployment_id = ?", owner[0])
	}
	result := q.Updates(map[string]any{"state": proto.MFCanceled, "rate_bps": 0, "message": message})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrModelDownloadInactive
	}
	s.publishModelFile(id, proto.MFCanceled)
	return s.ModelFileByID(id)
}

// SetDownloadDeployment associates only the named transfer. A later explicit
// model-page download clears this owner and therefore survives service deletion.
func (s *Store) SetDownloadDeployment(id uint, transferID string, deploymentID uint, previousOwner ...uint) error {
	q := s.db.Model(&ModelFile{}).Where("id = ? AND transfer_id = ? AND acquisition IN ?", id, transferID, []string{proto.ModelAcquireDownload, proto.ModelAcquireAuto})
	if len(previousOwner) > 0 {
		q = q.Where("download_deployment_id = ?", previousOwner[0])
	}
	return q.Update("download_deployment_id", deploymentID).Error
}

func (s *Store) DownloadsForDeployment(deploymentID uint) ([]ModelFile, error) {
	var rows []ModelFile
	err := s.db.Where("download_deployment_id = ? AND acquisition = ? AND state NOT IN ?", deploymentID, proto.ModelAcquireDownload, []string{proto.MFPresent, proto.MFRegistered, proto.MFDeleting, proto.MFCanceled}).Find(&rows).Error
	return rows, err
}

// DownloadDeploymentIDs includes deleted owners so startup can finish a
// withdrawal interrupted between deleting the service and canceling its task.
func (s *Store) DownloadDeploymentIDs() ([]uint, error) {
	var ids []uint
	err := s.db.Model(&ModelFile{}).Distinct("download_deployment_id").Where("download_deployment_id <> 0 AND acquisition = ? AND state NOT IN ?", proto.ModelAcquireDownload, []string{proto.MFPresent, proto.MFRegistered, proto.MFDeleting, proto.MFCanceled}).Pluck("download_deployment_id", &ids).Error
	return ids, err
}
