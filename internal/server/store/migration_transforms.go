package store

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

func mergeDuplicateModels(db *gorm.DB) error {
	var models []Model
	if err := db.Order("id asc").Find(&models).Error; err != nil {
		return err
	}
	keepers := map[string]uint{}
	for _, model := range models {
		source := strings.ToLower(strings.TrimSpace(model.Source))
		sourceID := strings.TrimSpace(model.SourceID)
		revision := strings.TrimSpace(model.Revision)
		key := source + "\x00" + strings.ToLower(sourceID) + "\x00" + revision
		keeperID, duplicate := keepers[key]
		if !duplicate {
			keepers[key] = model.ID
			if err := db.Model(&Model{}).Where("id = ?", model.ID).Updates(map[string]any{"source": source, "source_id": sourceID, "revision": revision}).Error; err != nil {
				return err
			}
			continue
		}
		if db.Migrator().HasTable(&ModelFile{}) {
			var files []ModelFile
			if err := db.Where("model_id = ?", model.ID).Find(&files).Error; err != nil {
				return err
			}
			for _, file := range files {
				var count int64
				if err := db.Model(&ModelFile{}).Where("model_id = ? AND node_id = ?", keeperID, file.NodeID).Count(&count).Error; err != nil {
					return err
				}
				if count > 0 {
					if err := db.Delete(&ModelFile{}, file.ID).Error; err != nil {
						return err
					}
				} else if err := db.Model(&ModelFile{}).Where("id = ?", file.ID).Update("model_id", keeperID).Error; err != nil {
					return err
				}
			}
		}
		for _, table := range []any{&Deployment{}, &Profile{}} {
			if db.Migrator().HasTable(table) {
				if err := db.Model(table).Where("model_id = ?", model.ID).Update("model_id", keeperID).Error; err != nil {
					return err
				}
			}
		}
		if err := db.Delete(&Model{}, model.ID).Error; err != nil {
			return err
		}
	}
	return nil
}

// splitMixedFrameworkSeries makes the series framework authoritative without
// silently changing the meaning of an existing version.
func splitMixedFrameworkSeries(db *gorm.DB) error {
	var seriesRows []ImageSeries
	if err := db.Find(&seriesRows).Error; err != nil {
		return err
	}
	for _, series := range seriesRows {
		var versions []Image
		if err := db.Where("series_id = ?", series.ID).Find(&versions).Error; err != nil {
			return err
		}
		groups := map[string][]Image{}
		order := []string{}
		for _, version := range versions {
			framework := strings.TrimSpace(version.Framework)
			if framework == "" {
				framework = strings.TrimSpace(series.Framework)
			}
			framework = strings.ToLower(framework)
			if _, ok := groups[framework]; !ok {
				order = append(order, framework)
			}
			groups[framework] = append(groups[framework], version)
		}
		if len(order) <= 1 {
			if len(order) == 1 && strings.TrimSpace(series.Framework) == "" {
				if err := db.Model(&ImageSeries{}).Where("id = ?", series.ID).Update("framework", order[0]).Error; err != nil {
					return err
				}
			}
			continue
		}
		keep := order[0]
		for _, framework := range order {
			if strings.EqualFold(strings.TrimSpace(series.Framework), framework) {
				keep = framework
				break
			}
		}
		if err := db.Model(&ImageSeries{}).Where("id = ?", series.ID).Update("framework", keep).Error; err != nil {
			return err
		}
		for _, framework := range order {
			if framework == keep {
				continue
			}
			name := strings.TrimSpace(series.Name) + " · " + framework
			base := name
			for suffix := 2; ; suffix++ {
				var count int64
				if err := db.Model(&ImageSeries{}).Where("name = ? AND id <> ?", name, series.ID).Count(&count).Error; err != nil {
					return err
				}
				if count == 0 {
					break
				}
				name = fmt.Sprintf("%s %d", base, suffix)
			}
			clone := series
			clone.ID = 0
			clone.Name = name
			clone.Framework = framework
			if err := db.Create(&clone).Error; err != nil {
				return err
			}
			for _, version := range groups[framework] {
				if err := db.Model(&Image{}).Where("id = ?", version.ID).Updates(map[string]any{"series_id": clone.ID, "framework": framework}).Error; err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// builtinImageRefsV2 lists the refs the embedded catalog carried when the
// source column was introduced.
//
//go:embed migration_builtin_image_refs.txt
var builtinImageRefsV2 string

// normalizeBuiltinImageSources repairs databases that predate the source
// column and would otherwise classify embedded official images as custom.
func normalizeBuiltinImageSources(db *gorm.DB) error {
	for _, line := range strings.Split(builtinImageRefsV2, "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" || strings.HasPrefix(ref, "#") {
			continue
		}
		var image Image
		if err := db.Where("image_ref = ?", ref).First(&image).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				continue
			}
			return err
		}
		if image.Source == "custom" || strings.TrimSpace(image.Source) == "" {
			if err := db.Model(&image).Update("source", "official").Error; err != nil {
				return err
			}
		}
		if image.SeriesID != 0 {
			if err := db.Model(&ImageSeries{}).Where("id = ? AND (source = 'custom' OR source IS NULL OR trim(source) = '')", image.SeriesID).Update("source", "official").Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// migrateModelEndpointToNetworkRewrite folds the legacy model_endpoint setting
// into the network configuration's URL rewrite rules, which is the single place
// an operator now expresses "reach this upstream through that mirror". The
// legacy setting only ever applied to Hugging Face, so the generated rule is
// scoped to that host and existing rules for it are left alone: an operator who
// already wrote one has expressed the newer, more specific intent.
func migrateModelEndpointToNetworkRewrite(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Setting{}) {
		return nil
	}
	var setting Setting
	if err := db.Where("key = ?", "model_endpoint").First(&setting).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	endpoint := decodeSettingString(setting.Value)
	if endpoint == "" {
		// Nothing to carry over, but the obsolete key must still go away.
		return db.Where("key = ?", "model_endpoint").Delete(&Setting{}).Error
	}
	from, to, err := modelEndpointRewrite(endpoint)
	if err != nil {
		// An unusable endpoint must not block startup, and must not survive as
		// a setting that no code reads any more.
		return db.Where("key = ?", "model_endpoint").Delete(&Setting{}).Error
	}
	var cfg proto.NetworkConfig
	var current Setting
	if err := db.Where("key = ?", "network").First(&current).Error; err == nil {
		if err := json.Unmarshal([]byte(current.Value), &cfg); err != nil {
			cfg = proto.NetworkConfig{}
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	for _, existing := range cfg.Rewrites {
		if strings.EqualFold(strings.TrimSpace(existing.From), from) {
			return db.Where("key = ?", "model_endpoint").Delete(&Setting{}).Error
		}
	}
	cfg.Rewrites = append(cfg.Rewrites, proto.URLRewrite{From: from, To: to})
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := db.Save(&Setting{Key: "network", Value: string(encoded)}).Error; err != nil {
		return err
	}
	return db.Where("key = ?", "model_endpoint").Delete(&Setting{}).Error
}

// decodeSettingString reads a setting that may be stored either as a JSON
// string (the settings API) or as a bare string (older databases and the
// server's own startup wiring).
func decodeSettingString(raw string) string {
	raw = strings.TrimSpace(raw)
	var decoded string
	if json.Unmarshal([]byte(raw), &decoded) == nil {
		return strings.TrimSpace(decoded)
	}
	return strings.Trim(raw, `"`)
}

// modelEndpointRewrite converts the legacy Hugging Face endpoint override into
// the URL rewrite (from, to) the network setting stored then. Invalid or
// empty endpoints are rejected so the migration never creates an unusable
// rule.
func modelEndpointRewrite(endpoint string) (from, to string, err error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return "", "", fmt.Errorf("model endpoint is empty")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", fmt.Errorf("model endpoint must be an absolute URL")
	}
	return "https://huggingface.co", endpoint, nil
}
