package store

import "time"

// InferenceModel is the flat catalog's original model row.  Executable
// configurations are stored in InferenceSpecRow; no compatibility/evidence
// entities are involved in this schema.
type InferenceModel struct {
	ID              uint   `gorm:"primaryKey"`
	ModelID         string `gorm:"size:255;not null;uniqueIndex"`
	Source          string `gorm:"size:16;not null;index"` // owner of canonical model metadata/readme
	DisplayTitle    string `gorm:"size:255"`
	ParameterCount  string `gorm:"size:64"`
	Architecture    string `gorm:"size:128"`
	InputModalities string `gorm:"column:input_modalities;type:text"` // JSON []string of declared input modalities
	// ModelConfig is the estimation-relevant subset of the model's config.json
	// (JSON modelconfig.Config, same field names). It lets KV/VRAM estimation run for a preset
	// model without fetching a repository that may be gated or offline.
	ModelConfig string `gorm:"column:model_config;type:text"`
	// KVBytesPerToken is the model's KV cost per token across the tensor-parallel
	// group, keyed by engine (JSON map[string]int64). It is the engine's own number rather than a
	// derivation from ModelConfig, and the engines differ for models with
	// recurrent layers. An absent engine key means the catalog carries no value
	// for it, which is not the same as a model that keeps no KV cache.
	KVBytesPerToken string `gorm:"column:kv_bytes_per_token;type:text"`
	ReadmeZHCN      string `gorm:"column:readme_zh_cn;size:255"`
	ReadmeEN        string `gorm:"column:readme_en;size:255"`
	PackageDigest   string `gorm:"size:64;index"`
	ContextLength   int64
	ReleaseDate     string `gorm:"size:10"`
	Unsupported     string `gorm:"type:text"` // JSON []inferencecatalog.UnsupportedGPU
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Specs           []InferenceSpecRow `gorm:"foreignKey:ModelID;constraint:OnDelete:CASCADE"`
}

func (InferenceModel) TableName() string { return "inference_models" }

// InferenceSpecRow is one complete, deployable model configuration. JSON fields
// intentionally remain opaque to SQL; model/spec identity and model metadata
// are the relational portions of this two-table catalog.
type InferenceSpecRow struct {
	ID                 uint   `gorm:"primaryKey"`
	ModelID            uint   `gorm:"not null;index;uniqueIndex:idx_inference_spec_identity,priority:1"`
	Source             string `gorm:"size:16;not null;default:upstream;index;uniqueIndex:idx_inference_spec_identity,priority:2"` // upstream | user; part of spec identity
	Name               string `gorm:"size:160;not null;uniqueIndex:idx_inference_spec_identity,priority:3"`
	PackageDigest      string `gorm:"size:64;index"`
	DisplayProfile     string `gorm:"size:16"`
	Scenario           string `gorm:"size:32;index"`
	Engine             string `gorm:"size:32;index"`
	Image              string `gorm:"size:512;not null"`
	ImageSources       string `gorm:"column:image_sources;type:text"` // JSON []string (inferencecatalog.InferenceSpec.ImageSources): further pull references for Image, ADR 0038; blank when none
	Quantization       string `gorm:"size:32;index"`
	WeightSources      string `gorm:"column:weight_sources;type:text;not null"` // JSON []inferencecatalog.WeightSource
	WeightBytes        int64
	RuntimeWeightBytes int64
	GPUs               string `gorm:"column:gpus;type:text;not null"`    // JSON inferencecatalog.GPUSelector
	Args               string `gorm:"column:args;type:text"`             // JSON []string
	Env                string `gorm:"column:env;type:text"`              // JSON map[string]string
	PreStartArgv       string `gorm:"column:pre_start_argv;type:text"`   // JSON []string
	PreStartScript     string `gorm:"column:pre_start_script;type:text"` // free text: shell script
	Assets             string `gorm:"column:assets;type:text"`           // JSON []inferencecatalog.Asset
	Entrypoint         string `gorm:"column:entrypoint;type:text"`       // JSON []string
	Placement          string `gorm:"column:placement;type:text"`        // JSON *inferencecatalog.Placement
	APIContract        string `gorm:"column:api_contract;type:text"`     // JSON *inferenceapi.Contract
	Components         string `gorm:"column:components;type:text"`       // JSON []inferencecatalog.Component
	// PDMode is the PD mode a PD-only spec is for; empty for every other spec.
	PDMode string `gorm:"column:pd_mode;size:32"`
	// ShmBytes is the /dev/shm size the spec's engine needs; zero states none.
	ShmBytes int64 `gorm:"column:shm_bytes"`
	// MemlockUnlimited starts the spec's container with unlimited locked memory.
	MemlockUnlimited bool `gorm:"column:memlock_unlimited"`
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (InferenceSpecRow) TableName() string { return "inference_specs" }
