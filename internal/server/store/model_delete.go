package store

import (
	"errors"
	"fmt"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ModelReference is a deployment that names a model: what keeps the model
// from being deleted, and what an operator needs to see to act on it.
type ModelReference struct {
	ID    uint
	Name  string
	State DeploymentState
	// Instances counts the instances that may still hold the model: every one
	// not stopped or failed.
	Instances int
	// CatalogSelectionSnapshot is the deployment's, for what a caller that
	// deletes it must release besides its rows (its managed directories).
	CatalogSelectionSnapshot string
}

// Idle reports whether the deployment is stopped and runs nothing, so
// deleting it together with its model loses no running service.
func (r ModelReference) Idle() bool { return r.State == DeployStopped && r.Instances == 0 }

// ModelReferenceError is a refused model deletion: the deployments that name
// the model. errors.Is(err, ErrReferenced) holds.
type ModelReferenceError struct {
	Deployments []ModelReference
}

func (e *ModelReferenceError) Error() string {
	return fmt.Sprintf("%v: model is used by %d deployment(s)", ErrReferenced, len(e.Deployments))
}

func (e *ModelReferenceError) Unwrap() error { return ErrReferenced }

// AllIdle reports whether every deployment that names the model is stopped
// and runs nothing (ModelReference.Idle).
func (e *ModelReferenceError) AllIdle() bool {
	for _, ref := range e.Deployments {
		if !ref.Idle() {
			return false
		}
	}
	return len(e.Deployments) > 0
}

// modelReferences lists the deployments that name model id in tx, with the
// instances of each that are not stopped or failed: two queries, however many
// deployments there are.
func modelReferences(tx *gorm.DB, id uint) ([]ModelReference, error) {
	var deps []Deployment
	if err := tx.Select("ID", "Name", "State", "CatalogSelectionSnapshot").Where("model_id = ?", id).Order("id").Find(&deps).Error; err != nil {
		return nil, err
	}
	if len(deps) == 0 {
		return nil, nil
	}
	ids := make([]uint, len(deps))
	for i, dep := range deps {
		ids[i] = dep.ID
	}
	var counts []struct {
		DeploymentID uint
		N            int
	}
	if err := tx.Model(&Instance{}).Select("deployment_id, COUNT(*) AS n").
		Where("deployment_id IN ? AND state NOT IN ?", ids, []string{proto.InstStopped, proto.InstFailed}).
		Group("deployment_id").Scan(&counts).Error; err != nil {
		return nil, err
	}
	live := make(map[uint]int, len(counts))
	for _, c := range counts {
		live[c.DeploymentID] = c.N
	}
	out := make([]ModelReference, len(deps))
	for i, dep := range deps {
		out[i] = ModelReference{ID: dep.ID, Name: dep.Name, State: dep.State, Instances: live[dep.ID], CatalogSelectionSnapshot: dep.CatalogSelectionSnapshot}
	}
	return out, nil
}

// DeleteModel removes an unused model and its derived node/profile rows. A
// deployment keeps the model definition authoritative, even after it stops:
// a model any deployment names is refused with a *ModelReferenceError
// listing them. With withIdleDeployments the deployments go too, in the same
// transaction, when every one of them is stopped and runs nothing; a running
// one still refuses the whole deletion (GWKS-530). It returns the deployments
// it deleted.
func (s *Store) DeleteModel(id uint, withIdleDeployments bool) ([]ModelReference, error) {
	var gone map[uint]deletedDeployment
	var deleted []ModelReference
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var model Model
		if err := tx.First(&model, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		refs, err := modelReferences(tx, id)
		if err != nil {
			return err
		}
		if len(refs) > 0 {
			refused := &ModelReferenceError{Deployments: refs}
			if !withIdleDeployments || !refused.AllIdle() {
				return refused
			}
			gone, deleted = make(map[uint]deletedDeployment, len(refs)), refs
			for _, ref := range refs {
				if gone[ref.ID], err = deleteDeploymentTx(tx, ref.ID); err != nil {
					return err
				}
			}
		}
		if err := checkNoCacheDeletion(tx.Where("model_id = ?", id)); err != nil {
			return err
		}
		if err := tx.Where("model_id = ?", id).Delete(&ModelFile{}).Error; err != nil {
			return err
		}
		if err := tx.Where("model_id = ?", id).Delete(&Profile{}).Error; err != nil {
			return err
		}
		result := tx.Delete(&Model{}, id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for depID, rows := range gone {
		s.publishDeletedDeployment(depID, rows)
	}
	s.Bus.PublishChange("model", uintKey(id), "deleted")
	return deleted, nil
}
