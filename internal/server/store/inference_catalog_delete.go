package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

var ErrInferenceTemplateMixedSources = errors.New("template contains non-user specs")

// DeleteUserInferenceSpec retires one user-authored spec. Imports are additive
// (specs absent from an import are preserved), so an explicit delete is the
// only way to remove a user spec. Deleting the last remaining spec of a user
// model drops the model row; upstream rows always keep their canonical specs
// and never reach that branch. Deletion refuses while a deployment outside
// "deleting" still pins the spec, or while another model mounts a
// to-be-dropped model as a component.
func (s *Store) DeleteUserInferenceSpec(modelID, specName string) (modelDeleted bool, err error) {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	specName = strings.TrimSpace(specName)
	if modelID == "" || specName == "" {
		return false, errors.New("model_id and spec are required")
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var model InferenceModel
		if err := tx.Preload("Specs").Where("model_id = ?", modelID).First(&model).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		target := -1
		for i := range model.Specs {
			if model.Specs[i].Source == "user" && model.Specs[i].Name == specName {
				target = i
				break
			}
		}
		if target < 0 {
			return ErrInferenceSpecNotFound
		}
		if err := refusePinnedInferenceTemplate(tx, model.ID, specName); err != nil {
			return err
		}
		if err := tx.Delete(&InferenceSpecRow{}, model.Specs[target].ID).Error; err != nil {
			return err
		}
		if len(model.Specs) > 1 {
			return nil
		}
		if model.Source != "user" {
			return fmt.Errorf("model %s would be left without specs", model.ModelID)
		}
		if err := refuseDanglingComponentReference(tx, model.ID, model.ModelID); err != nil {
			return err
		}
		modelDeleted = true
		return tx.Delete(&InferenceModel{}, model.ID).Error
	})
	return modelDeleted, err
}

// DeleteUserInferenceTemplate removes a custom model and its specs atomically.
func (s *Store) DeleteUserInferenceTemplate(modelID string) error {
	var model InferenceModel
	err := s.db.Transaction(func(tx *gorm.DB) error {
		err := tx.Where("model_id = ? AND source = ?", strings.ToLower(strings.TrimSpace(modelID)), "user").First(&model).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := refusePinnedInferenceTemplate(tx, model.ID, ""); err != nil {
			return err
		}
		if err := refuseDanglingComponentReference(tx, model.ID, model.ModelID); err != nil {
			return err
		}
		if err := tx.Where("model_id = ? AND source = ?", model.ID, "user").Delete(&InferenceSpecRow{}).Error; err != nil {
			return err
		}
		var remaining int64
		if err := tx.Model(&InferenceSpecRow{}).Where("model_id = ?", model.ID).Count(&remaining).Error; err != nil {
			return err
		}
		if remaining != 0 {
			return messages.WrapProductError(messages.ErrorInferenceTemplateMixedSources,
				fmt.Sprintf("template %s contains non-user specs and cannot be deleted as a whole", model.ModelID),
				map[string]any{"model": model.ModelID}, ErrInferenceTemplateMixedSources)
		}
		return tx.Delete(&model).Error
	})
	if err == nil {
		s.Bus.PublishChange(InferenceTemplateTopic, strconv.FormatUint(uint64(model.ID), 10), "")
	}
	return err
}

// An empty specName checks every deployment that pins the template.
func refusePinnedInferenceTemplate(query *gorm.DB, modelID uint, specName string) error {
	if specName != "" {
		query = query.Where("template_spec_source = ? AND template_spec_name = ?", "user", specName)
	}
	var names []string
	if err := query.Model(&Deployment{}).Where("template_id = ? AND desired_state <> ?", modelID, string(DeployDeleting)).
		Order("name").Pluck("name", &names).Error; err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	deployments := strings.Join(names, ", ")
	if specName != "" {
		return messages.WrapProductError(messages.ErrorInferenceSpecDeploymentReference,
			fmt.Sprintf("deployments %s still use spec %s", deployments, specName),
			map[string]any{"deployments": deployments, "spec": specName}, ErrInferenceSpecInUse)
	}
	return messages.WrapProductError(messages.ErrorInferenceTemplateDeploymentReference,
		fmt.Sprintf("deployments %s still use this template", deployments),
		map[string]any{"deployments": deployments}, ErrInferenceSpecInUse)
}

// refuseDanglingComponentReference mirrors
// inferencecatalog.validateComponentReferences for the delete path: a model
// row may not be dropped while any other spec still mounts it as a component.
func refuseDanglingComponentReference(tx *gorm.DB, modelRowID uint, modelID string) error {
	var rows []InferenceSpecRow
	if err := tx.Where("components <> '' AND components <> '[]' AND components <> 'null'").Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	var owners []InferenceModel
	if err := tx.Find(&owners).Error; err != nil {
		return err
	}
	ownerIDs := make(map[uint]string, len(owners))
	for _, owner := range owners {
		ownerIDs[owner.ID] = owner.ModelID
	}
	type componentReference struct {
		ModelID string `json:"model_id"`
	}
	for _, row := range rows {
		if row.ModelID == modelRowID {
			continue
		}
		var components []componentReference
		if err := json.Unmarshal([]byte(row.Components), &components); err != nil || len(components) == 0 {
			continue
		}
		for _, component := range components {
			if strings.EqualFold(strings.TrimSpace(component.ModelID), modelID) {
				return messages.WrapProductError(messages.ErrorInferenceTemplateComponentReference,
					fmt.Sprintf("%s: %s spec %s mounts %s", ErrInferenceModelReferenced, ownerIDs[row.ModelID], row.Name, modelID),
					map[string]any{"owner": ownerIDs[row.ModelID], "spec": row.Name}, ErrInferenceModelReferenced)
			}
		}
	}
	return nil
}
