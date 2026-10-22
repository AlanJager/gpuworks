package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// prepareInferenceTemplateClone materializes model-local files under the new
// model identity. A clone must not keep the source package digest: assets are
// addressed by both digest and model_id, so doing so would create a catalog
// row that previews correctly but fails only when the controller reads assets.
func (a *apiHandlers) prepareInferenceTemplateClone(source store.InferenceModel, clone *store.InferenceModel) (string, bool, error) {
	if clone == nil {
		return "", false, errors.New("inference template clone is nil")
	}
	files := map[string][]byte{}
	for _, file := range []string{source.ReadmeZHCN, source.ReadmeEN} {
		if strings.TrimSpace(file) == "" {
			continue
		}
		data, err := a.readInferenceTemplateCloneFile(source, file, true)
		if err != nil {
			return "", false, err
		}
		files[path.Join("models", clone.ModelID, file)] = data
	}
	seenAssets := map[string]struct{}{}
	for _, spec := range source.Specs {
		var assets []inferencecatalog.Asset
		if strings.TrimSpace(spec.Assets) != "" {
			if err := decodeProjectionJSON(spec.Assets, &assets); err != nil {
				return "", false, fmt.Errorf("decode source template %s/%s assets: %w", source.ModelID, spec.Name, err)
			}
		}
		for _, asset := range assets {
			if _, duplicate := seenAssets[asset.File]; duplicate {
				data, err := a.readInferenceTemplateSpecAsset(source, spec, asset.File)
				if err != nil {
					return "", false, err
				}
				name := path.Join("models", clone.ModelID, asset.File)
				if !bytes.Equal(files[name], data) {
					return "", false, fmt.Errorf("cannot clone %s: catalog sources use different content at %s; give each user spec a unique asset path", source.ModelID, asset.File)
				}
				continue
			}
			seenAssets[asset.File] = struct{}{}
			data, err := a.readInferenceTemplateSpecAsset(source, spec, asset.File)
			if err != nil {
				return "", false, err
			}
			files[path.Join("models", clone.ModelID, asset.File)] = data
		}
	}
	if len(files) == 0 {
		clone.PackageDigest = ""
		for index := range clone.Specs {
			clone.Specs[index].PackageDigest = ""
		}
		return "", false, nil
	}
	if a.d.CatalogPackages == nil {
		return "", false, errors.New("catalog package storage is required to clone template documentation or assets")
	}
	model, err := inferencecatalog.FromFlatStoreModel(*clone)
	if err != nil {
		return "", false, err
	}
	packageCatalog, err := a.withComponentModels(inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{model}})
	if err != nil {
		return "", false, err
	}
	packageFS, err := inferencecatalog.BuildPackageFS(time.Now().UTC().Format(time.RFC3339), packageCatalog, files)
	if err != nil {
		return "", false, fmt.Errorf("assemble cloned inference catalog package: %w", err)
	}
	digest, created, err := a.d.CatalogPackages.Put(packageFS)
	if err != nil {
		return "", false, fmt.Errorf("persist cloned inference catalog package: %w", err)
	}
	clone.PackageDigest = digest
	for index := range clone.Specs {
		clone.Specs[index].PackageDigest = digest
	}
	return digest, created, nil
}

func (a *apiHandlers) readInferenceTemplateSpecAsset(source store.InferenceModel, spec store.InferenceSpecRow, file string) ([]byte, error) {
	packageDigest := strings.TrimSpace(spec.PackageDigest)
	if packageDigest == "" {
		packageDigest = strings.TrimSpace(source.PackageDigest)
	}
	var data []byte
	var err error
	if a.d.CatalogPackages == nil || packageDigest == "" {
		return nil, errors.New("catalog package storage is unavailable")
	}
	data, err = a.d.CatalogPackages.ReadAsset(packageDigest, source.ModelID, file)
	if err != nil {
		return nil, fmt.Errorf("read source template %s %s asset %s: %w", source.ModelID, spec.Source, file, err)
	}
	if data == nil {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

func catalogRequiresLocalFiles(catalog inferencecatalog.FlatCatalog) bool {
	for _, model := range catalog.Models {
		if model.ReadmeZHCN != "" || model.ReadmeEN != "" {
			return true
		}
		for _, spec := range model.Specs {
			if len(spec.Assets) > 0 {
				return true
			}
		}
	}
	return false
}

// materializeEditedInferenceCatalogPackage makes YAML editing lossless for
// existing packaged user specs. YAML may change structured facts, but every
// referenced file must already exist in that exact user spec's current
// content-addressed package and is revalidated against the edited SHA fields.
func (a *apiHandlers) materializeEditedInferenceCatalogPackage(catalog inferencecatalog.FlatCatalog) (string, bool, error) {
	if a.d.CatalogPackages == nil {
		return "", false, errors.New("catalog package storage is required to edit template documentation or assets")
	}
	packageCatalog := inferencecatalog.FlatCatalog{Models: make([]inferencecatalog.InferenceModel, 0, len(catalog.Models))}
	files := map[string][]byte{}
	for _, incoming := range catalog.Models {
		existing, err := a.d.Store.InferenceModelByModelID(incoming.ModelID)
		if err != nil {
			return "", false, fmt.Errorf("model %s has local files but no existing packaged template: %w", incoming.ModelID, err)
		}
		incomingNames := make(map[string]struct{}, len(incoming.Specs))
		for _, spec := range incoming.Specs {
			incomingNames[strings.TrimSpace(spec.Name)] = struct{}{}
		}
		existingUserSpecs := make(map[string]store.InferenceSpecRow)
		userProjection := *existing
		userProjection.Source = "user"
		userProjection.Specs = nil
		for _, spec := range existing.Specs {
			if spec.Source != "user" {
				continue
			}
			existingUserSpecs[spec.Name] = spec
			if _, replaced := incomingNames[spec.Name]; !replaced {
				userProjection.Specs = append(userProjection.Specs, spec)
			}
		}
		projected, err := inferencecatalog.FromFlatStoreModel(userProjection)
		if err != nil {
			return "", false, err
		}
		incoming.Source = "user"
		incoming.Specs = append(projected.Specs, incoming.Specs...)
		if existing.Source == "upstream" {
			// Canonical model metadata and documentation remain owned by the
			// embedded package. This package contains only its user specs.
			incoming.ReadmeZHCN = ""
			incoming.ReadmeEN = ""
		}
		packageCatalog.Models = append(packageCatalog.Models, incoming)

		for _, file := range []string{incoming.ReadmeZHCN, incoming.ReadmeEN} {
			if file == "" {
				continue
			}
			data, err := a.readInferenceTemplateCloneFile(*existing, file, true)
			if err != nil {
				return "", false, err
			}
			files[path.Join("models", incoming.ModelID, file)] = data
		}
		for _, spec := range incoming.Specs {
			stored, ok := existingUserSpecs[spec.Name]
			for _, asset := range spec.Assets {
				name := path.Join("models", incoming.ModelID, asset.File)
				if !ok {
					return "", false, fmt.Errorf("model %s spec %s asset %s is not present in an existing user package; import a .gpuworks-catalog.tar.gz package", incoming.ModelID, spec.Name, asset.File)
				}
				data, err := a.readInferenceTemplateSpecAsset(*existing, stored, asset.File)
				if err != nil {
					return "", false, err
				}
				if previous, copied := files[name]; copied && !bytes.Equal(previous, data) {
					return "", false, fmt.Errorf("model %s user specs use different content at %s; give each spec a unique asset path", incoming.ModelID, asset.File)
				}
				files[name] = data
			}
		}
	}
	packageCatalog, err := a.withComponentModels(packageCatalog)
	if err != nil {
		return "", false, err
	}
	packageFS, err := inferencecatalog.BuildPackageFS(time.Now().UTC().Format(time.RFC3339), packageCatalog, files)
	if err != nil {
		return "", false, fmt.Errorf("assemble edited inference catalog package: %w", err)
	}
	digest, created, err := a.d.CatalogPackages.Put(packageFS)
	if err != nil {
		return "", false, fmt.Errorf("persist edited inference catalog package: %w", err)
	}
	return digest, created, nil
}

func decodeProjectionJSON(raw string, output any) error {
	return json.Unmarshal([]byte(raw), output)
}

func (a *apiHandlers) readInferenceTemplateCloneFile(source store.InferenceModel, file string, documentation bool) ([]byte, error) {
	var data []byte
	var err error
	if a.d.CatalogPackages == nil || source.PackageDigest == "" {
		return nil, errors.New("catalog package storage is unavailable")
	}
	if documentation {
		data, err = a.d.CatalogPackages.ReadDocumentation(source.PackageDigest, source.ModelID, file)
	} else {
		data, err = a.d.CatalogPackages.ReadAsset(source.PackageDigest, source.ModelID, file)
	}
	if err != nil {
		kind := "asset"
		if documentation {
			kind = "documentation"
		}
		return nil, fmt.Errorf("read source template %s %s %s: %w", source.ModelID, kind, file, err)
	}
	if data == nil {
		return nil, fs.ErrNotExist
	}
	return data, nil
}
